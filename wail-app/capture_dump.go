//go:build !linkstub

package main

import (
	"encoding/binary"
	"log"
	"os"
	"path/filepath"
)

// capture_dump.go is a debug instrument: for one capture channel it writes two
// WAV files — the interleaved int16 PCM handed to the Opus encoder ("pre"), and
// that audio decoded back exactly as a remote receiver would ("post"). A/B-ing
// the two localizes choppiness: pre bad → capture/LAN; pre clean but post bad →
// the codec; both clean → the fault is downstream (network/reassembly/receiver).

// wavStreamWriter appends interleaved int16 PCM to a canonical PCM WAV, patching
// the RIFF and data sizes on Close (they aren't known until the stream ends).
type wavStreamWriter struct {
	f       *os.File
	samples int64 // total int16 samples written (all channels)
}

func newWavStreamWriter(path string, channels, rate int) (*wavStreamWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	// 44-byte canonical header; RIFF/data sizes are placeholders, patched on Close.
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(hdr[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(hdr[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(rate*channels*2)) // byte rate
	binary.LittleEndian.PutUint16(hdr[32:34], uint16(channels*2))      // block align
	binary.LittleEndian.PutUint16(hdr[34:36], 16)                      // bits/sample
	copy(hdr[36:40], "data")
	if _, err := f.Write(hdr); err != nil {
		f.Close()
		return nil, err
	}
	return &wavStreamWriter{f: f}, nil
}

func (w *wavStreamWriter) write(samples []int16) error {
	if len(samples) == 0 {
		return nil
	}
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	if _, err := w.f.Write(buf); err != nil {
		return err
	}
	w.samples += int64(len(samples))
	return nil
}

func (w *wavStreamWriter) Close() error {
	dataBytes := w.samples * 2
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(36+dataBytes))
	if _, err := w.f.WriteAt(b[:], 4); err != nil {
		w.f.Close()
		return err
	}
	binary.LittleEndian.PutUint32(b[:], uint32(dataBytes))
	if _, err := w.f.WriteAt(b[:], 40); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// captureDump holds the pre/post WAV writers for one channel plus a stateful
// mirror decoder. The decoder is fed every frame in order (Opus is stateful),
// exactly like the receiver's per-stream decoder, so the post WAV is a faithful
// loss-free reconstruction of what a remote peer hears.
type captureDump struct {
	pre  *wavStreamWriter
	post *wavStreamWriter
	dec  *IntervalDecoder
}

// newCaptureDump opens <dir>/<name>_preopus.wav and <dir>/<name>_postopus.wav.
// channels/rate must match the encoder that produced the wire frames (the
// capture path is fixed stereo @ engineInternalRate).
func newCaptureDump(dir, name string, channels, rate int) (*captureDump, error) {
	dec, err := NewIntervalDecoder(channels, rate)
	if err != nil {
		return nil, err
	}
	pre, err := newWavStreamWriter(filepath.Join(dir, name+"_preopus.wav"), channels, rate)
	if err != nil {
		return nil, err
	}
	post, err := newWavStreamWriter(filepath.Join(dir, name+"_postopus.wav"), channels, rate)
	if err != nil {
		pre.Close()
		return nil, err
	}
	return &captureDump{pre: pre, post: post, dec: dec}, nil
}

// writePair records one window: the pre-encode PCM and the receiver-decode of
// the wire frame it produced. Best-effort — a failure logs and never disrupts
// the capture path. Called only on the channel's drain goroutine.
func (d *captureDump) writePair(samples []int16, wire []byte) {
	if err := d.pre.write(samples); err != nil {
		log.Printf("[audio] capture dump: pre write failed: %v", err)
	}
	f, err := DecodeAudioFrameWire(wire)
	if err != nil {
		log.Printf("[audio] capture dump: wire decode failed: %v", err)
		return
	}
	pcm, err := d.dec.DecodeFrame(f.OpusData)
	if err != nil {
		log.Printf("[audio] capture dump: opus decode failed: %v", err)
		return
	}
	// DecodeFrame's slice aliases the decoder's buffer — copy before writing.
	cp := make([]int16, len(pcm))
	copy(cp, pcm)
	if err := d.post.write(cp); err != nil {
		log.Printf("[audio] capture dump: post write failed: %v", err)
	}
}

func (d *captureDump) Close() {
	if d.pre != nil {
		d.pre.Close()
	}
	if d.post != nil {
		d.post.Close()
	}
}
