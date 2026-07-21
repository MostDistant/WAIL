package main

import (
	"math"
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/capture"
	"github.com/nicholasgasior/wail/wail-app/internal/emit"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
	"github.com/nicholasgasior/wail/wail-app/internal/playout"
)

// Interval-boundary continuity tests: capture a continuous signal through the
// real sender pieces (windowed assembler → reassembler), play it back through
// the real receiver pieces (playout scheduler → paced readers → cushion
// feeder) wired exactly like onBoundary/topUpSinks, and assert the emitted
// stream is gapless in both PCM and beat stamps across interval boundaries.
//
// At tempos where the interval length is not a multiple of the 960-frame WAIF
// window (e.g. 124.59 BPM → 369,853 frames), the final window runs past the
// interval end. A continuation-padding sender fills that pad with the next
// interval's real head — the receiver plays through it and starts the next
// interval past its twice-encoded head. A zero-padding (old) sender leaves it
// silent — the receiver truncates at the exact interval end instead.

const (
	testEmitRate     = 48000
	testEmitChannels = 2
	testEmitCushion  = testEmitRate * 160 / 1000 // mirrors emitCushionFrames
	testEmitChunk    = testEmitRate * 5 / 1000   // mirrors emitChunkFrames
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

// fillViaAssembler runs the continuous source through the real capture
// assembler (a continuation-padding sender) into the reassembler.
func fillViaAssembler(t *testing.T, reasm *emit.Reassembler, tempo float64, cfg interval.Config, nIntervals int) {
	t.Helper()
	windowFrames := samplesPerWaifFrame(testEmitRate)
	intervalFrames := cfg.IntervalSamples(testEmitRate, tempo)
	frameBeats := tempo / 60.0 / testEmitRate

	asm := capture.NewWindowed(cfg, testEmitChannels, testEmitRate, windowFrames)
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
}

// fillZeroPad synthesizes an old sender: every interval's final window is
// zero-padded past the interval end.
func fillZeroPad(t *testing.T, reasm *emit.Reassembler, tempo float64, cfg interval.Config, nIntervals int) {
	t.Helper()
	windowFrames := samplesPerWaifFrame(testEmitRate)
	intervalFrames := cfg.IntervalSamples(testEmitRate, tempo)
	total := (intervalFrames + windowFrames - 1) / windowFrames

	for idx := 0; idx < nIntervals; idx++ {
		for k := 0; k < total; k++ {
			w := make([]int16, windowFrames*testEmitChannels)
			for f := 0; f < windowFrames; f++ {
				src := k*windowFrames + f
				if src >= intervalFrames {
					break // zero pad
				}
				l, r := testSourceFrame(idx*intervalFrames + src)
				w[f*testEmitChannels] = l
				w[f*testEmitChannels+1] = r
			}
			reasm.Add(int64(idx), k, w, k == total-1, total)
		}
	}
}

// runBoundaryPlayback plays nIntervals of reassembled audio across the
// interval boundaries, wired exactly like onBoundary + topUpSinks, returning
// the committed chunks in commit order.
func runBoundaryPlayback(t *testing.T, reasm *emit.Reassembler, tempo float64, cfg interval.Config, nIntervals int) []emittedChunk {
	t.Helper()
	for idx := int64(0); idx < int64(nIntervals); idx++ {
		if !reasm.Complete(idx) {
			t.Fatalf("interval %d not fully captured", idx)
		}
	}

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
			paddedFrames := intervalPaddedFrames(cfg, testEmitRate, tempo)
			release, advanced := sched.OnBoundary(localIdx)
			if advanced {
				reasm.Drop(release - 1)
				nextIdx := release + 1
				idx := release
				makeNext := func() (*emit.PacedReader, int64, int) {
					start := 0
					if cur := feeder.Current(); cur != nil && paddedFrames > totalFrames {
						if s, _, _, ok := reasm.Interval(idx); ok && padCarriesAudio(s, totalFrames, paddedFrames, testEmitChannels) {
							cur.SetTotalFrames(paddedFrames)
							start = paddedFrames - totalFrames
						}
					}
					return emit.NewPacedReader(
						func() []int16 { s, _, _, _ := reasm.Interval(nextIdx); return s },
						testEmitChannels, testEmitRate, tempo, we, totalFrames,
					), nextIdx, start
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

	// Beat stamps must be exactly contiguous chunk to chunk: any overlap or
	// gap is a splice on the Link Audio timeline. Allow one frame of slack for
	// the sub-sample rounding residual of IntervalSamples at the handoff.
	for i := 1; i < len(out); i++ {
		prev := out[i-1]
		want := prev.beat + float64(len(prev.samples)/testEmitChannels)*frameBeats
		if d := math.Abs(out[i].beat - want); d > frameBeats {
			t.Fatalf("beat stamp discontinuity at chunk %d (beat %.5f): got %.5f, want %.5f (off by %.2f frames)",
				i, prev.beat, out[i].beat, want, d/frameBeats)
		}
	}

	// The emitted PCM must be the source signal, sample for sample — no
	// inserted silence or repeats at boundaries. The first chunk may start a
	// few frames into the stream (boundary detected a tick late; the feeder
	// skips the shortfall silently), so align by the first chunk's beat stamp.
	// Both handoff modes keep the concatenated stream contiguous in the source
	// domain: a continuation handoff plays interval N through its pad and
	// resumes N+1 past its twice-encoded head; a truncate handoff stops at the
	// exact interval end and resumes N+1 at frame 0.
	skip := int(math.Round((out[0].beat - bpi) * 60.0 / tempo * testEmitRate))
	pos := skip // frame index into the continuous source signal
	for ci, c := range out {
		frames := len(c.samples) / testEmitChannels
		for k := 0; k < frames; k++ {
			srcIdx := pos + k
			wl, wr := testSourceFrame(srcIdx)
			gl, gr := c.samples[k*testEmitChannels], c.samples[k*testEmitChannels+1]
			if gl != wl || gr != wr {
				boundary := srcIdx / intervalFrames
				into := srcIdx % intervalFrames
				t.Fatalf("PCM mismatch in chunk %d at source frame %d (interval %d, frame %d, %.1fms from interval end): got (%d,%d), want (%d,%d)",
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
	reasm := emit.New(testEmitChannels, samplesPerWaifFrame(testEmitRate))
	fillViaAssembler(t, reasm, tempo, cfg, 3)
	out := runBoundaryPlayback(t, reasm, tempo, cfg, 3)
	assertBoundaryContinuity(t, tempo, cfg, out)
}

func TestEmitBoundaryZeroPadSenderCompat(t *testing.T) {
	cfg := interval.Config{Bars: 4, Quantum: 4}
	tempo := 124.59
	reasm := emit.New(testEmitChannels, samplesPerWaifFrame(testEmitRate))
	fillZeroPad(t, reasm, tempo, cfg, 3)
	out := runBoundaryPlayback(t, reasm, tempo, cfg, 3)
	assertBoundaryContinuity(t, tempo, cfg, out)
}

func TestEmitBoundaryAtFrameAlignedTempo(t *testing.T) {
	cfg := interval.Config{Bars: 4, Quantum: 4}
	tempo := 120.0 // 384,000 frames/interval — exactly 400 WAIF windows
	if cfg.IntervalSamples(testEmitRate, tempo)%samplesPerWaifFrame(testEmitRate) != 0 {
		t.Fatal("test tempo unexpectedly unaligned")
	}
	reasm := emit.New(testEmitChannels, samplesPerWaifFrame(testEmitRate))
	fillViaAssembler(t, reasm, tempo, cfg, 3)
	out := runBoundaryPlayback(t, reasm, tempo, cfg, 3)
	assertBoundaryContinuity(t, tempo, cfg, out)
}
