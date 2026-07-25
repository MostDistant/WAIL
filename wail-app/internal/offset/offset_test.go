package offset

import (
	"math"
	"testing"
)

// clickSeries appends clicks (10ms = half a frame of full energy) at the
// given abs frame positions on a silent background.
func clickSeries(tr *Tracker, frameDurFrames int, positions []int64) {
	maxAbs := int64(0)
	for _, p := range positions {
		if p > maxAbs {
			maxAbs = p
		}
	}
	energy := map[int64]float64{}
	for _, p := range positions {
		energy[p] = 9000
	}
	for abs := int64(0); abs <= maxAbs+10; abs++ {
		tr.Add(abs, energy[abs])
	}
}

func TestOffsetOnGridClicks(t *testing.T) {
	// Clicks every beat (25 frames at 20ms, 120bpm = 500ms) at phase 0.
	tr := NewTracker(0)
	var pos []int64
	for k := int64(0); k < 8; k++ {
		pos = append(pos, k*25)
	}
	clickSeries(tr, 1, pos)
	ms, ok := tr.Offset(20, 500)
	if !ok {
		t.Fatal("no offset for clear click series")
	}
	if math.Abs(ms) > 15 {
		t.Fatalf("on-grid clicks measured %+.1f ms, want ~0", ms)
	}
}

func TestOffsetLateClicks(t *testing.T) {
	// Same series shifted +88ms (+4.4 frames at 20ms).
	tr := NewTracker(0)
	var pos []int64
	for k := int64(0); k < 8; k++ {
		pos = append(pos, 4+k*25) // +80ms
	}
	clickSeries(tr, 1, pos)
	ms, ok := tr.Offset(20, 500)
	if !ok {
		t.Fatal("no offset for click series")
	}
	if ms < 60 || ms > 100 {
		t.Fatalf("shifted clicks measured %+.1f ms, want ~+80", ms)
	}
}

func TestOffsetSubdividedPattern(t *testing.T) {
	// Clicks every HALF beat at +100ms: phases ~0.21 and ~0.71 — the modal
	// phase must pick one cluster, not smear (both are "late" by 100ms;
	// accepting either is correct for offset measurement). Beat = 480ms
	// (24 frames) so the half-beat lands on whole frames.
	tr := NewTracker(0)
	var pos []int64
	for k := int64(0); k < 12; k++ {
		pos = append(pos, 5+k*12) // every 240ms starting at +100ms
	}
	clickSeries(tr, 1, pos)
	ms, ok := tr.Offset(20, 480)
	if !ok {
		t.Fatal("no offset for subdivided pattern")
	}
	// 100ms (0.208 beat) or -140ms (0.708 wrapped) are both correct reads.
	if !(math.Abs(ms-100) < 25 || math.Abs(ms+140) < 25) {
		t.Fatalf("subdivided pattern measured %+.1f ms, want 100 or -140", ms)
	}
}

func TestOffsetInsufficientContent(t *testing.T) {
	tr := NewTracker(0)
	tr.Add(0, 9000)
	tr.Add(25, 9000)
	if _, ok := tr.Offset(20, 500); ok {
		t.Fatal("two clicks must not yield an offset")
	}
	// Silence must not yield an offset either.
	tr2 := NewTracker(0)
	for i := int64(0); i < 200; i++ {
		tr2.Add(i, 0)
	}
	if _, ok := tr2.Offset(20, 500); ok {
		t.Fatal("silence must not yield an offset")
	}
}
