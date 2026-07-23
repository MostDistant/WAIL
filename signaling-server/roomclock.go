package main

import "math"

// The relay owns each room's interval clock (ADR-0003, CONTEXT.md pillar 5): it
// tracks the room tempo + interval config and derives the authoritative room
// interval index, which every client references so all peers agree on the index
// by construction.
//
// This is a deliberately small, self-contained copy of the anchor math in
// wail-app/internal/interval (the two live in separate Go modules). Kept tiny
// and unit-tested here so the server never divides by zero on a bad tempo.

const (
	minTempoBPM = 1.0
	minBars     = 1
	minQuantum  = 1.0
)

// intervalConfig is a room's interval shape: Bars × Quantum beats.
type intervalConfig struct {
	Bars    uint32
	Quantum float64
}

func (c intervalConfig) beatsPerInterval() float64 {
	bars := c.Bars
	if bars < minBars {
		bars = minBars
	}
	q := c.Quantum
	if q < minQuantum {
		q = minQuantum
	}
	return float64(bars) * q
}

func clampTempo(bpm float64) float64 {
	if !(bpm > minTempoBPM) { // also catches NaN
		return minTempoBPM
	}
	return bpm
}

// roomAnchor pins the room clock: at AtMicros the room was at interval Index,
// running at TempoBPM with Config. Broadcast to clients as an interval_anchor.
type roomAnchor struct {
	Index    int64
	AtMicros int64
	TempoBPM float64
	Config   intervalConfig
}

// roomClock derives room interval indices and boundary times from an anchor.
type roomClock struct {
	a roomAnchor
	// Transition pin: after a reanchor, the index is pinned to the interval
	// still in progress until the next boundary. Without it, a tempo increase
	// makes the new-tempo math report an index BEHIND the current one during
	// the transition window — the room index would tick backward and every
	// client labeler aligned from a broadcast in that window is off by one.
	pinIndex       int64
	pinUntilMicros int64 // 0 = no pin
}

func newRoomClock(a roomAnchor) *roomClock { return &roomClock{a: a} }

func (rc *roomClock) anchor() roomAnchor { return rc.a }

// indexAt returns the room interval index in effect at server-clock time now.
func (rc *roomClock) indexAt(nowMicros int64) int64 {
	if rc.pinUntilMicros > 0 && nowMicros < rc.pinUntilMicros {
		return rc.pinIndex
	}
	bpi := rc.a.Config.beatsPerInterval()
	elapsedSec := float64(nowMicros-rc.a.AtMicros) / 1e6
	elapsedBeats := elapsedSec * clampTempo(rc.a.TempoBPM) / 60.0
	k := rc.a.Index + int64(math.Floor(elapsedBeats/bpi))
	for rc.boundaryMicros(k) > nowMicros {
		k--
	}
	for rc.boundaryMicros(k+1) <= nowMicros {
		k++
	}
	return k
}

// boundaryMicros returns the server-clock time at which interval index begins.
func (rc *roomClock) boundaryMicros(index int64) int64 {
	bpi := rc.a.Config.beatsPerInterval()
	beatsFromAnchor := float64(index-rc.a.Index) * bpi
	secFromAnchor := beatsFromAnchor * 60.0 / clampTempo(rc.a.TempoBPM)
	return rc.a.AtMicros + int64(math.Round(secFromAnchor*1e6))
}

// reanchor applies a tempo/config change at the next interval boundary, so the
// current interval finishes under the old tempo (quantize tempo changes to
// boundaries, ADR-0003). The index is pinned until that boundary (see above).
func (rc *roomClock) reanchor(nowMicros int64, newTempoBPM float64, newConfig intervalConfig) {
	nextIdx := rc.indexAt(nowMicros) + 1
	nextBoundary := rc.boundaryMicros(nextIdx)
	rc.pinIndex = nextIdx - 1
	rc.pinUntilMicros = nextBoundary
	rc.a = roomAnchor{
		Index:    nextIdx,
		AtMicros: nextBoundary,
		TempoBPM: newTempoBPM,
		Config:   newConfig,
	}
}
