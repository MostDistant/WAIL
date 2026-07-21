package emit

import "testing"

func totals(n int) func(int64) int { return func(int64) int { return n } }

func TestMissingSlotsWithinInterval(t *testing.T) {
	got := MissingSlots(FramePos{5, 2}, FramePos{5, 6}, 3, totals(400), 6)
	want := []FramePos{{5, 3}, {5, 4}, {5, 5}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slot %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMissingSlotsAcrossBoundary(t *testing.T) {
	got := MissingSlots(FramePos{5, 3}, FramePos{6, 1}, 2, totals(5), 6)
	want := []FramePos{{5, 4}, {6, 0}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMissingSlotsCapReturnsGapHead(t *testing.T) {
	got := MissingSlots(FramePos{0, 0}, FramePos{0, 11}, 10, totals(400), 3)
	if len(got) != 3 || got[0] != (FramePos{0, 1}) || got[2] != (FramePos{0, 3}) {
		t.Fatalf("got %v, want head slots 1..3", got)
	}
}

func TestMissingSlotsInconsistentReturnsNil(t *testing.T) {
	// Walking 2 slots from {5,3} lands on {5,6}, not {5,9} → inconsistent.
	if got := MissingSlots(FramePos{5, 3}, FramePos{5, 9}, 2, totals(400), 6); got != nil {
		t.Fatalf("inconsistent walk should return nil, got %v", got)
	}
	// Unknown interval length (totalFor <= 0) → nil.
	if got := MissingSlots(FramePos{5, 3}, FramePos{6, 0}, 1, totals(0), 6); got != nil {
		t.Fatalf("unknown totals should return nil, got %v", got)
	}
}

func TestMissingSlotsZeroGap(t *testing.T) {
	if got := MissingSlots(FramePos{5, 3}, FramePos{5, 4}, 0, totals(400), 6); got != nil {
		t.Fatalf("zero gap should return nil, got %v", got)
	}
}
