package abllink

import (
	"math"
	"testing"
	"time"
)

// TestIntervalQuantumPhaseSharing proves the two Link contracts WAIL's interval
// grid rests on (the audio engine asks Link for beats at quantum = BPI):
//
//  1. Peers in a session agree on beat phase at any shared quantum — including
//     a multi-bar interval quantum like 16. (Asking at the bar quantum only
//     pins beat mod 4, leaving which-bar-of-the-interval per-peer arbitrary —
//     the bar-aligned-but-interval-misaligned bug.)
//  2. Quantum grids nest: a zero-phase instant at quantum 16 is also zero-phase
//     at quantum 4, so interval boundaries stay bar-aligned and coincide with a
//     DAW's interval-length launch-quantization grid (ADR-0004).
//
// Two in-process instances need loopback multicast to form a session; skip when
// they can't (sandboxed CI).
func TestIntervalQuantumPhaseSharing(t *testing.T) {
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
	// Let the session settle its shared timeline after discovery.
	time.Sleep(500 * time.Millisecond)

	ssA := NewSessionState()
	defer ssA.Close()
	ssB := NewSessionState()
	defer ssB.Close()
	a.CaptureAppSessionState(ssA)
	b.CaptureAppSessionState(ssB)

	const (
		bpi       = 16.0 // 4 bars × 4 beats
		bar       = 4.0
		tolerance = 0.05 // beats; 25ms at 120 BPM
	)
	now := a.ClockMicros() // one host clock; comparable across instances

	// (1) Cross-instance phase agreement at the interval quantum.
	pa := ssA.PhaseAtTime(now, bpi)
	pb := ssB.PhaseAtTime(now, bpi)
	if d := cyclicDist(pa, pb, bpi); d > tolerance {
		t.Fatalf("phase(quantum=%v) disagrees across session peers: %v vs %v (cyclic dist %v)", bpi, pa, pb, d)
	}
	// And the beat/phase relationship the engine relies on: beat mod bpi == phase.
	beatA := ssA.BeatAtTime(now, bpi)
	if beatA >= 0 {
		if d := cyclicDist(math.Mod(beatA, bpi), pa, bpi); d > tolerance {
			t.Fatalf("beat mod quantum (%v) != phase (%v)", math.Mod(beatA, bpi), pa)
		}
	}

	// (2) Nesting: the next quantum-16 boundary is also a quantum-4 boundary.
	nextBoundary := math.Ceil(beatA/bpi) * bpi
	if nextBoundary <= beatA {
		nextBoundary += bpi
	}
	tb := ssA.TimeAtBeat(nextBoundary, bpi)
	if p := ssA.PhaseAtTime(tb, bar); cyclicDist(p, 0, bar) > tolerance {
		t.Fatalf("quantum-%v boundary is not bar-aligned: phase(quantum=%v) = %v", bpi, bar, p)
	}
}

// cyclicDist is the distance between two phases on a cycle of length q.
func cyclicDist(x, y, q float64) float64 {
	d := math.Mod(math.Abs(x-y), q)
	if d > q/2 {
		d = q - d
	}
	return d
}
