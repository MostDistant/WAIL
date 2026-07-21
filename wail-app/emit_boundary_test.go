package main

import (
	"math"
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/capture"
	"github.com/nicholasgasior/wail/wail-app/internal/emit"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
	"github.com/nicholasgasior/wail/wail-app/internal/playout"
)

// Interval-boundary continuity test: capture a continuous signal through the
// real sender pieces (windowed assembler → reassembler), play it back through
// the real receiver pieces (playout scheduler → paced readers → cushion
// feeder) wired exactly like onBoundary/topUpSinks, and assert the emitted
// stream is gapless in both PCM and beat stamps across interval boundaries.
//
// At tempos where the interval length is not a multiple of the 960-frame WAIF
// window (e.g. 124.59 BPM → 369,853 frames), the sender zero-pads the final
// window; playing that padding splices ~15ms of silence into continuous audio
// and overlaps the next interval's stamps — an audible click at every boundary.

const (
	testEmitRate     = 48000
	testEmitChannels = 2
	testEmitCushion  = testEmitRate * 80 / 1000 // mirrors emitCushionFrames
	testEmitChunk    = testEmitRate * 5 / 1000  // mirrors emitChunkFrames
)

// testSourceFrame generates the continuous never-zero test signal: frame i
// carries i-derived values on both channels so any inserted silence, repeat,
// or skip is detectable by direct comparison.
func testSourceFrame(i int) (l, r int16) {
	v := int16(1 + i%30000)
	return v, -v
}

type emittedChunk struct {
	beat    float64
	samples []int16
}

// runBoundaryPlayback pushes nIntervals of continuous audio through capture
// assembly into a reassembler, then plays it back across the interval
// boundaries, returning the committed chunks in commit order.
func runBoundaryPlayback(t *testing.T, tempo float64, cfg interval.Config, nIntervals int) []emittedChunk {
	t.Helper()
	windowFrames := samplesPerWaifFrame(testEmitRate)
	intervalFrames := cfg.IntervalSamples(testEmitRate, tempo)
	frameBeats := tempo / 60.0 / testEmitRate // beats spanned by one frame

	// --- sender: continuous signal through the windowed assembler ---
	asm := capture.NewWindowed(cfg, testEmitChannels, testEmitRate, windowFrames)
	reasm := emit.New(testEmitChannels, windowFrames)

	const bufFrames = 480 // 10ms capture buffers
	totalFeed := intervalFrames*nIntervals + testEmitRate/10
	buf := make([]int16, bufFrames*testEmitChannels)
	for off := 0; off < totalFeed; off += bufFrames {
		for k := 0; k < bufFrames; k++ {
			l, r := testSourceFrame(off + k)
			buf[k*testEmitChannels] = l
			buf[k*testEmitChannels+1] = r
		}
		beat := float64(off) * frameBeats
		for _, w := range asm.AddWindows(beat, tempo, buf, bufFrames) {
			reasm.Add(w.IntervalIndex, w.Number, w.Samples, w.IsFinal, w.Total)
		}
	}
	for idx := int64(0); idx < int64(nIntervals); idx++ {
		if !reasm.Complete(idx) {
			t.Fatalf("interval %d not fully captured", idx)
		}
	}

	// --- receiver: boundary handoff wired like onBoundary + topUpSinks ---
	sched := playout.New(1) // D=1
	feeder := emit.NewFeeder(testEmitCushion, testEmitChunk)
	var out []emittedChunk
	collect := func(samples []int16, beat float64) {
		cp := make([]int16, len(samples))
		copy(cp, samples)
		out = append(out, emittedChunk{beat: beat, samples: cp})
	}

	bpi := cfg.BeatsPerInterval()
	tickBeats := 0.005 * tempo / 60.0
	startBeat := bpi + 0.2*tickBeats // just past the boundary that releases interval 0
	// Through the boundary that releases the last fed interval plus half its
	// playback window (stopping before the next, only-partially-fed interval
	// would be released).
	endLoop := bpi*float64(nIntervals) + bpi/2

	var lastIdx int64
	haveLast := false
	for beat := startBeat; beat < endLoop; beat += tickBeats {
		localIdx := cfg.IndexAtBeat(beat)
		if !haveLast || localIdx > lastIdx {
			ws, we := cfg.BeatWindow(localIdx)
			totalFrames := intervalPlayoutFrames(cfg, testEmitRate, tempo)
			release, advanced := sched.OnBoundary(localIdx)
			if advanced {
				reasm.Drop(release - 1)
				nextIdx := release + 1
				makeNext := func() (*emit.PacedReader, int64) {
					return emit.NewPacedReader(
						func() []int16 { s, _, _, _ := reasm.Interval(nextIdx); return s },
						testEmitChannels, testEmitRate, tempo, we, totalFrames,
					), nextIdx
				}
				if !feeder.Promote(release, makeNext) {
					feeder.SetCurrent(release, emit.NewPacedReader(
						func() []int16 { s, _, _, _ := reasm.Interval(release); return s },
						testEmitChannels, testEmitRate, tempo, ws, totalFrames,
					), makeNext)
				}
			}
			lastIdx = localIdx
			haveLast = true
		}
		feeder.Advance(beat, collect)
	}
	if ev, fr := feeder.Underruns(); ev != 0 || fr != 0 {
		t.Fatalf("unexpected feeder underruns: %d events, %d frames", ev, fr)
	}
	if len(out) == 0 {
		t.Fatal("no chunks emitted")
	}
	return out
}

func assertBoundaryContinuity(t *testing.T, tempo float64, cfg interval.Config, out []emittedChunk) {
	t.Helper()
	intervalFrames := cfg.IntervalSamples(testEmitRate, tempo)
	frameBeats := tempo / 60.0 / testEmitRate
	bpi := cfg.BeatsPerInterval()

	// Beat stamps must be exactly contiguous chunk to chunk: any overlap (the
	// padded tail stamped past the next interval's anchor) or gap is a splice
	// on the Link Audio timeline. Allow one frame of slack for the sub-sample
	// rounding residual of IntervalSamples at the interval handoff.
	for i := 1; i < len(out); i++ {
		prev := out[i-1]
		want := prev.beat + float64(len(prev.samples)/testEmitChannels)*frameBeats
		if d := math.Abs(out[i].beat - want); d > frameBeats {
			t.Fatalf("beat stamp discontinuity at chunk %d (beat %.5f): got %.5f, want %.5f (off by %.2f frames)",
				i, prev.beat, out[i].beat, want, d/frameBeats)
		}
	}

	// The emitted PCM must be the source signal, sample for sample — no
	// inserted silence at boundaries. The first chunk may start a few frames
	// into the stream (boundary detected a tick late; the feeder skips the
	// shortfall silently), so align by the first chunk's beat stamp.
	skip := int(math.Round((out[0].beat - bpi) * 60.0 / tempo * testEmitRate))
	pos := skip // frame index into the continuous source signal
	for ci, c := range out {
		frames := len(c.samples) / testEmitChannels
		for k := 0; k < frames; k++ {
			// Interval N's reader frame f replays source frame
			// intervalFrames*N + f, so pos indexes source frames directly.
			srcIdx := pos + k
			wl, wr := testSourceFrame(srcIdx)
			gl, gr := c.samples[k*testEmitChannels], c.samples[k*testEmitChannels+1]
			if gl != wl || gr != wr {
				boundary := srcIdx / intervalFrames
				into := srcIdx % intervalFrames
				t.Fatalf("PCM mismatch in chunk %d at source frame %d (interval %d, frame %d, %.1fms from interval end): got (%d,%d), want (%d,%d) — inserted padding?",
					ci, srcIdx, boundary, into,
					float64(intervalFrames-into)*1000/testEmitRate, gl, gr, wl, wr)
			}
		}
		pos += frames
	}
}

func TestEmitBoundaryAtNonFrameAlignedTempo(t *testing.T) {
	cfg := interval.Config{Bars: 4, Quantum: 4}
	tempo := 124.59 // 369,853 frames/interval — NOT a multiple of 960
	if cfg.IntervalSamples(testEmitRate, tempo)%samplesPerWaifFrame(testEmitRate) == 0 {
		t.Fatal("test tempo unexpectedly frame-aligned; pick another tempo")
	}
	out := runBoundaryPlayback(t, tempo, cfg, 3)
	assertBoundaryContinuity(t, tempo, cfg, out)
}

func TestEmitBoundaryAtFrameAlignedTempo(t *testing.T) {
	cfg := interval.Config{Bars: 4, Quantum: 4}
	tempo := 120.0 // 384,000 frames/interval — exactly 400 WAIF windows
	if cfg.IntervalSamples(testEmitRate, tempo)%samplesPerWaifFrame(testEmitRate) != 0 {
		t.Fatal("test tempo unexpectedly unaligned")
	}
	out := runBoundaryPlayback(t, tempo, cfg, 3)
	assertBoundaryContinuity(t, tempo, cfg, out)
}
