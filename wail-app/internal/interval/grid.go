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
	// SlewDeadbandUs is the |δ| above which the slew fires (below it the grid
	// is aligned enough and the loop rests).
	SlewDeadbandUs = 10_000
	// SlewSettleUs is the |δ| below which an ACTIVE slew restores the room
	// tempo — deliberately tighter than SlewDeadbandUs (hysteresis): episodes
	// settle deep instead of stopping at the deadband edge and re-firing when
	// skew walks δ back out (ss3 field pattern: settle at −9.1 ms re-fired
	// every ~40 s).
	SlewSettleUs = 5_000
	// SlewMaxFraction caps the steady-state tempo nudge: 0.05% = 0.86 cents,
	// below the pitch JND even for trained ears on isolated sustained tones
	// (~1–3 cents) — inaudible, full stop. The old 0.3% cap (5.2 cents) was
	// NOT inaudible: the 2026-07-25 field session heard every slew episode,
	// and a post-tempo-change aftershock slewed for 7s at the cap. Because
	// the cap sits below the proportional rate for any |δ| past the 10ms
	// deadband on periods under 20s, the slew is effectively a fixed
	// micro-nudge in practice: on when |δ| > deadband, off inside — closing
	// 4ms per second per active tick on an 8s period.
	SlewMaxFraction = 0.0005
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
// server↔local clock offset, and computes the local grid's alignment error δ
// from locally sampled clocks. Not safe for concurrent use — the session loop
// owns it.
type GridAligner struct {
	// Anchor: the server-clock time of the boundary that ends the anchor's
	// CurrentIndex, plus the interval period at the room tempo.
	nextBoundaryServerUs int64
	periodUs             float64
	bpm                  float64
	haveAnchor           bool

	// server − local clock offset, taken from the LOWEST-RTT sample (NTP's
	// minimum-filter principle: network buffering only ever ADDS delay, so
	// the cleanest sample is the most accurate; median filters let a single
	// stalled pong — laptop sleep, Wi-Fi stall — poison the estimate,
	// especially at join when few samples exist). Samples within 1.5× of the
	// best RTT are also accepted so the offset can track slow clock drift.
	offsetUs      int64
	bestRttUs     int64
	offsetSamples int
	haveOffset    bool
	lastLocalUs   int64 // local clock at the previous sample (dt for the slew cap)
}

// minOffsetSamples is how many server pongs must arrive before the aligner is
// Ready. One wild sample must never be enough to run entry conformance or a
// label derivation (field finding: a stalled first pong put a joiner's offset
// out by seconds — intervals of label error, frozen for the session).
const minOffsetSamples = 3

const (
	// offsetFreeSamples is the bootstrap window: the first N samples move the
	// estimate freely (min-RTT selection), so entry conformance behaves as it
	// always has. The slew cap engages afterwards.
	offsetFreeSamples = 8
	// maxOffsetSlewPpm caps the post-bootstrap offset tracking rate. Real
	// clock offsets only DRIFT (crystal skew, ≤ ~100ppm in practice); they
	// never teleport. A candidate further than this rate allows is converged
	// toward, not jumped to (field finding, 2026-07-26 Australia VPN: jittery
	// min-RTT re-selections jumped the estimate ±70ms and the grid slew
	// chased every jump — remote audio flammed by up to ~100ms).
	maxOffsetSlewPpm = 500
	// offsetSlewMarginUs absorbs measurement quantization on top of the rate.
	offsetSlewMarginUs = 500
)

// NewGridAligner creates an empty aligner (not Ready until both an anchor and
// an offset sample arrive).
func NewGridAligner() *GridAligner { return &GridAligner{} }

// ObserveServerTime folds one server-time measurement into the offset
// estimate. serverNowEstUs is the server's clock reading estimated at the
// local receipt instant (server stamp + RTT/2); localNowUs is the local clock
// sampled at the same instant; rttUs is the measured round trip. The offset
// comes from the lowest-RTT sample (buffering only adds delay); samples
// within 1.5× of the best RTT also update it, so slow clock drift tracks.
// After the bootstrap window, updates are slew-capped: the estimate converges
// toward faraway candidates at a plausible clock-skew rate instead of jumping
// (offsets drift; they never teleport).
func (g *GridAligner) ObserveServerTime(serverNowEstUs, localNowUs, rttUs int64) {
	sample := serverNowEstUs - localNowUs
	g.offsetSamples++
	if !g.haveOffset || rttUs <= g.bestRttUs+g.bestRttUs/2 {
		if !g.haveOffset || rttUs < g.bestRttUs {
			g.bestRttUs = rttUs
		}
		if g.haveOffset && g.offsetSamples > offsetFreeSamples {
			dt := localNowUs - g.lastLocalUs
			if dt < 0 {
				dt = 0
			}
			maxStep := dt*maxOffsetSlewPpm/1e6 + offsetSlewMarginUs
			if d := sample - g.offsetUs; d > maxStep {
				sample = g.offsetUs + maxStep
			} else if d < -maxStep {
				sample = g.offsetUs - maxStep
			}
		}
		g.offsetUs = sample
		g.haveOffset = true
	}
	g.lastLocalUs = localNowUs
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

// Ready reports whether both an anchor and enough offset samples have
// arrived — until then no δ can be computed and neither snap nor slew may run.
func (g *GridAligner) Ready() bool {
	return g.haveAnchor && g.haveOffset && g.offsetSamples >= minOffsetSamples
}

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
