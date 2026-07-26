package abllink

import (
	"math"
	"testing"
	"time"
)

// TestMicroTempoConvergence pins the grid-cruise design's one hardware
// assumption: a Link session tracks sub-0.01%-scale tempo commits. The
// cruise clamp (SlewMaxFraction = 0.05%) holds the session at e.g. 120.06
// for seconds at a time; if Link's convergence rounded, rejected, or fought
// offsets that small, the clamp would be fiction. Two in-process instances
// need loopback multicast to form a session; skip when they can't
// (sandboxed CI).
func TestMicroTempoConvergence(t *testing.T) {
	a := New(120.0)
	defer a.Close()
	b := New(120.0)
	defer b.Close()

	a.Enable(true)
	defer a.Enable(false)
	b.Enable(true)
	defer b.Enable(false)

	deadline := time.Now().Add(5 * time.Second)
	for a.NumPeers() == 0 || b.NumPeers() == 0 {
		if time.Now().After(deadline) {
			t.Skip("no Link session formed between two local instances (multicast unavailable?)")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // session settle

	ssA := NewSessionState()
	defer ssA.Close()
	ssB := NewSessionState()
	defer ssB.Close()

	commitTempo := func(bpm float64) {
		a.CaptureAppSessionState(ssA)
		ssA.SetTempo(bpm, a.ClockMicros())
		a.CommitAppSessionState(ssA)
	}
	bTempo := func() float64 {
		b.CaptureAppSessionState(ssB)
		return ssB.Tempo()
	}
	awaitTempo := func(want float64, tol float64) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for {
			if got := bTempo(); math.Abs(got-want) <= tol {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("peer B tempo = %.5f, want %.5f±%.5f — Link did not track a micro commit", bTempo(), want, tol)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// The cruise clamp: +0.05% at 120 BPM.
	commitTempo(120.06)
	awaitTempo(120.06, 0.005)
	// And back to the exact room tempo (the deadband restore).
	commitTempo(120.0)
	awaitTempo(120.0, 0.005)
}
