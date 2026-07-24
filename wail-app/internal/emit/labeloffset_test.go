package emit

import "testing"

func TestLabelOffsetHealthyStream(t *testing.T) {
	var tr LabelOffsetTracker
	// Interval 10: 100 frames labeled 10 (healthy) + 2 stragglers labeled 9.
	for i := 0; i < 100; i++ {
		tr.Add(10, 10)
	}
	tr.Add(10, 9)
	tr.Add(10, 9)
	// Roll to interval 11: finalizes interval 10 → verdict 0.
	if v, changed := tr.Add(11, 11); !changed || v != 0 {
		t.Fatalf("roll verdict = (%d, %v), want (0, true)", v, changed)
	}
	if v, ok := tr.Verdict(); !ok || v != 0 {
		t.Fatalf("Verdict = (%d, %v), want (0, true)", v, ok)
	}
}

func TestLabelOffsetLaggingPeer(t *testing.T) {
	var tr LabelOffsetTracker
	// Every frame labeled one behind the local room index.
	for i := 0; i < 50; i++ {
		tr.Add(20, 19)
	}
	v, changed := tr.Add(21, 20)
	if !changed || v != -1 {
		t.Fatalf("lagging peer verdict = (%d, %v), want (-1, true)", v, changed)
	}
}

func TestLabelOffsetVerdictChangeReportedOnce(t *testing.T) {
	var tr LabelOffsetTracker
	for i := 0; i < 50; i++ {
		tr.Add(30, 29)
	}
	tr.Add(31, 30) // finalize: -1
	// Next interval also lags: no new verdict to report.
	for i := 0; i < 50; i++ {
		if _, changed := tr.Add(31, 30); changed {
			t.Fatal("verdict re-reported without change")
		}
	}
	if _, changed := tr.Add(32, 31); changed {
		t.Fatal("same verdict (-1) must not re-report")
	}
}

func TestLabelOffsetRecovery(t *testing.T) {
	var tr LabelOffsetTracker
	for i := 0; i < 50; i++ {
		tr.Add(40, 39)
	}
	tr.Add(41, 40) // finalize: -1
	// Peer realigns (entry snap): healthy interval.
	for i := 0; i < 50; i++ {
		tr.Add(41, 41)
	}
	if v, changed := tr.Add(42, 42); !changed || v != 0 {
		t.Fatalf("recovery verdict = (%d, %v), want (0, true)", v, changed)
	}
}

func TestLabelOffsetTooFewFramesNoVerdict(t *testing.T) {
	var tr LabelOffsetTracker
	for i := 0; i < 5; i++ {
		tr.Add(50, 49)
	}
	tr.Add(51, 50) // roll with only 5 frames: no verdict
	if _, ok := tr.Verdict(); ok {
		t.Fatal("verdict must not finalize on too few frames")
	}
}

func TestLabelOffsetClampsExtremeDeltas(t *testing.T) {
	var tr LabelOffsetTracker
	for i := 0; i < 20; i++ {
		tr.Add(60, 100) // +40: clamped to +4
	}
	if v, changed := tr.Add(61, 61); !changed || v != labelOffsetRange {
		t.Fatalf("clamped verdict = (%d, %v), want (%d, true)", v, changed, labelOffsetRange)
	}
}
