package main

import (
	"log"

	"gopkg.in/hraban/opus.v2"
)

// Interval Opus codec: turns a full interval of PCM into a sequence of 20ms WAIF
// frames and back. This is the encode/decode that used to live in the Rust
// plugins; on the Link Audio path it runs in the Go app (capture encodes,
// playback decodes). Pure in-process (libopus via cgo) — no Link hardware — so
// the whole PCM→WAIF→PCM pipeline is loopback-testable.

const (
	waifFrameMs = 20 // WAIF frames are 20ms Opus packets (CONTEXT.md glossary)
)

// samplesPerWaifFrame returns the per-channel sample count of one 20ms frame.
func samplesPerWaifFrame(sampleRate int) int { return sampleRate * waifFrameMs / 1000 }

// IntervalEncoder Opus-encodes interval PCM into WAIF frames for one stream.
type IntervalEncoder struct {
	enc             *opus.Encoder
	channels        int
	sampleRate      int
	samplesPerFrame int // per channel
	opusBuf         []byte
	scratch         []int16 // reused padded frame buffer
}

// NewIntervalEncoder creates an encoder for the given channel count and rate.
func NewIntervalEncoder(channels, sampleRate, bitrateKbps int) (*IntervalEncoder, error) {
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppAudio)
	if err != nil {
		return nil, err
	}
	if bitrateKbps > 0 {
		if err := enc.SetBitrate(bitrateKbps * 1000); err != nil {
			log.Printf("[audio] warn: set Opus bitrate %dkbps failed, using default: %v", bitrateKbps, err)
		}
	}
	spf := samplesPerWaifFrame(sampleRate)
	return &IntervalEncoder{
		enc:             enc,
		channels:        channels,
		sampleRate:      sampleRate,
		samplesPerFrame: spf,
		opusBuf:         make([]byte, 4096),
		scratch:         make([]int16, spf*channels),
	}, nil
}

// EncodeInterval splits interleaved interval PCM into 20ms Opus WAIF frames
// labeled with the shared room interval index. seqStart is the running per-stream
// frame sequence (for WAN loss detection); the returned nextSeq continues it. The
// final frame carries the interval trailer (rate, total frames, bpm, quantum,
// bars). A short trailing chunk is zero-padded to a full Opus frame.
func (e *IntervalEncoder) EncodeInterval(pcm []int16, roomIndex int64, streamID uint16, seqStart uint32, bpm, quantum float64, bars uint32) ([][]byte, uint32, error) {
	frameLen := e.samplesPerFrame * e.channels
	if frameLen == 0 {
		return nil, seqStart, nil
	}
	numFrames := (len(pcm) + frameLen - 1) / frameLen
	if numFrames == 0 {
		numFrames = 1 // always emit at least a final frame so receivers learn the total
	}

	frames := make([][]byte, 0, numFrames)
	seq := seqStart
	for i := 0; i < numFrames; i++ {
		start := i * frameLen
		var chunk []int16
		if start >= len(pcm) {
			chunk = e.zeroScratch()
		} else if end := start + frameLen; end <= len(pcm) {
			chunk = pcm[start:end]
		} else {
			// Short trailing chunk: pad with silence to a full Opus frame.
			chunk = e.zeroScratch()
			copy(chunk, pcm[start:])
		}

		n, err := e.enc.Encode(chunk, e.opusBuf)
		if err != nil {
			return frames, seq, err
		}
		opusData := make([]byte, n)
		copy(opusData, e.opusBuf[:n])

		isFinal := i == numFrames-1
		f := &AudioFrame{
			IntervalIndex: roomIndex,
			StreamID:      streamID,
			FrameNumber:   uint32(i),
			FrameSeq:      seq,
			Channels:      uint16(e.channels),
			OpusData:      opusData,
			IsFinal:       isFinal,
		}
		if isFinal {
			f.SampleRate = uint32(e.sampleRate)
			f.TotalFrames = uint32(numFrames)
			f.BPM = bpm
			f.Quantum = quantum
			f.Bars = bars
		}
		frames = append(frames, EncodeAudioFrameWire(f))
		seq++
	}
	return frames, seq, nil
}

func (e *IntervalEncoder) zeroScratch() []int16 {
	for i := range e.scratch {
		e.scratch[i] = 0
	}
	return e.scratch
}

// IntervalDecoder Opus-decodes WAIF frames back to PCM for one stream.
type IntervalDecoder struct {
	dec             *opus.Decoder
	channels        int
	samplesPerFrame int
	out             []int16
}

// NewIntervalDecoder creates a decoder for the given channels and rate.
func NewIntervalDecoder(channels, sampleRate int) (*IntervalDecoder, error) {
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		return nil, err
	}
	spf := samplesPerWaifFrame(sampleRate)
	return &IntervalDecoder{
		dec:             dec,
		channels:        channels,
		samplesPerFrame: spf,
		out:             make([]int16, spf*channels),
	}, nil
}

// DecodeFrame decodes one WAIF frame's Opus payload to interleaved PCM. The
// returned slice is freshly allocated (safe to retain).
func (d *IntervalDecoder) DecodeFrame(opusData []byte) ([]int16, error) {
	n, err := d.dec.Decode(opusData, d.out)
	if err != nil {
		return nil, err
	}
	pcm := make([]int16, n*d.channels)
	copy(pcm, d.out[:n*d.channels])
	return pcm, nil
}
