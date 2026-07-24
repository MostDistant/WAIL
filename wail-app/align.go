package main

// Grid alignment glue (ADR-0006): the session loop measures the local Link
// interval grid's alignment error δ against the relay's room clock, snaps on
// entry when misaligned, and slews tempo gently to close steady-state drift.
// The math lives in internal/interval (GridAligner, SlewTempo, WrapPhase);
// this file is the bridge-sampling glue and the gating policy.

import (
	"math"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

const (
	// alignSnapSettle is the post-entry settling window during which the slew
	// stays out of the way of the snap's aftershocks.
	alignSnapSettle = 5 * time.Second
	// alignTempoGate suppresses the slew after any tempo change (local user
	// or remote adoption) so WAIL never fights a hand on the tempo knob.
	alignTempoGate = 3 * time.Second
)

// measureDelta samples the local grid and returns δ in microseconds (positive
// = local grid runs late vs the room grid). ok is false until the aligner is
// Ready (anchor + relay offset) or when the local state is unusable.
func measureDelta(aligner *interval.GridAligner, link LinkBridgeInterface, bpi float64) (int64, bool) {
	if bpi <= 0 {
		return 0, false
	}
	st := link.State()
	if st.BPM <= 0 {
		return 0, false
	}
	// The local grid's next boundary in Link-clock µs: the beat lens is the
	// room BPI, so boundaries fall at multiples of bpi.
	boundaryBeat := (math.Floor(st.Beat/bpi) + 1) * bpi
	tEnd := link.TimeAtBeat(boundaryBeat)
	return aligner.Delta(tEnd)
}

// abs64 returns |v| for the δ comparisons below (int64; math.Abs is float64).
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// snapNeeded reports whether a measured δ justifies the entry snap.
func snapNeeded(deltaUs int64) bool {
	return abs64(deltaUs) > interval.AlignThresholdUs
}

// slewAllowed applies the slew gates: no steering in the settling window after
// an entry snap, and none shortly after any tempo change (local user's hand or
// a remote adoption — both re-anchor or re-tempo the grids anyway).
func slewAllowed(now, lastSnapAt, lastTempoAt time.Time) bool {
	if !lastSnapAt.IsZero() && now.Sub(lastSnapAt) < alignSnapSettle {
		return false
	}
	if !lastTempoAt.IsZero() && now.Sub(lastTempoAt) < alignTempoGate {
		return false
	}
	return true
}

// alignStateName buckets δ for the UI: aligned inside the deadband, aligning
// while the slew is working, drifted past the perceptual threshold.
func alignStateName(deltaUs int64) string {
	switch abs := abs64(deltaUs); {
	case abs > interval.AlignThresholdUs:
		return "drifted"
	case abs > interval.SlewDeadbandUs:
		return "aligning"
	default:
		return "aligned"
	}
}
