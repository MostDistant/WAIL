package main

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// fakeBridge is a minimal LinkBridgeInterface for alignment glue tests.
type fakeBridge struct {
	state       LinkState
	timeAtBeat  int64
	snapDeltaUs *int64
	setTempos   []float64
}

func (f *fakeBridge) Enable()                   {}
func (f *fakeBridge) Disable()                  {}
func (f *fakeBridge) State() LinkState          { return f.state }
func (f *fakeBridge) TimeAtBeat(float64) int64  { return f.timeAtBeat }
func (f *fakeBridge) SetTempo(bpm float64)      { f.setTempos = append(f.setTempos, bpm) }
func (f *fakeBridge) ForceBeat(float64, *int64) {}
func (f *fakeBridge) Detector() *TempoChangeDetector {
	return NewTempoChangeDetector(f.state.BPM)
}
func (f *fakeBridge) SnapGrid(deltaUs int64) {
	d := deltaUs
	f.snapDeltaUs = &d
}
func (f *fakeBridge) SpawnPoller(ctx context.Context) (chan<- LinkCommand, <-chan LinkEvent) {
	return SpawnLinkPoller(ctx, f)
}

func TestMeasureDeltaAlignedGrid(t *testing.T) {
	aligner := interval.NewGridAligner()
	// 120 BPM, 16 BPI → 8s period; anchor boundary at server 8s.
	aligner.SetAnchor(8_000_000, 120, 16)
	// Server runs 2s ahead of local link clock.
	aligner.ObserveServerTime(10_000_000, 8_000_000)

	// Local grid: beat 16 (next boundary) occurs at local 14s — the room
	// boundary mapped into local time (8s − 2s + 8s). Aligned.
	fb := &fakeBridge{state: LinkState{BPM: 120, Beat: 15.5, TimestampUs: 13_500_000}, timeAtBeat: 14_000_000}
	delta, ok := measureDelta(aligner, fb, 16)
	if !ok {
		t.Fatal("measureDelta not ok")
	}
	if delta != 0 {
		t.Fatalf("aligned grid δ = %d, want 0", delta)
	}
	if snapNeeded(delta) {
		t.Fatal("aligned grid must not trigger the entry snap")
	}
}

func TestMeasureDeltaLateGridSnaps(t *testing.T) {
	aligner := interval.NewGridAligner()
	aligner.SetAnchor(8_000_000, 120, 16)
	aligner.ObserveServerTime(10_000_000, 8_000_000)

	// Local next boundary 100ms after the room's: late by 100ms > 25ms threshold.
	fb := &fakeBridge{state: LinkState{BPM: 120, Beat: 15.5, TimestampUs: 13_500_000}, timeAtBeat: 14_100_000}
	delta, ok := measureDelta(aligner, fb, 16)
	if !ok {
		t.Fatal("measureDelta not ok")
	}
	if delta != 100_000 {
		t.Fatalf("δ = %d, want 100000", delta)
	}
	if !snapNeeded(delta) {
		t.Fatal("100ms misalignment must trigger the entry snap")
	}
}

func TestMeasureDeltaNotReady(t *testing.T) {
	aligner := interval.NewGridAligner() // no anchor, no offset
	fb := &fakeBridge{state: LinkState{BPM: 120, Beat: 15.5, TimestampUs: 13_500_000}, timeAtBeat: 14_000_000}
	if _, ok := measureDelta(aligner, fb, 16); ok {
		t.Fatal("aligner without anchor/offset must not report a delta")
	}
	if _, ok := measureDelta(aligner, fb, 0); ok {
		t.Fatal("zero BPI must not report a delta")
	}
}

func TestSlewGates(t *testing.T) {
	now := time.Now()
	// No history: allowed.
	if !slewAllowed(now, time.Time{}, time.Time{}) {
		t.Fatal("no history should allow slew")
	}
	// Within the post-snap settle window: blocked.
	if slewAllowed(now, now.Add(-time.Second), time.Time{}) {
		t.Fatal("settle window after snap should block slew")
	}
	// Within the tempo gate: blocked.
	if slewAllowed(now, time.Time{}, now.Add(-time.Second)) {
		t.Fatal("tempo gate should block slew")
	}
	// Everything old enough: allowed.
	if !slewAllowed(now, now.Add(-10*time.Second), now.Add(-10*time.Second)) {
		t.Fatal("settled snap + old tempo change should allow slew")
	}
}

func TestAlignStateName(t *testing.T) {
	if s := alignStateName(5_000); s != "aligned" {
		t.Fatalf("5ms = %q, want aligned", s)
	}
	if s := alignStateName(15_000); s != "aligning" {
		t.Fatalf("15ms = %q, want aligning", s)
	}
	if s := alignStateName(-100_000); s != "drifted" {
		t.Fatalf("-100ms = %q, want drifted", s)
	}
}

func TestSnapshotTempoAdopt(t *testing.T) {
	// No anchor (old server / clockless room): pre-anchor behavior — adopt any
	// differing tempo.
	if !snapshotTempoAdopt(0, false, 110, 120) {
		t.Fatal("no anchor: differing tempo should be adopted")
	}
	if snapshotTempoAdopt(0, false, 120, 120) {
		t.Fatal("same tempo is always a no-op")
	}
	// With an anchor: only anchor-matching snapshots are adopted.
	if !snapshotTempoAdopt(120, true, 120, 110) {
		t.Fatal("anchor-matching snapshot should be adopted")
	}
	if snapshotTempoAdopt(120, true, 110, 120) {
		t.Fatal("anchor-diverging snapshot must be ignored (oscillator)")
	}
}

// Two-peer snapshot oscillator (field report: 110↔120 flap every ~200ms with
// no DAWs involved). With naive adoption, crossing snapshots invert the pair
// every period forever; anchor-gated adoption converges on the room tempo.
func TestSnapshotAdoptionOscillator(t *testing.T) {
	// One exchange of crossing snapshots: each side may adopt the other's
	// tempo as of before the exchange.
	step := func(a, b float64, adopt func(msg, local float64) bool) (float64, float64) {
		na, nb := a, b
		if adopt(b, a) {
			na = b
		}
		if adopt(a, b) {
			nb = a
		}
		return na, nb
	}

	naive := func(msg, local float64) bool { return math.Abs(msg-local) > tempoChangeThreshold }
	a, b := 120.0, 110.0
	a, b = step(a, b, naive)
	if a != 110 || b != 120 {
		t.Fatalf("naive step 1: a=%v b=%v, want inverted 110/120", a, b)
	}
	a, b = step(a, b, naive)
	if a != 120 || b != 110 {
		t.Fatalf("naive step 2: a=%v b=%v, want re-inverted 120/110 (oscillation)", a, b)
	}

	gated := func(msg, local float64) bool { return snapshotTempoAdopt(120, true, msg, local) }
	a, b = 120.0, 110.0
	a, b = step(a, b, gated)
	if a != 120 || b != 120 {
		t.Fatalf("gated: a=%v b=%v, want both converged to anchor 120", a, b)
	}
	// And stable once converged (B's stale 110 snapshots keep being ignored).
	a, b = step(a, b, func(msg, local float64) bool {
		return snapshotTempoAdopt(120, true, msg, local)
	})
	if a != 120 || b != 120 {
		t.Fatalf("gated steady state drifted: a=%v b=%v", a, b)
	}
}
