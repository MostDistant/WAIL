package interval

// Grid alignment (ADR-0006): the relay's room clock is the single fixed
// reference; every peer measures its alignment error δ (the phase distance
// between its local Link interval grid and the room grid) against it, snaps on
// entry when δ exceeds the perceptual threshold, and slews tempo gently to
// close steady-state drift. Peers never measure each other — alignment is
// transitive through the relay, so there is no feedback path to oscillate.
//
// This file is pure math: callers sample the clocks (Link session state,
// server-stamped pongs) and hand in microsecond readings.

import "math"

const (
	// AlignThresholdUs is the |δ| above which alignment is acted on (~25 ms:
	// below this, flamming on tight unison material is inaudible).
	AlignThresholdUs = 25_000
	// SlewDeadbandUs is the |δ| below which the slew considers the grid
	// aligned and restores the exact room tempo (hysteresis: the loop settles
	// instead of hunting around zero).
	SlewDeadbandUs = 10_000
	// SlewMaxFraction caps the steady-state tempo nudge (0.3% — inaudible).
	SlewMaxFraction = 0.003
	// gridOffsetWindow is the median-filter width for server↔local offset samples.
	gridOffsetWindow = 8
)

// WrapPhase wraps a phase difference to the range (-period/2, period/2], so a
// grid disagreement is always expressed as the shorter of "late" or "early".
// A non-positive period (defensive) yields 0.
func WrapPhase(deltaUs, periodUs int64) int64 {
	if periodUs <= 0 {
		return 0
	}
	d := deltaUs % periodUs
	if d > periodUs/2 {
		d -= periodUs
	}
	if d <= -periodUs/2 {
		d += periodUs
	}
	return d
}

// GridAligner tracks the room grid (in the relay's clock domain) plus a
// filtered server↔local clock offset, and computes the local grid's alignment
// error δ from locally sampled clocks. Not safe for concurrent use — the
// session loop owns it.
type GridAligner struct {
	// Anchor: the server-clock time of the boundary that ends the anchor's
	// CurrentIndex, plus the interval period at the room tempo.
	nextBoundaryServerUs int64
	periodUs             float64
	bpm                  float64
	haveAnchor           bool

	// server − local clock offset, median-filtered. "Local" is whatever
	// monotonic clock domain the caller samples (the Link clock).
	offsetSamples []int64
	offsetUs      int64
	haveOffset    bool
}

// NewGridAligner creates an empty aligner (not Ready until both an anchor and
// an offset sample arrive).
func NewGridAligner() *GridAligner { return &GridAligner{} }

// ObserveServerTime folds one server-time measurement into the offset filter.
// serverNowEstUs is the server's clock reading estimated at the local receipt
// instant (server stamp + RTT/2); localNowUs is the local clock sampled at the
// same instant. The offset is server − local.
func (g *GridAligner) ObserveServerTime(serverNowEstUs, localNowUs int64) {
	sample := serverNowEstUs - localNowUs
	g.offsetSamples = append(g.offsetSamples, sample)
	if len(g.offsetSamples) > gridOffsetWindow {
		g.offsetSamples = g.offsetSamples[1:]
	}
	g.offsetUs = medianOfSamples(g.offsetSamples)
	g.haveOffset = true
}

// SetAnchor records the room grid from an interval_anchor: the server-clock
// time of the boundary ending CurrentIndex, and the period derived from the
// anchor's tempo and config. Fields absent on old servers (zero values) are
// rejected, leaving the aligner un-Ready — graceful degradation to label-only.
func (g *GridAligner) SetAnchor(nextBoundaryServerUs int64, bpm, beatsPerInterval float64) {
	if nextBoundaryServerUs <= 0 || bpm <= 0 || beatsPerInterval <= 0 {
		return
	}
	g.nextBoundaryServerUs = nextBoundaryServerUs
	g.periodUs = beatsPerInterval * 60.0 / bpm * 1e6
	g.bpm = bpm
	g.haveAnchor = true
}

// Ready reports whether both an anchor and at least one offset sample have
// arrived — until then no δ can be computed and neither snap nor slew may run.
func (g *GridAligner) Ready() bool { return g.haveAnchor && g.haveOffset }

// RoomBPM returns the anchor's tempo (valid once SetAnchor accepted one).
func (g *GridAligner) RoomBPM() (float64, bool) {
	if !g.haveAnchor {
		return 0, false
	}
	return g.bpm, true
}

// OffsetUs returns the filtered server − local offset (valid once
// ObserveServerTime has been called). Exposed for tests and diagnostics.
func (g *GridAligner) OffsetUs() (int64, bool) {
	if !g.haveOffset {
		return 0, false
	}
	return g.offsetUs, true
}

// AnchorBoundary returns the server-clock time of the boundary that ends the
// anchor's CurrentIndex (valid once SetAnchor accepted an anchor). With the
// OffsetUs mapping this pins the room grid in the local clock domain, so the
// local→room index mapping is derivable by construction instead of by
// sampling (ADR-0006: a sample align taken before a grid snap is off by one
// whenever the snap moves the sampling instant across a boundary).
func (g *GridAligner) AnchorBoundary() (int64, bool) {
	if !g.haveAnchor {
		return 0, false
	}
	return g.nextBoundaryServerUs, true
}

// PeriodUs returns the anchor's interval period in whole microseconds (valid
// once SetAnchor accepted an anchor).
func (g *GridAligner) PeriodUs() (int64, bool) {
	if !g.haveAnchor {
		return 0, false
	}
	return int64(math.Round(g.periodUs)), true
}

// Delta returns how LATE the local grid runs relative to the room grid, in
// microseconds, wrapped to ±period/2: positive means local boundaries occur
// after the corresponding room boundaries (the local grid must advance to
// close the error). localNextBoundaryUs is the local grid's next boundary in
// the same clock domain passed to ObserveServerTime. ok is false until Ready.
func (g *GridAligner) Delta(localNextBoundaryUs int64) (int64, bool) {
	if !g.Ready() {
		return 0, false
	}
	period := int64(math.Round(g.periodUs))
	if period <= 0 {
		return 0, false
	}
	// Room boundaries in the local domain: anchorLocal + k·period.
	anchorLocal := g.nextBoundaryServerUs - g.offsetUs
	k := math.Round(float64(localNextBoundaryUs-anchorLocal) / g.periodUs)
	roomNearest := anchorLocal + int64(math.Round(k*g.periodUs))
	return WrapPhase(localNextBoundaryUs-roomNearest, period), true
}

// SlewTempo maps δ to the tempo to apply this tick: inside the deadband it
// returns the exact room tempo with active=false (restore and rest); outside
// it nudges the room tempo toward closing δ, proportional to δ/period and
// clamped to SlewMaxFraction. Positive δ (local late) speeds the local grid up.
func SlewTempo(roomBPM float64, deltaUs, periodUs int64) (target float64, active bool) {
	if roomBPM <= 0 || periodUs <= 0 {
		return roomBPM, false
	}
	abs := deltaUs
	if abs < 0 {
		abs = -abs
	}
	if abs <= SlewDeadbandUs {
		return roomBPM, false
	}
	frac := float64(abs) / float64(periodUs)
	if frac > SlewMaxFraction {
		frac = SlewMaxFraction
	}
	if deltaUs > 0 {
		return roomBPM * (1 + frac), true
	}
	return roomBPM * (1 - frac), true
}

func medianOfSamples(s []int64) int64 {
	if len(s) == 0 {
		return 0
	}
	cp := make([]int64, len(s))
	copy(cp, s)
	// insertion sort — window is tiny (8)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	return cp[len(cp)/2]
}
