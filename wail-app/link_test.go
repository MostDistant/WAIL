package main

import (
	"math"
	"testing"
	"time"
)

func TestAboveThresholdEmitsChange(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.Check(122.0, now) // candidate
	bpm, changed := d.Check(122.0, now.Add(tempoHoldDown))
	if !changed || bpm != 122.0 {
		t.Fatalf("expected a change to 122 after hold-down, got %.4f changed=%v", bpm, changed)
	}
}

// Hundredths of a BPM are Link convergence noise, not intent. WAIL observes
// the session rather than owning the tempo, so the reporting bar sits above
// that noise — see tempoReportThreshold.
func TestBelowThresholdIgnored(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	for _, bpm := range []float64{120.005, 120.02, 120.1, 119.9} {
		d.Check(bpm, now)
		if _, changed := d.Check(bpm, now.Add(tempoHoldDown+time.Second)); changed {
			t.Fatalf("%.3f reported as a deliberate tempo change", bpm)
		}
	}
}

func TestEchoGuardSuppressesDetection(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.ArmEchoGuard(now.Add(150 * time.Millisecond))
	_, changed := d.Check(130.0, now)
	if changed {
		t.Fatal("should suppress during echo guard")
	}
	_, changed = d.Check(130.0, now.Add(100*time.Millisecond))
	if changed {
		t.Fatal("should still suppress at 100ms")
	}
}

func TestEchoGuardExpiresAllowsDetection(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.ArmEchoGuard(now.Add(150 * time.Millisecond))
	d.Check(130.0, now.Add(151*time.Millisecond)) // candidate after expiry
	bpm, changed := d.Check(130.0, now.Add(151*time.Millisecond+tempoHoldDown))
	if !changed || bpm != 130.0 {
		t.Fatal("should detect after guard expires and the value holds")
	}
}

func TestEchoGuardClearsAfterExpiry(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.ArmEchoGuard(now.Add(150 * time.Millisecond))
	t1 := now.Add(200 * time.Millisecond)
	d.Check(130.0, t1)
	d.Check(130.0, t1.Add(tempoHoldDown))
	t2 := t1.Add(tempoHoldDown)
	d.Check(140.0, t2)
	bpm, changed := d.Check(140.0, t2.Add(tempoHoldDown))
	if !changed || bpm != 140.0 {
		t.Fatal("guard should be cleared, second change should work")
	}
}

func TestLastTempoTracksAcrossChanges(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.Check(125.0, now)
	d.Check(125.0, now.Add(tempoHoldDown))
	if d.LastTempo() != 125.0 {
		t.Fatal("expected 125.0")
	}
	t1 := now.Add(tempoHoldDown)
	d.Check(130.0, t1)
	d.Check(130.0, t1.Add(tempoHoldDown))
	if d.LastTempo() != 130.0 {
		t.Fatal("expected 130.0")
	}
	d.Check(130.005, t1.Add(2*tempoHoldDown))
	if d.LastTempo() != 130.0 {
		t.Fatal("below threshold should not update baseline")
	}
}

func TestNaNBPMDoesNotPoisonDetector(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	d.SetLastTempo(math.NaN())
	d.Check(130.0, now)
	bpm, changed := d.Check(130.0, now.Add(tempoHoldDown))
	if !changed || bpm != 130.0 {
		t.Fatal("NaN should not poison detector — must still detect changes")
	}
}

func TestZeroBPMRejectedByDetector(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	_, changed := d.Check(0.0, now)
	if changed {
		t.Fatal("zero BPM should be rejected")
	}
	if d.LastTempo() != 120.0 {
		t.Fatal("baseline should not change")
	}
}

func TestNegativeBPMRejectedByDetector(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	now := time.Now()
	_, changed := d.Check(-120.0, now)
	if changed {
		t.Fatal("negative BPM should be rejected")
	}
	if d.LastTempo() != 120.0 {
		t.Fatal("baseline should not change")
	}
}

func TestSetLastTempoRejectsInvalid(t *testing.T) {
	d := NewTempoChangeDetector(120.0)
	d.SetLastTempo(math.NaN())
	if d.LastTempo() != 120.0 {
		t.Fatal("NaN should be rejected by SetLastTempo")
	}
	d.SetLastTempo(0.0)
	if d.LastTempo() != 120.0 {
		t.Fatal("zero should be rejected by SetLastTempo")
	}
	d.SetLastTempo(-50.0)
	if d.LastTempo() != 120.0 {
		t.Fatal("negative should be rejected by SetLastTempo")
	}
}

// A peer whose clock wanders around one intended tempo must not have each
// excursion reported to the room as a tempo change. WAIL never originates a
// tempo — it observes the Link session, which is the DAW's intent plus Link's
// convergence noise — and at the old 0.01 BPM bar a 119.9↔120 wobble dragged
// the whole room's tempo and left every peer's slew gated off.
func TestWanderingClockIsNotReportedAsATempoChange(t *testing.T) {
	d := NewTempoChangeDetector(120)
	now := time.Now()

	for i := 0; i < 600; i++ { // ten minutes of wobble at one reading a second
		bpm := 120.0
		if i%2 == 1 {
			bpm = 119.9
		}
		if got, ok := d.Check(bpm, now.Add(time.Duration(i)*time.Second)); ok {
			t.Fatalf("tick %d: wobble to %.2f reported as a tempo change (%.4f)", i, bpm, got)
		}
	}
}

// A real tempo change must still get through, and arrive as the value a human
// would have typed rather than the fraction Link happened to report.
func TestDeliberateTempoChangeIsReportedAndSnapped(t *testing.T) {
	now := time.Now()

	// 120 → ~124, observed slightly off as Link converges.
	d := NewTempoChangeDetector(120)
	d.Check(123.97, now)
	got, ok := d.Check(123.97, now.Add(tempoHoldDown+time.Second))
	if !ok {
		t.Fatal("a 4 BPM change was not reported")
	}
	if got != 124 {
		t.Fatalf("reported %.4f, want 124 — the room reference should carry the intended value", got)
	}

	// A genuinely fractional project tempo must survive untouched.
	d2 := NewTempoChangeDetector(120)
	d2.Check(128.5, now)
	got, ok = d2.Check(128.5, now.Add(tempoHoldDown+time.Second))
	if !ok {
		t.Fatal("a change to 128.5 was not reported")
	}
	if got != 128.5 {
		t.Fatalf("reported %.4f, want 128.5 left alone", got)
	}
}
