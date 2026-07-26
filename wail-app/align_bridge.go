package main

import "github.com/nicholasgasior/wail/wail-app/internal/align"

// alignBridge adapts the Link bridge to the grid steer's narrow LinkGrid
// seam (internal/align), translating the main-package LinkState. Three of
// the four capabilities forward verbatim; SetTempo additionally marks the
// move as steering in the tempo detector (see below).
type alignBridge struct{ lb LinkBridgeInterface }

func (a alignBridge) State() align.State {
	st := a.lb.State()
	return align.State{BPM: st.BPM, Beat: st.Beat, TimestampUs: st.TimestampUs}
}

func (a alignBridge) TimeAtBeat(beat float64) int64 { return a.lb.TimeAtBeat(beat) }

// SetTempo applies a steering tempo move (slew nudge, slew restore, entry
// adoption) and moves the detector baseline with it: steering is never user
// intent, so it must never survive the hold-down and broadcast. The real
// LinkBridge.SetTempo already does this sync; the linkstub bridge did NOT —
// doing it at the seam makes the property hold for every bridge
// implementation (and keeps the next one from forgetting it). A genuine
// user change mid-slew still diverges from the new baseline and broadcasts
// normally. Order matters: set-then-baseline, so a poller read mid-flight
// can only form a candidate the baseline update immediately clears.
func (a alignBridge) SetTempo(bpm float64) {
	a.lb.SetTempo(bpm)
	a.lb.Detector().SetLastTempo(bpm)
}

func (a alignBridge) SnapGrid(deltaUs int64) { a.lb.SnapGrid(deltaUs) }
