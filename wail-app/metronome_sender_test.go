package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
	"github.com/nicholasgasior/wail/wail-app/internal/metronome"
)

// collectMetronomeInterval drives MetronomeSenderTask across two boundaries so
// the first interval's frames are all flushed (the second boundary force-ships
// whatever the wall-clock drip hasn't sent yet), then returns that interval's
// frames in FrameNumber order.
func collectMetronomeInterval(t *testing.T, cfg interval.Config, bpm float64, roomIdx int64) []*AudioFrame {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan []byte, 4096)
	send := func(w []byte) {
		select {
		case got <- w:
		default: // buffer is generously sized; never block the sender goroutine
		}
	}
	boundaryCh := make(chan IntervalBoundaryInfo, 4)
	go MetronomeSenderTask(ctx, metronomeBroadcastStreamID, send, boundaryCh)

	boundaryCh <- IntervalBoundaryInfo{Index: roomIdx, BPM: bpm, Cfg: cfg}
	boundaryCh <- IntervalBoundaryInfo{Index: roomIdx + 1, BPM: bpm, Cfg: cfg}

	var frames []*AudioFrame
	deadline := time.After(3 * time.Second)
	for {
		select {
		case w := <-got:
			f, err := DecodeAudioFrameWire(w)
			if err != nil {
				t.Fatalf("decode wire: %v", err)
			}
			if f.IntervalIndex != roomIdx {
				continue // second interval's frames — not under test here
			}
			frames = append(frames, f)
			if f.IsFinal {
				return frames // frames are emitted in order; final is last
			}
		case <-deadline:
			t.Fatalf("timed out; got %d frames for interval %d", len(frames), roomIdx)
		}
	}
}

func TestMetronomeSenderEmitsRoomTaggedFrames(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 4}
	const bpm = 240.0          // 4 beats @ 240 BPM = 1s = 50 WAIF frames
	const roomIdx = int64(100) // nonzero: proves the room index is honored, not hardcoded
	frames := collectMetronomeInterval(t, cfg, bpm, roomIdx)

	// A fresh encoder over the same render must be byte-identical: Opus is
	// deterministic, and the sender's encoder is fresh on its first interval.
	refEnc, err := NewIntervalEncoder(metChannels, metSampleRate, metBitrateKbps)
	if err != nil {
		t.Fatalf("ref encoder: %v", err)
	}
	pcm := metronome.RenderInterval(cfg, bpm, metSampleRate, metChannels, roomIdx)
	refWire, _, err := refEnc.EncodeInterval(pcm, roomIdx, metronomeBroadcastStreamID, 0, bpm, cfg.Quantum, cfg.Bars)
	if err != nil {
		t.Fatalf("ref encode: %v", err)
	}
	if len(frames) != len(refWire) {
		t.Fatalf("frame count: got %d, want %d", len(frames), len(refWire))
	}

	for i, f := range frames {
		if f.StreamID != metronomeBroadcastStreamID {
			t.Errorf("frame %d stream id: got %d, want %d", i, f.StreamID, metronomeBroadcastStreamID)
		}
		if f.IntervalIndex != roomIdx {
			t.Errorf("frame %d room index: got %d, want %d", i, f.IntervalIndex, roomIdx)
		}
		if f.Channels != metChannels {
			t.Errorf("frame %d channels: got %d, want %d", i, f.Channels, metChannels)
		}
		if int(f.FrameNumber) != i {
			t.Errorf("frame %d frame number: got %d", i, f.FrameNumber)
		}
		ref, derr := DecodeAudioFrameWire(refWire[i])
		if derr != nil {
			t.Fatalf("decode ref %d: %v", i, derr)
		}
		if !bytes.Equal(f.OpusData, ref.OpusData) {
			t.Errorf("frame %d opus payload differs from reference render+encode", i)
		}
	}

	last := frames[len(frames)-1]
	if !last.IsFinal {
		t.Fatal("last frame not marked final")
	}
	if last.SampleRate != metSampleRate || last.TotalFrames != uint32(len(frames)) ||
		last.BPM != bpm || last.Quantum != cfg.Quantum || last.Bars != cfg.Bars {
		t.Errorf("final-frame trailer wrong: rate=%d total=%d bpm=%v quantum=%v bars=%d",
			last.SampleRate, last.TotalFrames, last.BPM, last.Quantum, last.Bars)
	}
}

func TestMetronomeSenderDecodesToAudibleClicks(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 4}
	const bpm = 240.0
	frames := collectMetronomeInterval(t, cfg, bpm, 0)

	dec, err := NewIntervalDecoder(metChannels, metSampleRate)
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	var maxAbs, total int
	for _, f := range frames {
		out, derr := dec.DecodeFrame(f.OpusData)
		if derr != nil {
			t.Fatalf("decode frame %d: %v", f.FrameNumber, derr)
		}
		total += len(out) / metChannels
		for _, s := range out {
			v := int(s)
			if v < 0 {
				v = -v
			}
			if v > maxAbs {
				maxAbs = v
			}
		}
	}
	if maxAbs < 1000 {
		t.Fatalf("decoded metronome nearly silent (max abs %d) — expected audible clicks", maxAbs)
	}
	if want := len(frames) * samplesPerWaifFrame(metSampleRate); total != want {
		t.Fatalf("decoded sample count: got %d, want %d", total, want)
	}
}
