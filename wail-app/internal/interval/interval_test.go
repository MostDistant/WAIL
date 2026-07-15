package interval

import (
	"math"
	"testing"
)

func TestBeatsPerInterval(t *testing.T) {
	if got := (Config{Bars: 4, Quantum: 4}).BeatsPerInterval(); got != 16 {
		t.Fatalf("4x4 = %v, want 16", got)
	}
	if got := (Config{Bars: 8, Quantum: 3}).BeatsPerInterval(); got != 24 {
		t.Fatalf("8x3 = %v, want 24", got)
	}
}

func TestIndexAtBeat(t *testing.T) {
	c := Config{Bars: 4, Quantum: 4} // 16 beats/interval
	cases := []struct {
		beat float64
		want int64
	}{
		{0, 0},
		{15.999, 0},
		{16, 1},
		{31.5, 1},
		{32, 2},
		{-0.5, -1}, // negative beats floor toward -inf
		{-16, -1},
		{-16.0001, -2},
	}
	for _, tc := range cases {
		if got := c.IndexAtBeat(tc.beat); got != tc.want {
			t.Errorf("IndexAtBeat(%v) = %d, want %d", tc.beat, got, tc.want)
		}
	}
}

func TestBeatWindowRoundTrips(t *testing.T) {
	c := Config{Bars: 4, Quantum: 4}
	for idx := int64(-3); idx <= 3; idx++ {
		start, end := c.BeatWindow(idx)
		if c.IndexAtBeat(start) != idx {
			t.Errorf("window start %v of interval %d maps to %d", start, idx, c.IndexAtBeat(start))
		}
		// A beat just inside the end belongs to the same interval.
		if c.IndexAtBeat(end-1e-6) != idx {
			t.Errorf("window end-eps of interval %d maps to %d", idx, c.IndexAtBeat(end-1e-6))
		}
		// The end beat itself belongs to the next interval.
		if c.IndexAtBeat(end) != idx+1 {
			t.Errorf("window end %v of interval %d maps to %d, want %d", end, idx, c.IndexAtBeat(end), idx+1)
		}
	}
}

func TestIntervalSamples(t *testing.T) {
	c := Config{Bars: 4, Quantum: 4} // 16 beats
	// 16 beats at 120 BPM = 8 seconds; at 48 kHz = 384000 frames.
	if got := c.IntervalSamples(48000, 120); got != 384000 {
		t.Fatalf("IntervalSamples = %d, want 384000", got)
	}
	// 16 beats at 60 BPM = 16 seconds; at 44100 = 705600.
	if got := c.IntervalSamples(44100, 60); got != 705600 {
		t.Fatalf("IntervalSamples = %d, want 705600", got)
	}
}

func TestFrameOffset(t *testing.T) {
	c := Config{Bars: 4, Quantum: 4} // 16 beats
	// Interval 2 starts at beat 32. A buffer beginning at beat 32 → offset 0.
	if got := c.FrameOffset(32, 2, 48000, 120); got != 0 {
		t.Fatalf("offset at window start = %d, want 0", got)
	}
	// Beat 33 is 1 beat in = 0.5s at 120 BPM = 24000 frames.
	if got := c.FrameOffset(33, 2, 48000, 120); got != 24000 {
		t.Fatalf("offset at +1 beat = %d, want 24000", got)
	}
	// A buffer before the window start clamps to 0, never negative.
	if got := c.FrameOffset(31, 2, 48000, 120); got != 0 {
		t.Fatalf("pre-window offset = %d, want 0 (clamped)", got)
	}
	// A buffer past the window end clamps to IntervalSamples.
	max := c.IntervalSamples(48000, 120)
	if got := c.FrameOffset(999, 2, 48000, 120); got != max {
		t.Fatalf("post-window offset = %d, want %d (clamped)", got, max)
	}
}

// TestSafetyClamps covers the divide-by-zero / bad-input cases: zero bars, zero
// quantum, zero/negative/NaN tempo must never panic or produce Inf/NaN.
func TestSafetyClamps(t *testing.T) {
	bad := []Config{
		{Bars: 0, Quantum: 0},
		{Bars: 0, Quantum: 4},
		{Bars: 4, Quantum: 0},
	}
	for _, c := range bad {
		if bpi := c.BeatsPerInterval(); bpi <= 0 || math.IsNaN(bpi) || math.IsInf(bpi, 0) {
			t.Fatalf("BeatsPerInterval(%+v) = %v, want finite positive", c, bpi)
		}
		if idx := c.IndexAtBeat(10); idx < 0 { // must not panic / must be finite
			t.Fatalf("IndexAtBeat with %+v = %d", c, idx)
		}
	}

	c := Config{Bars: 4, Quantum: 4}
	for _, tempo := range []float64{0, -50, math.NaN()} {
		if s := c.IntervalSamples(48000, tempo); s <= 0 || s > 1<<30 {
			t.Fatalf("IntervalSamples with tempo %v = %d, want sane positive", tempo, s)
		}
		if off := c.FrameOffset(33, 0, 48000, tempo); off < 0 {
			t.Fatalf("FrameOffset with tempo %v = %d, want >= 0", tempo, off)
		}
	}
}
