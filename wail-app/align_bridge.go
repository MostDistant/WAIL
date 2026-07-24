package main

import "github.com/nicholasgasior/wail/wail-app/internal/align"

// alignBridge adapts the Link bridge to the grid steer's narrow LinkGrid
// seam (internal/align), translating the main-package LinkState. It adds no
// behavior — the steerer's four capabilities forward verbatim.
type alignBridge struct{ lb LinkBridgeInterface }

func (a alignBridge) State() align.State {
	st := a.lb.State()
	return align.State{BPM: st.BPM, Beat: st.Beat, TimestampUs: st.TimestampUs}
}

func (a alignBridge) TimeAtBeat(beat float64) int64 { return a.lb.TimeAtBeat(beat) }
func (a alignBridge) SetTempo(bpm float64)           { a.lb.SetTempo(bpm) }
func (a alignBridge) SnapGrid(deltaUs int64)         { a.lb.SnapGrid(deltaUs) }
