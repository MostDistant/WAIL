package interval

import "math"

// Anchor pins the room interval clock: at server-clock time AtMicros, the room
// was at interval Index, running at TempoBPM with the given Config. The relay
// server owns the authoritative anchor and broadcasts it; every client derives
// the same room index from it (ADR-0003).
//
// Deriving from an anchor (epoch + tempo) rather than a per-boundary "tick"
// broadcast means a client that misses messages still computes the right index
// the moment the next anchor arrives, and a late-joining client is correct
// immediately. This resolves migration-plan open question §70 in favour of
// epoch+tempo derivation.
type Anchor struct {
	Index    int64
	AtMicros int64
	TempoBPM float64
	Config   Config
}

// RoomClock derives room interval indices and boundary times from an Anchor.
// All times are in the anchor's clock domain (the relay server's clock); a
// client converts its local time into that domain using its measured server
// offset (see clock.go) before calling.
type RoomClock struct {
	a Anchor
}

// NewRoomClock creates a room clock from an anchor.
func NewRoomClock(a Anchor) *RoomClock { return &RoomClock{a: a} }

// Anchor returns the current anchor.
func (rc *RoomClock) Anchor() Anchor { return rc.a }

// SetAnchor replaces the anchor (e.g. a client receiving a fresh broadcast).
func (rc *RoomClock) SetAnchor(a Anchor) { rc.a = a }

// IndexAt returns the room interval index in effect at the given server-clock
// time: the unique k with BoundaryMicros(k) <= now < BoundaryMicros(k+1). The
// float estimate is corrected against exact integer boundary times so IndexAt is
// a precise inverse of BoundaryMicros (no off-by-one from µs quantization).
func (rc *RoomClock) IndexAt(nowMicros int64) int64 {
	bpi := rc.a.Config.BeatsPerInterval()
	elapsedSec := float64(nowMicros-rc.a.AtMicros) / 1e6
	elapsedBeats := elapsedSec * clampTempo(rc.a.TempoBPM) / 60.0
	k := rc.a.Index + int64(math.Floor(elapsedBeats/bpi))
	for rc.BoundaryMicros(k) > nowMicros {
		k--
	}
	for rc.BoundaryMicros(k+1) <= nowMicros {
		k++
	}
	return k
}

// BoundaryMicros returns the server-clock time at which the given interval index
// begins. Inverse of IndexAt at interval boundaries.
func (rc *RoomClock) BoundaryMicros(index int64) int64 {
	bpi := rc.a.Config.BeatsPerInterval()
	beatsFromAnchor := float64(index-rc.a.Index) * bpi
	secFromAnchor := beatsFromAnchor * 60.0 / clampTempo(rc.a.TempoBPM)
	return rc.a.AtMicros + int64(math.Round(secFromAnchor*1e6))
}

// Reanchor applies a tempo/config change at the *next* interval boundary, so the
// interval currently in progress finishes under the old tempo and the new tempo
// governs from the following interval onward (ADR-0003 / research: quantize tempo
// changes to boundaries, never mid-interval). The next boundary time is computed
// under the current (old) anchor; the resulting anchor is continuous with it.
func (rc *RoomClock) Reanchor(nowMicros int64, newTempoBPM float64, newConfig Config) {
	nextIdx := rc.IndexAt(nowMicros) + 1
	nextBoundary := rc.BoundaryMicros(nextIdx) // under the old tempo
	rc.a = Anchor{
		Index:    nextIdx,
		AtMicros: nextBoundary,
		TempoBPM: newTempoBPM,
		Config:   newConfig,
	}
}
