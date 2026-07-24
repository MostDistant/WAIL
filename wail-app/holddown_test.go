package main

import (
	"testing"
	"time"
)

// Hold-down: a detected local tempo change is only reported after the new
// value holds continuously for tempoHoldDown. Link session convergence
// nudges (join merge, phase re-lock — up to ±2% for a handful of polls) are
// transient and must never reach the room; a human's knob turn persists.

func TestHoldDownReportsHeldChange(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	// Fresh change: candidate only, nothing reported.
	if _, changed := d.Check(122.0, now); changed {
		t.Fatal("change must not be reported instantly")
	}
	// Still holding, but inside the window.
	if _, changed := d.Check(122.0, now.Add(tempoHoldDown/2)); changed {
		t.Fatal("change must not be reported inside the hold-down window")
	}
	// Window elapsed with the value held: report it.
	bpm, changed := d.Check(122.0, now.Add(tempoHoldDown))
	if !changed || bpm != 122.0 {
		t.Fatalf("held change must be reported after the window, got %v %v", bpm, changed)
	}
}

func TestHoldDownDiscardsTransientNudge(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	// Link convergence passing through: 119.6 for a few polls, then 120.
	d.Check(119.6, now)
	d.Check(119.6, now.Add(20*time.Millisecond))
	d.Check(119.8, now.Add(40*time.Millisecond))
	d.Check(120.0, now.Add(60*time.Millisecond))
	// Long after, still no report — the nudge never held.
	if _, changed := d.Check(120.0, now.Add(10*tempoHoldDown)); changed {
		t.Fatal("transient convergence nudge must never be reported")
	}
	if got := d.LastTempo(); got != 120.0 {
		t.Fatalf("baseline moved to %v on a transient", got)
	}
}

func TestHoldDownSettleBackClearsCandidate(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.Check(122.0, now) // candidate
	// Settles back to the reported tempo before the window closes.
	if _, changed := d.Check(120.0, now.Add(tempoHoldDown/2)); changed {
		t.Fatal("settle-back must not report")
	}
	// A later steady 122 starts a NEW window (not the old one's remainder).
	if _, changed := d.Check(122.0, now.Add(tempoHoldDown)); changed {
		t.Fatal("candidate must reset when the reading settles back")
	}
}

func TestHoldDownConvergenceRampNeverAccepted(t *testing.T) {
	d := NewTempoChangeDetector(122.0)
	now := time.Now()
	// Gradual convergence 122 → 120 (each poll a different value, as Link
	// delivers while phase-locking): nothing may be reported mid-ramp…
	for i := 1; i <= 40; i++ { // 2s of ramp, 50ms polls
		bpm := 122.0 - 2.0*float64(i)/40.0
		if _, changed := d.Check(bpm, now.Add(time.Duration(i)*50*time.Millisecond)); changed {
			t.Fatalf("mid-ramp value reported at poll %d (%.2f)", i, bpm)
		}
	}
	// …but the settled consensus IS reported once it holds.
	if _, changed := d.Check(120.0, now.Add(2*time.Second)); changed {
		t.Fatal("still ramping/settled check must hold first")
	}
	bpm, changed := d.Check(120.0, now.Add(2*time.Second+tempoHoldDown))
	if !changed || bpm != 120.0 {
		t.Fatalf("settled convergence target must report after holding, got %v %v", bpm, changed)
	}
}

func TestHoldDownResetOnSetLastTempoAndEchoGuard(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.Check(122.0, now) // candidate
	d.SetLastTempo(122.0)
	// Baseline moved externally (remote apply): the stale candidate must not
	// later "complete" against an unrelated reading.
	if _, changed := d.Check(121.0, now.Add(2*tempoHoldDown)); changed {
		// 121 vs baseline 122 IS a change — but it must hold from scratch.
		t.Fatal("stale candidate survived SetLastTempo")
	}
}

func TestEchoGuardThenHoldDown(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.ArmEchoGuard(now.Add(150 * time.Millisecond))
	// Guard expired, but the hold-down still applies: a fresh change must
	// hold before reporting.
	if _, changed := d.Check(130.0, now.Add(200*time.Millisecond)); changed {
		t.Fatal("guard expiry must not skip the hold-down")
	}
	bpm, changed := d.Check(130.0, now.Add(200*time.Millisecond+tempoHoldDown))
	if !changed || bpm != 130.0 {
		t.Fatalf("held change after guard expiry must report, got %v %v", bpm, changed)
	}
}
