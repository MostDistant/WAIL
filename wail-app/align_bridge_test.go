package main

import (
	"context"
	"testing"
	"time"
)

// fakeLinkBridge is the LinkBridgeInterface subset the align bridge touches:
// a scripted session tempo plus a real detector, so steering SetTempos can be
// watched for broadcast candidates.
type fakeLinkBridge struct {
	bpm float64
	det *TempoChangeDetector
}

func (f *fakeLinkBridge) Enable()                          {}
func (f *fakeLinkBridge) Disable()                         {}
func (f *fakeLinkBridge) SetTempo(bpm float64)             { f.bpm = bpm }
func (f *fakeLinkBridge) ForceBeat(float64, *int64)        {}
func (f *fakeLinkBridge) SnapGrid(int64)                   {}
func (f *fakeLinkBridge) TimeAtBeat(float64) int64         { return 0 }
func (f *fakeLinkBridge) State() LinkState                 { return LinkState{BPM: f.bpm} }
func (f *fakeLinkBridge) Detector() *TempoChangeDetector   { return f.det }
func (f *fakeLinkBridge) SpawnPoller(context.Context) (chan<- LinkCommand, <-chan LinkEvent) {
	return nil, nil
}

// pollFeeds n readings of the bridge's current tempo into the detector at
// linkPollInterval cadence starting from base, returning the first reported
// change (0 if none).
func pollFeeds(lb *fakeLinkBridge, base time.Time, n int) (float64, bool) {
	for i := 1; i <= n; i++ {
		if bpm, changed := lb.Detector().Check(lb.State().BPM, base.Add(time.Duration(i)*linkPollInterval)); changed {
			return bpm, true
		}
	}
	return 0, false
}

// TestSteeringSetTempoNeverBroadcasts: slew nudges, slew restores, and entry
// adoptions all flow through alignBridge.SetTempo. They must update the
// detector baseline so the hold-down never reports them as user intent.
// (The real LinkBridge.SetTempo has always done this sync; the seam now
// guarantees it for every bridge implementation — the linkstub bridge did
// not. Field note: the 2026-07-25 session logs exonerated the slew — no
// slew transient ever broadcast; the suspected ping-pong was two
// independent slews fighting a recurring δ bias, misread through the
// "Applied remote tempo" self-steering log line.)
func TestSteeringSetTempoNeverBroadcasts(t *testing.T) {
	lb := &fakeLinkBridge{bpm: 120, det: NewTempoChangeDetector(120)}
	ab := alignBridge{lb: lb}
	now := time.Now()

	// Slew nudge: the session tempo moves to 119.85 and holds past the
	// hold-down (60 polls × 20ms = 1.2s > tempoHoldDown). No change may be
	// reported.
	ab.SetTempo(119.85)
	if bpm, changed := pollFeeds(lb, now, 60); changed {
		t.Fatalf("slew nudge reported as a tempo change: %v BPM", bpm)
	}

	// Slew restore: same requirement.
	ab.SetTempo(120.0)
	if bpm, changed := pollFeeds(lb, now.Add(1200*time.Millisecond), 60); changed {
		t.Fatalf("slew restore reported as a tempo change: %v BPM", bpm)
	}

	// A genuine user change mid-slew must still broadcast: the baseline
	// follows steering, not the user.
	lb.bpm = 121
	bpm, changed := pollFeeds(lb, now.Add(2400*time.Millisecond), 60)
	if !changed || bpm != 121 {
		t.Fatalf("user tempo change during slew: changed=%v bpm=%v, want a 121 report", changed, bpm)
	}
}
