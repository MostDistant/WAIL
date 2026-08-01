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
	// Every frame labeled one behind the local room index, held across two
	// intervals — one alone is a blip (see the hold-down).
	for i := 0; i < 50; i++ {
		tr.Add(20, 19)
	}
	for i := 0; i < 50; i++ {
		tr.Add(21, 20)
	}
	v, changed := tr.Add(22, 21)
	if !changed || v != -1 {
		t.Fatalf("lagging peer verdict = (%d, %v), want (-1, true)", v, changed)
	}
}

func TestLabelOffsetVerdictChangeReportedOnce(t *testing.T) {
	var tr LabelOffsetTracker
	for i := 0; i < 50; i++ {
		tr.Add(30, 29)
	}
	for i := 0; i < 50; i++ {
		tr.Add(31, 30)
	}
	if _, changed := tr.Add(32, 31); !changed { // confirmed across two intervals: -1
		t.Fatal("a persistent -1 was never reported")
	}
	// Still lagging: no new verdict to report, however long it goes on.
	for i := 0; i < 50; i++ {
		if _, changed := tr.Add(32, 31); changed {
			t.Fatal("verdict re-reported without change")
		}
	}
	if _, changed := tr.Add(33, 32); changed {
		t.Fatal("same verdict (-1) must not re-report")
	}
}

func TestLabelOffsetRecovery(t *testing.T) {
	var tr LabelOffsetTracker
	for i := 0; i < 50; i++ {
		tr.Add(40, 39)
	}
	for i := 0; i < 50; i++ {
		tr.Add(41, 40)
	}
	if v, changed := tr.Add(42, 41); !changed || v != -1 {
		t.Fatalf("setup: want a reported -1 to recover from, got (%d, %v)", v, changed)
	}
	// Peer realigns (entry snap): healthy interval. Recovery is not held down —
	// leaving a stale fault on the books is worse than reporting one early.
	for i := 0; i < 50; i++ {
		tr.Add(42, 42)
	}
	if v, changed := tr.Add(43, 43); !changed || v != 0 {
		t.Fatalf("recovery verdict = (%d, %v), want (0, true)", v, changed)
	}
}

// The join transition produces exactly this: one interval where the stream's
// labels and our room index disagree while the labeler is still settling.
// Reporting it claims a peer is playing intervals late when nothing is wrong,
// and the operator has no way to tell that from the real thing.
func TestLabelOffsetIgnoresASingleIntervalBlip(t *testing.T) {
	var tr LabelOffsetTracker
	for i := 0; i < 50; i++ {
		tr.Add(70, 71) // one odd interval: labels read +1
	}
	if v, changed := tr.Add(71, 71); changed && v != 0 {
		t.Fatalf("a one-interval blip reported %+d — join settling reads as a mislabeled peer", v)
	}
	// ...and the stream is healthy from here on, so nothing nonzero ever lands.
	for i := 0; i < 50; i++ {
		if v, changed := tr.Add(71, 71); changed && v != 0 {
			t.Fatalf("blip reported late as %+d", v)
		}
	}
	if v, changed := tr.Add(72, 72); changed && v != 0 {
		t.Fatalf("blip reported at the next roll as %+d", v)
	}
	if v, ok := tr.Verdict(); !ok || v != 0 {
		t.Fatalf("Verdict = (%d, %v), want (0, true)", v, ok)
	}
}

// The flip side: a genuinely mislabeled peer must still be reported, one
// interval later than before. Anything that suppresses the blip has to leave
// this working, or the tracker stops earning its keep.
func TestLabelOffsetReportsAPersistentOffset(t *testing.T) {
	var tr LabelOffsetTracker
	for i := 0; i < 50; i++ {
		tr.Add(80, 82)
	}
	if _, changed := tr.Add(81, 83); changed {
		t.Fatal("reported on the first interval — that is the blip case")
	}
	for i := 0; i < 50; i++ {
		tr.Add(81, 83)
	}
	if v, changed := tr.Add(82, 84); !changed || v != 2 {
		t.Fatalf("persistent offset = (%d, %v), want (2, true)", v, changed)
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
	for i := 0; i < 20; i++ {
		tr.Add(61, 101)
	}
	if v, changed := tr.Add(62, 62); !changed || v != labelOffsetRange {
		t.Fatalf("clamped verdict = (%d, %v), want (%d, true)", v, changed, labelOffsetRange)
	}
}
