package capture

import (
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

const sr = 48000

// fill returns n*channels samples all equal to v (so we can assert placement).
func fill(nFrames, channels int, v int16) []int16 {
	s := make([]int16, nFrames*channels)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestBucketsIntoIntervalsOnAdvance(t *testing.T) {
	cfg := interval.Config{Bars: 4, Quantum: 4} // 16 beats; at 120 BPM = 8s = 384000 frames
	a := New(cfg, 2, sr)

	// A buffer at beat 0 (interval 0). No completed interval yet.
	if c := a.Add(0, 120, fill(480, 2, 1), 480); c != nil {
		t.Fatalf("first add should not complete an interval, got %+v", c)
	}
	// Another buffer still in interval 0 (beat 8, well inside 0..16).
	if c := a.Add(8, 120, fill(480, 2, 1), 480); c != nil {
		t.Fatal("still in interval 0, should not complete")
	}
	// A buffer at beat 16 opens interval 1 → interval 0 completes.
	c := a.Add(16, 120, fill(480, 2, 2), 480)
	if c == nil {
		t.Fatal("crossing into interval 1 should complete interval 0")
	}
	if c.Index != 0 {
		t.Fatalf("completed index = %d, want 0", c.Index)
	}
	if c.Frames != 384000 || c.Channels != 2 {
		t.Fatalf("completed frames=%d channels=%d, want 384000/2", c.Frames, c.Channels)
	}
	if len(c.Samples) != 384000*2 {
		t.Fatalf("samples len = %d, want %d", len(c.Samples), 384000*2)
	}
}

func TestFrameOffsetPlacement(t *testing.T) {
	cfg := interval.Config{Bars: 4, Quantum: 4}
	a := New(cfg, 1, sr)

	// Buffer at beat 1 (0.5s at 120 BPM) → offset 24000 frames within interval 0.
	a.Add(1, 120, fill(100, 1, 7), 100)
	c := a.Flush()
	if c == nil {
		t.Fatal("flush should return the partial interval 0")
	}
	// Frames [0,24000) are silence; [24000, 24100) hold the value 7.
	if c.Samples[23999] != 0 {
		t.Fatalf("sample just before offset should be silence, got %d", c.Samples[23999])
	}
	if c.Samples[24000] != 7 || c.Samples[24099] != 7 {
		t.Fatalf("buffer not placed at expected offset")
	}
	if c.Samples[24100] != 0 {
		t.Fatalf("sample just after buffer should be silence, got %d", c.Samples[24100])
	}
	if c.WrittenFrames != 24100 {
		t.Fatalf("WrittenFrames = %d, want 24100", c.WrittenFrames)
	}
	if c.Complete() {
		t.Fatal("a partial interval should not report Complete()")
	}
}

func TestCompleteWhenFullyCovered(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 1} // 1 beat; at 120 BPM = 0.5s = 24000 frames
	a := New(cfg, 1, sr)
	// One buffer covering the whole interval.
	a.Add(0, 120, fill(24000, 1, 3), 24000)
	c := a.Flush()
	if c == nil || !c.Complete() {
		t.Fatalf("interval fully covered should report Complete(); got %+v", c)
	}
}

func TestLateBufferDropped(t *testing.T) {
	cfg := interval.Config{Bars: 4, Quantum: 4}
	a := New(cfg, 1, sr)
	a.Add(16, 120, fill(100, 1, 1), 100) // interval 1
	// A buffer for interval 0 arriving now is late → dropped.
	if c := a.Add(0, 120, fill(100, 1, 1), 100); c != nil {
		t.Fatal("late buffer should not complete anything")
	}
	if a.DroppedLate() != 1 {
		t.Fatalf("DroppedLate = %d, want 1", a.DroppedLate())
	}
}

func TestBoundaryStraddleClamped(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 1} // 24000 frames at 120 BPM
	a := New(cfg, 1, sr)
	// A buffer starting near the end that would overflow the interval must clamp
	// (not panic / not write past the buffer).
	a.Add(0.999, 120, fill(5000, 1, 9), 5000)
	c := a.Flush()
	if c == nil {
		t.Fatal("expected an interval")
	}
	if c.WrittenFrames > c.Frames {
		t.Fatalf("WrittenFrames %d exceeds Frames %d", c.WrittenFrames, c.Frames)
	}
}
