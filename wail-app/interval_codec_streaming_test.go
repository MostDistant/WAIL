package main

import (
	"bytes"
	"math"
	"testing"
)

// TestEncodeWindowMatchesEncodeInterval feeds the same interval PCM through
// EncodeInterval (batch) and a window-at-a-time EncodeWindow sequence with
// matching metadata; the WAIF frames must be byte-identical, proving the
// streaming path changes delivery timing only, never the bitstream.
func TestEncodeWindowMatchesEncodeInterval(t *testing.T) {
	const (
		channels = 2
		sr       = 48000
		bpm      = 120.0
		quantum  = 4.0
		bars     = uint32(4)
		roomIdx  = int64(7)
		streamID = uint16(3)
		seq0     = uint32(1000)
	)
	spf := samplesPerWaifFrame(sr)

	// 5.5 windows of a sine so the final window exercises padding.
	pcm := make([]int16, spf*channels*5+spf*channels/2)
	for i := 0; i < len(pcm); i += channels {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i/channels)/float64(sr)))
		for c := 0; c < channels; c++ {
			pcm[i+c] = v
		}
	}

	batchEnc, err := NewIntervalEncoder(channels, sr, 128)
	if err != nil {
		t.Fatal(err)
	}
	streamEnc, err := NewIntervalEncoder(channels, sr, 128)
	if err != nil {
		t.Fatal(err)
	}

	batch, nextSeq, err := batchEnc.EncodeInterval(pcm, roomIdx, streamID, seq0, bpm, quantum, bars)
	if err != nil {
		t.Fatal(err)
	}
	total := uint32(len(batch))

	var streamed [][]byte
	seq := seq0
	frameLen := spf * channels
	for i := uint32(0); i < total; i++ {
		start := int(i) * frameLen
		end := start + frameLen
		var chunk []int16
		if end <= len(pcm) {
			chunk = pcm[start:end]
		} else {
			chunk = pcm[start:] // short final chunk: EncodeWindow must pad
		}
		f, err := streamEnc.EncodeWindow(chunk, WindowMeta{
			RoomIndex:   roomIdx,
			StreamID:    streamID,
			FrameNumber: i,
			Seq:         seq,
			IsFinal:     i == total-1,
			TotalFrames: total,
			BPM:         bpm,
			Quantum:     quantum,
			Bars:        bars,
		})
		if err != nil {
			t.Fatal(err)
		}
		streamed = append(streamed, f)
		seq++
	}

	if seq != nextSeq {
		t.Fatalf("sequence mismatch: streaming ended at %d, batch at %d", seq, nextSeq)
	}
	if len(streamed) != len(batch) {
		t.Fatalf("frame count mismatch: streaming %d, batch %d", len(streamed), len(batch))
	}
	for i := range batch {
		if !bytes.Equal(streamed[i], batch[i]) {
			t.Fatalf("frame %d differs between streaming and batch encode", i)
		}
	}
}

// TestEncodeWindowFinalCarriesTrailer decodes the wire bytes of a final window
// and checks the interval trailer fields round-trip.
func TestEncodeWindowFinalCarriesTrailer(t *testing.T) {
	enc, err := NewIntervalEncoder(1, 48000, 64)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]int16, samplesPerWaifFrame(48000))
	wire, err := enc.EncodeWindow(chunk, WindowMeta{
		RoomIndex: 3, StreamID: 9, FrameNumber: 41, Seq: 500,
		IsFinal: true, TotalFrames: 42, BPM: 97.5, Quantum: 4, Bars: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := DecodeAudioFrameWire(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !f.IsFinal || f.TotalFrames != 42 || f.BPM != 97.5 || f.Bars != 8 || f.SampleRate != 48000 {
		t.Fatalf("trailer did not round-trip: %+v", f)
	}
	if f.IntervalIndex != 3 || f.StreamID != 9 || f.FrameNumber != 41 || f.FrameSeq != 500 {
		t.Fatalf("header did not round-trip: %+v", f)
	}
}
