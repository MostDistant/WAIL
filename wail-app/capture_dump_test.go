//go:build !linkstub

package main

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

const dumpTestRate = 48000

// sineWindow returns one interleaved int16 window of a continuous sine, advancing
// phase so successive windows join without a click.
func sineWindow(spf, channels int, freq float64, phase *float64) []int16 {
	s := make([]int16, spf*channels)
	for i := 0; i < spf; i++ {
		v := int16(8000 * math.Sin(*phase))
		for c := 0; c < channels; c++ {
			s[i*channels+c] = v
		}
		*phase += 2 * math.Pi * freq / float64(dumpTestRate)
	}
	return s
}

// parseWav reads a canonical 44-byte-header PCM WAV and returns its format + samples.
func parseWav(t *testing.T, path string) (rate, channels, bits int, samples []int16) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[36:40]) != "data" {
		t.Fatalf("%s: not a canonical RIFF/WAVE/data file", path)
	}
	channels = int(binary.LittleEndian.Uint16(b[22:24]))
	rate = int(binary.LittleEndian.Uint32(b[24:28]))
	bits = int(binary.LittleEndian.Uint16(b[34:36]))
	riffSize := binary.LittleEndian.Uint32(b[4:8])
	dataSize := binary.LittleEndian.Uint32(b[40:44])
	if int(riffSize) != 36+int(dataSize) {
		t.Fatalf("%s: RIFF size %d != 36+data %d (header not patched)", path, riffSize, dataSize)
	}
	if int(dataSize) != len(b)-44 {
		t.Fatalf("%s: data size %d != file body %d", path, dataSize, len(b)-44)
	}
	n := int(dataSize) / 2
	samples = make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(b[44+i*2:]))
	}
	return
}

// TestCaptureDumpWritesAlignedPrePost drives a captureDump exactly as emitWindow
// does — feeding (pre-encode PCM, wire frame) pairs — and asserts the pre WAV is
// bit-exact to the input, the post WAV equals an independent reference decode of
// the same wires (validating the receiver mirror), and the two are sample-aligned.
func TestCaptureDumpWritesAlignedPrePost(t *testing.T) {
	dir := t.TempDir()
	const channels = 2
	spf := samplesPerWaifFrame(dumpTestRate) // 960

	enc, err := NewIntervalEncoder(channels, dumpTestRate, 128)
	if err != nil {
		t.Fatal(err)
	}
	refDec, err := NewIntervalDecoder(channels, dumpTestRate)
	if err != nil {
		t.Fatal(err)
	}

	dump, err := newCaptureDump(dir, "chanA_stream0", channels, dumpTestRate)
	if err != nil {
		t.Fatal(err)
	}

	const N = 12
	var pre, refPost []int16
	var seq uint32 = 100
	phase := 0.0
	for i := 0; i < N; i++ {
		w := sineWindow(spf, channels, 440, &phase)
		wire, err := enc.EncodeWindow(w, WindowMeta{
			RoomIndex: 5, StreamID: 0, FrameNumber: uint32(i), Seq: seq,
			IsFinal: i == N-1, TotalFrames: N, BPM: 120, Quantum: 4, Bars: 4,
		})
		if err != nil {
			t.Fatal(err)
		}
		seq++
		dump.writePair(w, wire)
		pre = append(pre, w...)

		f, err := DecodeAudioFrameWire(wire)
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := refDec.DecodeFrame(f.OpusData)
		if err != nil {
			t.Fatal(err)
		}
		refPost = append(refPost, pcm...)
	}
	dump.Close()

	rate, ch, bits, preSamples := parseWav(t, filepath.Join(dir, "chanA_stream0_preopus.wav"))
	if rate != dumpTestRate || ch != channels || bits != 16 {
		t.Fatalf("pre header rate=%d ch=%d bits=%d, want %d/%d/16", rate, ch, bits, dumpTestRate, channels)
	}
	if len(preSamples) != len(pre) {
		t.Fatalf("pre samples %d != input %d", len(preSamples), len(pre))
	}
	for i := range pre {
		if preSamples[i] != pre[i] {
			t.Fatalf("pre sample %d: got %d want %d (not bit-exact)", i, preSamples[i], pre[i])
		}
	}

	_, _, _, postSamples := parseWav(t, filepath.Join(dir, "chanA_stream0_postopus.wav"))
	if len(postSamples) != len(refPost) {
		t.Fatalf("post samples %d != reference decode %d", len(postSamples), len(refPost))
	}
	for i := range refPost {
		if postSamples[i] != refPost[i] {
			t.Fatalf("post sample %d: got %d want %d (mirror decode mismatch)", i, postSamples[i], refPost[i])
		}
	}
	if len(postSamples) != len(preSamples) {
		t.Fatalf("post len %d != pre len %d (not sample-aligned)", len(postSamples), len(preSamples))
	}
	var energy int64
	for _, s := range postSamples {
		energy += int64(s) * int64(s)
	}
	if energy == 0 {
		t.Fatal("post WAV is silent for non-silent input")
	}
}

func TestWavStreamWriterHeaderPatched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h.wav")
	w, err := newWavStreamWriter(path, 2, dumpTestRate)
	if err != nil {
		t.Fatal(err)
	}
	const M = 500 // int16 samples
	if err := w.write(make([]int16, M)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	rate, ch, bits, samples := parseWav(t, path)
	if rate != dumpTestRate || ch != 2 || bits != 16 {
		t.Fatalf("header rate=%d ch=%d bits=%d", rate, ch, bits)
	}
	if len(samples) != M {
		t.Fatalf("samples %d != %d", len(samples), M)
	}
}
