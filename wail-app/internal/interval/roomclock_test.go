package interval

import "testing"

func TestRoomClockIndexAt(t *testing.T) {
	// Anchor: interval 100 began at t=0, 120 BPM, 4×4 (16 beats = 8s per interval).
	rc := NewRoomClock(Anchor{
		Index:    100,
		AtMicros: 0,
		TempoBPM: 120,
		Config:   Config{Bars: 4, Quantum: 4},
	})
	cases := []struct {
		micros int64
		want   int64
	}{
		{0, 100},
		{7_999_999, 100},  // just before the 8s boundary
		{8_000_000, 101},  // exactly one interval later
		{16_000_000, 102}, // two intervals
		{-1, 99},          // before the anchor
	}
	for _, tc := range cases {
		if got := rc.IndexAt(tc.micros); got != tc.want {
			t.Errorf("IndexAt(%d) = %d, want %d", tc.micros, got, tc.want)
		}
	}
}

func TestRoomClockBoundaryInvertsIndex(t *testing.T) {
	rc := NewRoomClock(Anchor{Index: 100, AtMicros: 5_000_000, TempoBPM: 140, Config: Config{Bars: 8, Quantum: 4}})
	for idx := int64(97); idx <= 105; idx++ {
		tb := rc.BoundaryMicros(idx)
		if got := rc.IndexAt(tb); got != idx {
			t.Errorf("BoundaryMicros(%d)=%d maps back to %d", idx, tb, got)
		}
		// One microsecond before a boundary belongs to the previous interval.
		if got := rc.IndexAt(tb - 1); got != idx-1 {
			t.Errorf("just before boundary of %d maps to %d, want %d", idx, got, idx-1)
		}
	}
}

// TestReanchorQuantizesTempoToNextBoundary verifies a mid-interval tempo change
// lets the current interval finish at the old tempo and applies the new tempo
// from the next boundary, with a continuous index.
func TestReanchorQuantizesTempoToNextBoundary(t *testing.T) {
	rc := NewRoomClock(Anchor{Index: 0, AtMicros: 0, TempoBPM: 120, Config: Config{Bars: 4, Quantum: 4}})
	// 8s per interval at 120 BPM. At t=20s we're mid-interval-2 (16s..24s).
	now := int64(20_000_000)
	if idx := rc.IndexAt(now); idx != 2 {
		t.Fatalf("precondition: index at 20s = %d, want 2", idx)
	}

	// Double the tempo mid-interval. It should take effect at interval 3 (24s).
	rc.Reanchor(now, 240, Config{Bars: 4, Quantum: 4})

	if got := rc.Anchor().AtMicros; got != 24_000_000 {
		t.Fatalf("reanchored AtMicros = %d, want 24s (next boundary at old tempo)", got)
	}
	// Interval 2 still ends at 24s under the old tempo.
	if got := rc.IndexAt(23_999_999); got != 2 {
		t.Fatalf("index just before 24s = %d, want 2 (old tempo still governs)", got)
	}
	if got := rc.IndexAt(24_000_000); got != 3 {
		t.Fatalf("index at 24s = %d, want 3", got)
	}
	// At 240 BPM an interval is 4s. Interval 4 begins at 24s + 4s = 28s.
	if got := rc.IndexAt(28_000_000); got != 4 {
		t.Fatalf("index at 28s after tempo doubling = %d, want 4", got)
	}
	if got := rc.BoundaryMicros(4); got != 28_000_000 {
		t.Fatalf("boundary of interval 4 = %d, want 28s", got)
	}
}
