package lanloss

import "testing"

func TestNoLossOnSequential(t *testing.T) {
	var tr Tracker
	for c := uint64(10); c < 20; c++ {
		if g := tr.Observe(c); g != nil {
			t.Fatalf("unexpected gap at count %d: %+v", c, g)
		}
	}
	if tr.LostBuffers() != 0 || tr.GapEvents() != 0 {
		t.Fatalf("expected no loss, got lost=%d events=%d", tr.LostBuffers(), tr.GapEvents())
	}
}

func TestDetectsSingleGap(t *testing.T) {
	var tr Tracker
	tr.Observe(0)
	tr.Observe(1)
	g := tr.Observe(4) // 2 and 3 lost
	if g == nil {
		t.Fatal("expected a gap")
	}
	if g.ExpectedCount != 2 || g.GotCount != 4 || g.LostBuffers != 2 {
		t.Fatalf("gap = %+v, want expected=2 got=4 lost=2", g)
	}
	if tr.LostBuffers() != 2 || tr.GapEvents() != 1 {
		t.Fatalf("lost=%d events=%d, want 2 and 1", tr.LostBuffers(), tr.GapEvents())
	}
	// Continue sequentially from the new baseline.
	if g := tr.Observe(5); g != nil {
		t.Fatalf("unexpected gap after recovery: %+v", g)
	}
}

func TestReorderNotDoubleCounted(t *testing.T) {
	var tr Tracker
	tr.Observe(0)
	tr.Observe(3) // lost 1,2
	// A late-arriving 1 and 2 (reordered) must not add to the loss count.
	tr.Observe(1)
	tr.Observe(2)
	if tr.LostBuffers() != 2 {
		t.Fatalf("lost=%d, want 2 (reorders must not double-count)", tr.LostBuffers())
	}
	if tr.Reorders() != 2 {
		t.Fatalf("reorders=%d, want 2", tr.Reorders())
	}
}

func TestDuplicateIgnored(t *testing.T) {
	var tr Tracker
	tr.Observe(5)
	tr.Observe(6)
	if g := tr.Observe(6); g != nil { // duplicate
		t.Fatalf("duplicate should not be a gap: %+v", g)
	}
	if tr.LostBuffers() != 0 {
		t.Fatalf("lost=%d, want 0", tr.LostBuffers())
	}
}
