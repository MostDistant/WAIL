package capture

import (
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// Windowed-mode tests use a tiny sample rate so intervals stay small:
// 16 beats at 120 BPM = 8s; at 100 Hz that is 800 frames per interval.
// With windowFrames=100 an interval is exactly 8 windows.
const (
	wsr     = 100
	wframes = 100
)

func wcfg() interval.Config { return interval.Config{Bars: 4, Quantum: 4} }

// beatAt converts a frame offset within interval 0 to a beat at 120 BPM /
// wsr Hz: one beat = 0.5s = 50 frames.
func beatAt(frameOff int) float64 { return float64(frameOff) / 50.0 }

func collectWindows(rs ...[]Window) []Window {
	var all []Window
	for _, r := range rs {
		all = append(all, r...)
	}
	return all
}

func TestWindowsEmitAsCoverageAdvances(t *testing.T) {
	a := NewWindowed(wcfg(), 2, wsr, wframes)

	// 60 frames from offset 0: no full window yet.
	if w := a.AddWindows(beatAt(0), 120, fill(60, 2, 1), 60); len(w) != 0 {
		t.Fatalf("expected no windows after 60/100 frames, got %d", len(w))
	}
	// 60 more (coverage 120): window 0 is complete.
	w := a.AddWindows(beatAt(60), 120, fill(60, 2, 2), 60)
	if len(w) != 1 {
		t.Fatalf("expected exactly window 0, got %d windows", len(w))
	}
	if w[0].IntervalIndex != 0 || w[0].Number != 0 || w[0].IsFinal {
		t.Fatalf("window 0 mislabeled: %+v", w[0])
	}
	if w[0].Total != 8 {
		t.Fatalf("total windows = %d, want 8", w[0].Total)
	}
	if len(w[0].Samples) != wframes*2 {
		t.Fatalf("window sample count = %d, want %d", len(w[0].Samples), wframes*2)
	}
	// Content: frames 0..59 value 1, frames 60..99 value 2.
	if w[0].Samples[0] != 1 || w[0].Samples[59*2] != 1 || w[0].Samples[60*2] != 2 || w[0].Samples[99*2+1] != 2 {
		t.Fatal("window 0 content does not match written buffers")
	}
}

func TestGapWithinIntervalEmitsSilenceWindows(t *testing.T) {
	a := NewWindowed(wcfg(), 1, wsr, wframes)

	a.AddWindows(beatAt(0), 120, fill(100, 1, 5), 100) // window 0 ready
	// Jump to offset 350 (gap 100..350 is silence). Coverage reaches 450:
	// windows 1..3 become ready (1,2 all/partial silence, 3 holds data).
	w := a.AddWindows(beatAt(350), 120, fill(100, 1, 6), 100)
	if len(w) != 3 {
		t.Fatalf("expected windows 1..3, got %d windows", len(w))
	}
	if w[0].Number != 1 || w[1].Number != 2 || w[2].Number != 3 {
		t.Fatalf("window numbers = %d,%d,%d want 1,2,3", w[0].Number, w[1].Number, w[2].Number)
	}
	if w[0].Samples[0] != 0 || w[1].Samples[99] != 0 {
		t.Fatal("gap windows should read as silence")
	}
	if w[2].Samples[50] != 6 {
		t.Fatal("window 3 should hold the second buffer's samples")
	}
}

func TestIntervalCloseFlushesZeroPaddedTailWithFinal(t *testing.T) {
	a := NewWindowed(wcfg(), 1, wsr, wframes)

	a.AddWindows(beatAt(0), 120, fill(200, 1, 3), 200) // windows 0,1 emitted
	// Next buffer opens interval 1 → remaining windows 2..7 flush zero-padded,
	// last one final; the new buffer (100 frames) completes interval 1 window 0.
	w := a.AddWindows(16, 120, fill(100, 1, 9), 100)
	if len(w) != 7 {
		t.Fatalf("expected 6 tail windows + 1 new-interval window, got %d", len(w))
	}
	for i, win := range w[:6] {
		if win.IntervalIndex != 0 || win.Number != 2+i {
			t.Fatalf("tail window %d mislabeled: %+v", i, win)
		}
		if win.Samples[0] != 0 {
			t.Fatal("tail windows must be silence")
		}
	}
	last := w[5]
	if !last.IsFinal || last.Number != 7 {
		t.Fatalf("last tail window should be final #7, got %+v", last)
	}
	if w[6].IntervalIndex != 1 || w[6].Number != 0 || w[6].Samples[0] != 9 {
		t.Fatalf("new interval window mislabeled: %+v", w[6])
	}
	// Only the very last window of an interval is final.
	for _, win := range w[:5] {
		if win.IsFinal {
			t.Fatalf("non-last window marked final: %+v", win)
		}
	}
}

func TestExactCoverageEmitsFinalWithoutClose(t *testing.T) {
	a := NewWindowed(wcfg(), 1, wsr, wframes)

	var all []Window
	for off := 0; off < 800; off += 100 {
		all = collectWindows(all, a.AddWindows(beatAt(off), 120, fill(100, 1, 1), 100))
	}
	if len(all) != 8 {
		t.Fatalf("expected all 8 windows once coverage is exact, got %d", len(all))
	}
	if !all[7].IsFinal {
		t.Fatal("window 7 should be final as soon as it is fully covered")
	}
	// Closing the interval later must not re-emit anything.
	w := a.AddWindows(16, 120, fill(10, 1, 2), 10)
	if len(w) != 0 {
		t.Fatalf("close after full emission should emit nothing, got %d windows", len(w))
	}
}

func TestShortFinalWindowIsPadded(t *testing.T) {
	// sr=90: 8s interval = 720 frames; windows of 100 → 8 windows, final holds
	// 20 frames + 80 frames of padding.
	a := NewWindowed(wcfg(), 1, 90, wframes)
	// One beat = 45 frames at 90 Hz / 120 BPM.
	var all []Window
	all = collectWindows(all, a.AddWindows(0, 120, fill(720, 1, 4), 720))
	if len(all) != 8 {
		t.Fatalf("expected 8 windows for a 720-frame interval, got %d", len(all))
	}
	final := all[7]
	if !final.IsFinal || final.Total != 8 {
		t.Fatalf("final window mislabeled: %+v", final)
	}
	if len(final.Samples) != wframes {
		t.Fatalf("final window must be padded to %d frames, got %d", wframes, len(final.Samples))
	}
	if final.Samples[19] != 4 || final.Samples[20] != 0 {
		t.Fatal("final window should hold 20 data frames then silence padding")
	}
}

func TestFlushWindowsEmitsRemainingTail(t *testing.T) {
	a := NewWindowed(wcfg(), 1, wsr, wframes)
	a.AddWindows(beatAt(0), 120, fill(150, 1, 2), 150) // window 0 emitted
	w := a.FlushWindows()
	if len(w) != 7 {
		t.Fatalf("flush should emit windows 1..7, got %d", len(w))
	}
	if !w[6].IsFinal || w[6].Number != 7 {
		t.Fatalf("flushed tail should end with final #7, got %+v", w[6])
	}
	if a.FlushWindows() != nil {
		t.Fatal("second flush should be empty")
	}
}

func TestBackfillBehindEmittedWindowIsDropped(t *testing.T) {
	a := NewWindowed(wcfg(), 1, wsr, wframes)
	w := a.AddWindows(beatAt(0), 120, fill(100, 1, 1), 100)
	if len(w) != 1 {
		t.Fatalf("window 0 should be emitted, got %d", len(w))
	}
	// A buffer entirely behind the emitted boundary cannot amend window 0.
	if w := a.AddWindows(beatAt(10), 120, fill(50, 1, 9), 50); len(w) != 0 {
		t.Fatal("backfill must not emit windows")
	}
	if a.DroppedBackfill() == 0 {
		t.Fatal("backfill drop should be counted")
	}
}

func TestLateIntervalBufferStillDropped(t *testing.T) {
	a := NewWindowed(wcfg(), 1, wsr, wframes)
	a.AddWindows(16, 120, fill(10, 1, 1), 10) // opens interval 1
	if w := a.AddWindows(0, 120, fill(10, 1, 2), 10); len(w) != 0 {
		t.Fatal("buffer for an already-passed interval must be dropped")
	}
	if a.DroppedLate() == 0 {
		t.Fatal("late drop should be counted")
	}
}

func TestStreamingMatchesBatchAssembly(t *testing.T) {
	// The same buffer sequence through batch and windowed assemblers must
	// yield identical interval PCM (windows concatenated == batch samples,
	// modulo final-window padding).
	batch := New(wcfg(), 2, wsr)
	stream := NewWindowed(wcfg(), 2, wsr, wframes)

	type buf struct {
		off, n int
		v      int16
	}
	bufs := []buf{{0, 130, 1}, {130, 70, 2}, {200, 250, 3}, {520, 200, 4}} // gap 450..520
	var windows []Window
	for _, b := range bufs {
		batch.Add(beatAt(b.off), 120, fill(b.n, 2, b.v), b.n)
		windows = collectWindows(windows, stream.AddWindows(beatAt(b.off), 120, fill(b.n, 2, b.v), b.n))
	}
	completed := batch.Add(16, 120, fill(10, 2, 0), 10) // close interval 0
	windows = collectWindows(windows, stream.AddWindows(16, 120, fill(10, 2, 0), 10))

	if completed == nil {
		t.Fatal("batch assembler should complete interval 0")
	}
	var got []int16
	for _, w := range windows {
		if w.IntervalIndex != 0 {
			continue
		}
		got = append(got, w.Samples...)
	}
	want := completed.Samples // 800 frames × 2ch; window concat is the same length here
	if len(got) != len(want) {
		t.Fatalf("streamed length %d != batch length %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d differs: stream %d, batch %d", i, got[i], want[i])
		}
	}
}
