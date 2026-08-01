// Package align implements the grid steer (CONTEXT.md, ADR-0006): the module
// that owns WAIL's active grid alignment end to end — entry conformance
// (adopt room tempo, measure δ, snap when misaligned), the gated steady-state
// grid slew, snapshot-tempo arbitration, and the committed-tempo record.
//
// The Steerer drives the GridAligner math (internal/interval) and the Link
// bridge behind the narrow LinkGrid seam; the session loop only forwards
// events to it (anchor, server pong, tempo commits, a per-tick slew driver)
// and never sees alignment state — entry pending, gate timestamps, and the
// slew target are all internal. Time flows in as a parameter so the gates
// are deterministically testable.
package align

import (
	"math"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

const (
	// snapSettle is the post-entry settling window during which the slew
	// stays out of the way of the snap's aftershocks.
	snapSettle = 5 * time.Second
	// gateWedgeTimeout is how long the tempo-settling gate may stay shut before
	// the steerer says so. Long enough to sit out an ordinary tempo change and
	// its re-anchor, so the ordinary case stays quiet.
	gateWedgeTimeout = 10 * time.Second
	// tempoGate suppresses the slew after any tempo commit (local user's
	// hand, remote change, or entry adoption) so WAIL never fights a hand
	// on the tempo knob. Deliberately longer than the 150ms echo guard.
	tempoGate = 3 * time.Second
	// tempoThreshold is the equality epsilon for adoption decisions: below it
	// two tempos are "the same". It matches the detector's tempoSteadyBand
	// (link_types.go), which asks a different question — has a reading stopped
	// moving — and is deliberately NOT the detector's reporting bar, which is
	// far coarser. It used to be described as mirroring a single shared
	// constant; that constant was split, and the mirroring stopped being true.
	tempoThreshold = 0.01
	// slewPersistenceTicks is how many consecutive ticks δ must hold outside
	// the deadband in the same direction before the slew acts. Real grid
	// drift persists for minutes; measurement spikes (a teleporting offset
	// estimate over a jittery WAN path — 2026-07-26 Australia VPN) last a
	// tick or two and often alternate sign. Chasing spikes flapped the tempo
	// every two seconds in the field. Settling stays immediate — restoring
	// the room tempo is always safe.
	slewPersistenceTicks = 2
	// emitDeltaUs is how far δ must move inside one state bucket before the
	// UI is told again. Below it the number is jitter; above it the displayed
	// error is stale enough to mislead. The UI only shows the figure past
	// 10 ms, so this is the granularity that reads as "live".
	emitDeltaUs = 10_000
	// emitMinInterval bounds same-bucket reports. Without it the step above is
	// no bound at all for an oscillating δ.
	emitMinInterval = 3 * time.Second
)

// State is the slice of Link session state the steerer samples. Beat is
// phase-encoded at the room BPI (the interval-quantum lens, ADR-0003).
type State struct {
	BPM float64
	// MeanBPM is the session tempo averaged over the detector's window. The
	// tempo-settling gate judges this rather than BPM: a peer whose clock
	// wanders 119.9↔120 has a mean of 119.95 — inside the slew's authority —
	// so drift correction keeps running through the wobble instead of stopping
	// dead on every excursion. Zero falls back to BPM (bridges that do not
	// track a mean, and the first ticks before any sample).
	MeanBPM     float64
	Beat        float64
	TimestampUs int64
}

// settlingBPM is the reading the tempo-settling gate judges: the windowed mean
// when the bridge supplies one, else the instantaneous tempo.
func (s State) settlingBPM() float64 {
	if s.MeanBPM > 0 {
		return s.MeanBPM
	}
	return s.BPM
}

// LinkGrid is the narrow Link bridge seam the steerer needs — the four
// capabilities grid alignment depends on, nothing more. *LinkBridge
// satisfies it via a thin adapter in package main (LinkState translation).
type LinkGrid interface {
	State() State
	// TimeAtBeat returns the Link-clock time at which the given
	// interval-quantum phase-encoded beat occurs (grid boundary math).
	TimeAtBeat(beat float64) int64
	SetTempo(bpm float64)
	// SnapGrid shifts the local interval grid earlier by deltaUs (positive =
	// local grid runs late). Entry conformance only, never steady state.
	SnapGrid(deltaUs int64)
}

// Steerer owns the ADR-0006 surface: entry conformance, the gated grid slew,
// snapshot-tempo arbitration, the enable/disable semantics, the
// committed-tempo record (the single home of "what tempo the session last
// committed to, and when" — the slew's tempo gate), and the post-snap
// room-label re-derivation. Not safe for concurrent use; the session loop
// owns it.
type Steerer struct {
	link LinkGrid
	emit func(state string, errMs float64)
	logf func(format string, args ...any)
	// alignRoomLabel applies a by-construction labeler alignment (the
	// engine's AlignRoomLabel): the local interval ending at the anchor's
	// boundary corresponds to the anchor's index, exact on an aligned grid.
	alignRoomLabel func(roomIndex, localIndex int64)
	// onSnapGrid fires after an entry-conformance snap so the audio engine
	// can re-anchor its emit feeders: the snap moved the playhead, not the
	// audio — the jumped frames must skip silently, never count as underruns.
	onSnapGrid func(deltaUs int64)

	aligner      *interval.GridAligner
	enabled      bool
	entryPending bool
	lastSnapAt   time.Time
	lastTempoAt  time.Time
	// gateShutSince is when the tempo-settling gate last started blocking, zero
	// while it is open — the deferral timer (see noteGateShut).
	gateShutSince time.Time
	// gateOpen is the gate's current side, so its band can be hysteretic: it
	// takes the full band to shut and 0.9x to reopen, and a tempo hovering at
	// the edge cannot chatter.
	gateOpen bool
	// episodeBase is the tempo the session was observed at when the current slew
	// episode began — the tempo every nudge is measured against and the tempo
	// restored on settle. Zero when no episode is in flight. Keeping the base as
	// an observation rather than deriving targets from the room tempo is what
	// makes "the slew never overwrites a deliberate change" a property of the
	// writer instead of an accident of threshold spacing (ADR-0009).
	episodeBase      float64
	slewTarget       float64 // 0 = no episode in flight
	slewDir          int     // sign of the active episode's δ (0 = not slewing)
	slewPendingDir   int     // sign of the δ being confirmed (0 = none)
	slewPendingCount int     // consecutive same-direction ticks past the deadband
	lastState        string
	lastEmittedUs    int64     // δ at the last emit — see emitState
	lastEmitAt       time.Time // when that emit happened (same-bucket rate limit)
	lastJumpEntryAt  time.Time // last jump-triggered re-entry (snaps are audible)
	currentBPM       float64
	anchorIndex      int64
	haveRoomAnchor   bool
}

// NewSteerer creates a steerer with entry conformance armed (every session
// start is an entry). initialBPM seeds the committed-tempo record. emit
// reports alignment state changes ("aligned"/"aligning"/"drifted"/"off");
// logf narrates steering decisions; alignRoomLabel applies the by-construction
// labeler alignment to the audio engine. All three funcs may be nil.
func NewSteerer(link LinkGrid, initialBPM float64, emit func(state string, errMs float64), logf func(format string, args ...any), alignRoomLabel func(roomIndex, localIndex int64), onSnapGrid ...func(deltaUs int64)) *Steerer {
	if emit == nil {
		emit = func(string, float64) {}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if alignRoomLabel == nil {
		alignRoomLabel = func(int64, int64) {}
	}
	snap := func(int64) {}
	if len(onSnapGrid) > 0 && onSnapGrid[0] != nil {
		snap = onSnapGrid[0]
	}
	return &Steerer{
		link:           link,
		emit:           emit,
		logf:           logf,
		alignRoomLabel: alignRoomLabel,
		onSnapGrid:     snap,
		aligner:        interval.NewGridAligner(),
		enabled:        true,
		entryPending:   true,
		gateOpen:       true, // nothing has shut it yet
		currentBPM:     initialBPM,
	}
}

// OnAnchor feeds a relay interval_anchor to the steerer: the room grid's
// next boundary (server clock), the anchor's room index, and the room's
// authoritative tempo. Runs entry conformance if armed, then re-derives the
// room label by construction (post-snap, or immediately on steady-state
// anchor refreshes).
func (s *Steerer) OnAnchor(nextBoundaryServerUs, index int64, roomBPM, bpi float64, now time.Time) {
	s.aligner.SetAnchor(nextBoundaryServerUs, roomBPM, bpi)
	s.anchorIndex, s.haveRoomAnchor = index, true
	s.tryEntry(bpi, now)
	// Steady-state anchor (tempo/config change): entry wasn't pending, so
	// re-derive the label here — race-free on an aligned grid.
	s.applyRoomLabel(bpi)
}

// OnServerPong folds one relay time sample into the server↔local offset
// estimate. serverNowEstUs is the server's clock reading estimated at the
// local receipt instant (server stamp + RTT/2); rttUs is the measured round
// trip (the offset estimate takes the lowest-RTT samples — see GridAligner);
// the local clock is sampled from the bridge at the same instant. Runs entry
// conformance if armed.
func (s *Steerer) OnServerPong(serverNowEstUs, rttUs int64, bpi float64, now time.Time) {
	s.aligner.ObserveServerTime(serverNowEstUs, s.link.State().TimestampUs, rttUs)
	s.tryEntry(bpi, now)
}

// NoteTempoCommitted records that the session committed to a tempo — a local
// user's change, a remote TempoChange, an adopted snapshot, or a UI command.
// This is the fused write that used to be two variables at five call sites:
// it updates the committed-tempo record AND arms the slew's tempo gate.
// A commit is an ownership move, so it also drops any in-flight episode:
// pre-fix the stale slew target survived, and the tempo-settling gate then
// blocked the slew forever (the session at the new room tempo never matched
// the old target) — drift correction silently died for the rest of the
// session on every mid-slew tempo change. No restore: the commit itself is
// writing the tempo, so putting the episode's base back would fight it.
func (s *Steerer) NoteTempoCommitted(bpm float64, now time.Time) {
	s.currentBPM = bpm
	s.lastTempoAt = now
	s.clearEpisode()
}

// CurrentBPM returns the tempo the session last committed to. The session
// reads it for echo-guard comparisons, boundary/sender tagging, and the
// disable-restore target.
func (s *Steerer) CurrentBPM() float64 { return s.currentBPM }

// SnapshotTempoAdopt decides whether a StateSnapshot's tempo may be applied
// locally. With a room anchor, the anchor's tempo is authoritative:
// snapshots disagreeing with it are stale (e.g. from a not-yet-conformed
// joiner), and adopting them feeds the two-peer adoption oscillator — A
// adopts B's tempo while B adopts A's, inverting the pair every 200ms
// snapshot period forever (the 110↔120 field report, #424). Genuine changes
// travel the TempoChange path (event-driven, re-anchors the room), so gating
// snapshots costs nothing. Without an anchor (old server, clockless room),
// keep the pre-anchor convergence behavior.
func (s *Steerer) SnapshotTempoAdopt(msgBPM float64) bool {
	roomBPM, hasAnchor := s.aligner.RoomBPM()
	return snapshotTempoAdopt(roomBPM, hasAnchor, msgBPM, s.currentBPM)
}

// Tick drives the steady-state grid slew: close drift against the room grid
// with bounded tempo nudges. Gated against entry settling and tempo commits;
// never acts while entry conformance is pending. The slew chases the
// anchor's authoritative room tempo (ADR-0006: "nudge the room tempo… then
// restore"), not the committed tempo — they can differ by the adoption
// threshold. Slew nudges deliberately do NOT touch the committed-tempo
// record: that would arm the tempo gate and suppress the slew itself. See
// Tick for the tempo-settling gate that keeps the slew from fighting tempo
// changes in flight.
func (s *Steerer) Tick(bpi float64, now time.Time) {
	// Every path that returns before the gate clears the wedge timer: it
	// measures how long the gate has been *continuously* shut across ticks we
	// actually evaluated, not wall time since it first shut. Otherwise a
	// tempo-commit burst, or alignment being switched off for five minutes,
	// leaves it accruing and the next evaluated tick escalates instantly.
	if !s.enabled || !s.aligner.Ready() || s.entryPending {
		s.gateShutSince = time.Time{}
		return
	}
	if !slewAllowed(now, s.lastSnapAt, s.lastTempoAt) {
		s.gateShutSince = time.Time{}
		return
	}
	roomBPM, ok := s.aligner.RoomBPM()
	if !ok || roomBPM <= 0 {
		s.gateShutSince = time.Time{}
		return
	}
	// Same-rate gate: δ is a phase measurement that is only meaningful when
	// both grids tick at the same rate. While the session tempo diverges
	// from the slew's baseline (the slew target when actively slewing, else
	// the anchor tempo) — a LAN convergence drag, a user knob turn, a tempo
	// change whose re-anchor hasn't landed — something else owns the tempo
	// and nudging would fight it (field finding: the slew chased a stale
	// 120 anchor while the session moved to 122, preventing the settle the
	// detector hold-down waits for — a live-lock).
	// Measured before the gate: the gate suppresses *steering*, never
	// *reporting*. Returning without emitting froze the UI on whatever δ was
	// current when the state last changed — a reading minutes stale, shown as
	// if it were live (field: a badge stuck at "drifted 2605ms" for a quarter
	// of an hour while the grid sat at ~10ms).
	delta, ok := s.measureDelta(bpi)
	if !ok {
		s.gateShutSince = time.Time{}
		return
	}
	st := s.link.State()
	// An episode holds the session away from where we found it. If the session
	// is no longer sitting at our nudge, someone else has taken the tempo: the
	// base we are measuring against is stale, so drop the episode and judge the
	// fresh reading on its own merits. No write — a session that is not at our
	// target has nothing of ours left in force to put back. This is what used to
	// need a ten-second wedge timer to escape (the gate compared against the
	// stale target, and the gate then prevented clearing it: sixteen minutes of
	// no alignment at all, in the field).
	if s.slewTarget != 0 && math.Abs(st.BPM-s.slewTarget) > tempoThreshold {
		s.logf("[align] slew episode dropped: session %.4f BPM is no longer our target %.4f — re-measuring",
			st.BPM, s.slewTarget)
		s.clearEpisode()
	}
	// Tempo-settling gate: δ is a phase measurement, meaningful only while both
	// grids tick at the same rate. The reading judged is the session tempo with
	// our own nudge taken back out — episodeBase while an episode is in flight —
	// so an active slew can never gate itself off.
	settling := st.settlingBPM()
	if s.slewTarget != 0 {
		settling = s.episodeBase
	}
	if !s.tempoSettled(settling, roomBPM) {
		// Bucket only while the gate is shut. δ is a phase measurement between
		// grids ticking at different rates, so it sweeps and wraps at ±period/2
		// — publishing that at tick rate would trade a stale number for a
		// meaningless one. The bucket still moves if things get materially
		// worse, and the recovery paths bound how long this can last.
		s.emitBucket(delta, now)
		s.noteGateShut(now, roomBPM, settling)
		return
	}
	s.gateShutSince = time.Time{}
	periodUs := int64(bpi * 60.0 / roomBPM * 1e6)
	// A new episode nudges from what we just observed; a running one keeps the
	// base it started with, so a settle restores the session's own tempo rather
	// than pulling it onto the room's.
	base := s.episodeBase
	if s.slewTarget == 0 {
		base = st.BPM
	}
	target, active := interval.SlewTempo(base, delta, periodUs)
	if s.slewTarget != 0 && !active {
		// Settle hysteresis: an active slew restores only when δ is truly
		// closed (≤ SlewSettleUs) or has flipped sign — in between it holds
		// the nudge so the episode settles deep instead of stopping at the
		// deadband edge and re-firing when skew walks δ back out.
		if abs64(delta) <= interval.SlewSettleUs || (delta > 0) != (s.slewDir > 0) {
			restored := s.episodeBase
			s.link.SetTempo(restored)
			s.clearEpisode()
			s.logf("[align] slew settled (δ=%+.1f ms), restored %.4f BPM", float64(delta)/1000, restored)
		}
		s.emitState(delta, now)
		return
	}
	if active {
		// Persistence: δ must hold the same direction past the deadband for
		// slewPersistenceTicks ticks before acting (drift persists; spikes
		// don't). A sign flip restarts the confirmation without retargeting.
		dir := 1
		if delta < 0 {
			dir = -1
		}
		if dir == s.slewPendingDir {
			s.slewPendingCount++
		} else {
			s.slewPendingDir, s.slewPendingCount = dir, 1
		}
		if s.slewPendingCount < slewPersistenceTicks {
			s.emitState(delta, now)
			return
		}
		if target != s.slewTarget {
			s.link.SetTempo(target)
			s.episodeBase = base
			s.slewTarget = target
			s.slewDir = dir
			s.logf("[align] slew: δ=%+.1f ms → tempo %.4f BPM (from %.4f)", float64(delta)/1000, target, base)
		}
	} else {
		s.slewPendingDir, s.slewPendingCount = 0, 0
	}
	s.emitState(delta, now)
}

// SetEnabled toggles grid alignment (ADR-0006 debug control). Disabling
// stops all steering — restoring the committed tempo if mid-slew — and
// reports "off". Enabling re-arms entry conformance so the grid re-measures
// and snaps if needed.
func (s *Steerer) SetEnabled(on bool, bpi float64, now time.Time) {
	s.enabled = on
	if !on {
		s.cancelSlew() // restores the episode's own base, not the committed tempo
		s.lastState = ""
		s.emit("off", 0)
		s.logf("[align] grid alignment disabled")
		return
	}
	s.entryPending = true
	s.tryEntry(bpi, now)
	s.logf("[align] grid alignment enabled")
}

// OnRejoin re-arms entry conformance after a signaling reconnect (ADR-0006:
// rejoin is an entry). The fresh anchor + relay pongs re-measure δ; a
// mid-blip rejoin finds δ ≈ 0 and no-ops, a genuinely diverged grid snaps
// back onto the room. A rejoin also drops any in-flight episode — the room may
// have moved while we were gone, so the base we were nudging from no longer
// describes anything, and entry conformance is about to re-measure regardless.
func (s *Steerer) OnRejoin() {
	s.entryPending = true
	s.clearEpisode()
}

// Status returns the debug-panel readout: ("off", 0, true) when disabled,
// ok=false until a δ can be measured (no anchor/offset yet, or Link state
// unusable), otherwise the bucketed state and δ in milliseconds.
func (s *Steerer) Status(bpi float64) (state string, errMs float64, ok bool) {
	if !s.enabled {
		return "off", 0, true
	}
	if !s.aligner.Ready() {
		return "", 0, false
	}
	delta, mok := s.measureDelta(bpi)
	if !mok {
		return "", 0, false
	}
	return stateName(delta), float64(delta) / 1000, true
}

// tryEntry runs entry conformance once the anchor and the first relay time
// sample are both in: adopt the room's authoritative tempo first (the snap
// assumes the grids tick at the same rate — idempotent, safe across
// retries), then measure δ and snap only past the perceptual threshold.
// Entry conformance is mandated on every (re)join (ADR-0006) — a failed
// measurement stays pending so the next anchor or relay pong retries.
func (s *Steerer) tryEntry(bpi float64, now time.Time) {
	if !s.enabled || !s.entryPending || !s.aligner.Ready() {
		return
	}
	if roomBPM, ok := s.aligner.RoomBPM(); ok {
		if st := s.link.State(); st.BPM > 0 && math.Abs(st.BPM-roomBPM) > tempoThreshold {
			s.link.SetTempo(roomBPM)
			s.currentBPM = roomBPM
			s.lastTempoAt = now
			s.logf("[align] entry: adopted room tempo %.1f BPM", roomBPM)
		}
	}
	delta, ok := s.measureDelta(bpi)
	if !ok {
		// Transient (e.g. Link still coming up): stay pending.
		s.logf("[align] entry: δ measurement unavailable (Link not ready?) — will retry")
		return
	}
	s.entryPending = false
	if snapNeeded(delta) {
		s.link.SnapGrid(delta)
		s.onSnapGrid(delta)
		s.lastSnapAt = now
		s.logf("[align] entry: snapped grid %+.1f ms onto the room grid", float64(delta)/1000)
	} else {
		s.logf("[align] entry: grid already aligned (δ=%+.1f ms)", float64(delta)/1000)
	}
	s.emitState(delta, now)
	// The snap may have shifted the grid past a boundary since SetRoomAnchor
	// sampled the labeler — re-derive the label by construction.
	s.applyRoomLabel(bpi)
}

// applyRoomLabel re-derives the labeler offset from the anchor's boundary
// time (exact by construction on an aligned grid) and overrides
// SetRoomAnchor's sample align. A sample taken before the entry snap is off
// by one whenever the snap moves the sampling instant across a local
// boundary — the peer then labels every interval one off and its audio
// silently plays an interval late/early for the whole session (anchors only
// re-send on tempo/config change). Runs only once entry conformance has
// completed (grid aligned); without it the sample align stays as the
// fallback (ADR-0006).
func (s *Steerer) applyRoomLabel(bpi float64) {
	if !s.enabled || s.entryPending || !s.haveRoomAnchor {
		return
	}
	li, ok := s.roomLabelLocalIndex(bpi)
	if !ok {
		s.logf("[align] label derive failed (anchor/BPM/offset not ready) — keeping sample align")
		return
	}
	s.alignRoomLabel(s.anchorIndex, li)
}

// roomLabelLocalIndex derives the local interval index that ends at the
// anchor's next boundary (mapped into the local Link clock) — the local
// index corresponding to the anchor's CurrentIndex by construction. Valid
// only on an aligned grid (post entry conformance): it rounds the room
// boundary to the nearest local boundary. Unlike sampling the current local
// index at anchor receipt, this is exact regardless of when in the interval
// it runs and immune to the align-then-snap hazard. ok is false until the
// aligner is Ready and the local state is usable.
func (s *Steerer) roomLabelLocalIndex(bpi float64) (int64, bool) {
	if bpi <= 0 {
		return 0, false
	}
	boundaryServerUs, ok := s.aligner.AnchorBoundary()
	if !ok {
		return 0, false
	}
	offsetUs, ok := s.aligner.OffsetUs()
	if !ok {
		return 0, false
	}
	periodUs, ok := s.aligner.PeriodUs()
	if !ok || periodUs <= 0 {
		return 0, false
	}
	st := s.link.State()
	if st.BPM <= 0 {
		return 0, false
	}
	boundaryLocalUs := boundaryServerUs - offsetUs
	// The local interval in progress ends at tEnd; each later local interval
	// ends one period after that. Round the room boundary to the nearest
	// local boundary and count intervals between them. Note the rounding
	// divides a local-clock distance by the ROOM period: on a tempo-change
	// anchor processed before local tempo adoption the two grids tick at
	// slightly different periods. The error is ~tempo-ratio × |δ|/period and
	// stays far below period/2 for any plausible tempo change (δ ≤ 25 ms
	// post-conformance), so math.Round still lands on the right boundary.
	curIdx := int64(math.Floor(st.Beat / bpi))
	tEnd := s.link.TimeAtBeat(float64(curIdx+1) * bpi)
	k := int64(math.Round(float64(boundaryLocalUs-tEnd) / float64(periodUs)))
	// Diagnostic (field hunt: frozen +N label offsets after anchor
	// turbulence, v3.12.3 jam): every input to the rounding, so a wrong k
	// can be attributed — torn State/TimeAtBeat read across a Link timeline
	// jump, a bad offsetUs (stale pong RTT), or an unaligned grid.
	s.logf("[align] label derive: anchorIdx=%d boundarySrv=%d offsetUs=%d periodUs=%d beat=%.2f curIdx=%d tEnd=%d k=%d → local=%d",
		s.anchorIndex, boundaryServerUs, offsetUs, periodUs, st.Beat, curIdx, tEnd, k, curIdx+k)
	return curIdx + k, true
}

// measureDelta samples the local grid and returns δ in microseconds
// (positive = local grid runs late vs the room grid). ok is false until the
// aligner is Ready or when the local state is unusable. The beat lens is the
// room BPI, so boundaries fall at multiples of bpi.
func (s *Steerer) measureDelta(bpi float64) (int64, bool) {
	if bpi <= 0 {
		return 0, false
	}
	st := s.link.State()
	if st.BPM <= 0 {
		return 0, false
	}
	boundaryBeat := (math.Floor(st.Beat/bpi) + 1) * bpi
	return s.aligner.Delta(s.link.TimeAtBeat(boundaryBeat))
}

// tempoSettlingBand is how far the session tempo may sit from the room tempo
// before δ stops meaning anything. It is exactly the slew's authority (0.06 BPM
// at 120): the slew may steer precisely what it can hold, and a divergence
// wider than that is someone else's to resolve — reported as intent, or
// enforced back. Keyed to the authority rather than the flat 0.5% it used to
// be, so "what we can fix" and "what the room must be told" tile with no band
// between them that is neither (ADR-0009).
//
// A wandering clock no longer trips it, because the gate judges the windowed
// mean: that was the failure the 0.5% band was widened to avoid, and widening
// is no longer the mechanism doing the work.
func tempoSettlingBand(roomBPM float64) float64 {
	return math.Max(tempoThreshold, interval.SlewAuthorityBPM(roomBPM))
}

// gateReopenFraction makes the band hysteretic: it takes the full band to shut
// the gate and 0.9× to reopen it, so a tempo sitting on the edge cannot chatter
// the slew on and off tick by tick.
const gateReopenFraction = 0.9

// tempoSettled reports whether the session is close enough to the room tempo
// for δ to be a real measurement, and records which side of the band we are on.
func (s *Steerer) tempoSettled(settlingBPM, roomBPM float64) bool {
	band := tempoSettlingBand(roomBPM)
	if !s.gateOpen {
		band *= gateReopenFraction
	}
	s.gateOpen = math.Abs(settlingBPM-roomBPM) <= band
	return s.gateOpen
}

// clearEpisode drops all slew-episode bookkeeping without writing a tempo.
func (s *Steerer) clearEpisode() {
	s.episodeBase = 0
	s.slewTarget, s.slewDir = 0, 0
	s.slewPendingDir, s.slewPendingCount = 0, 0
}

// noteGateShut reports a gate that has been deferring for a long time.
//
// It used to be the escape from a wedge: the gate compared the session tempo
// against the slew's own target, so once something else moved the tempo the
// stale target held the gate shut and the shut gate prevented clearing the
// target — sixteen minutes of no alignment at all, in the field. Tick now drops
// an episode the moment the session stops sitting at our nudge, and the gate
// judges the session against the ROOM tempo rather than against our target, so
// the loop cannot form. What remains is worth saying out loud: a gate shut this
// long means the peer's tempo has genuinely diverged and drift is going
// uncorrected while we defer to whoever owns it.
//
// Deliberately still no tempo write and no forced re-entry: both would fight
// whoever legitimately owns the tempo, and a re-entry snap is audible.
func (s *Steerer) noteGateShut(now time.Time, roomBPM, settlingBPM float64) {
	if s.gateShutSince.IsZero() {
		s.gateShutSince = now
		return
	}
	if now.Sub(s.gateShutSince) < gateWedgeTimeout {
		return
	}
	s.gateShutSince = now // re-arm, so this repeats rather than logging once
	s.logf("[align] deferring for %s: session %.4f BPM vs room %.4f — drift uncorrected while the tempo is someone else's",
		gateWedgeTimeout, settlingBPM, roomBPM)
}

// OnGridJump re-arms entry conformance after the local Link grid moved out
// from under us — a session merge or transport reset, which the engine
// detects as a beat discontinuity. The slew only closes small phase errors at
// a bounded rate, so a jump of whole beats is not something it can walk back;
// without a re-snap the grid stays off for the rest of the session.
func (s *Steerer) OnGridJump(beats float64, now time.Time) {
	if !s.enabled {
		return
	}
	// Re-entry snaps the grid, which is audible. Detection is a heuristic over
	// a sampled beat clock, so one bad reading must not be able to snap the
	// grid repeatedly on a peer whose machine is stalling — rate limit it to
	// the settle window entry conformance already observes.
	if !s.lastJumpEntryAt.IsZero() && now.Sub(s.lastJumpEntryAt) < snapSettle {
		s.logf("[align] local grid jumped %+.2f beats — ignored, re-entered %s ago",
			beats, now.Sub(s.lastJumpEntryAt).Round(time.Millisecond))
		return
	}
	s.lastJumpEntryAt = now
	s.gateShutSince = time.Time{}
	s.cancelSlew()
	s.entryPending = true
	s.logf("[align] local grid jumped %+.2f beats — re-running entry conformance", beats)
}

// cancelSlew abandons an in-flight slew, putting the tempo back first. The
// slew works by holding the session away from the room tempo, so clearing the
// bookkeeping alone strands it there: entry conformance is not guaranteed to
// commit a tempo (it only adopts past tempoThreshold, and not at all until the
// aligner is Ready), and SetEnabled's restore keys off the very target we just
// cleared — leaving the session parked on a slew tempo with nothing tracking it.
func (s *Steerer) cancelSlew() {
	if s.slewTarget != 0 {
		// The episode's own base first: it is the tempo the session had before
		// we nudged it, so putting it back is exact. The room tempo is only a
		// fallback for an episode with no base recorded.
		restore := s.episodeBase
		if restore <= 0 {
			restore = s.currentBPM
			if roomBPM, ok := s.aligner.RoomBPM(); ok && roomBPM > 0 {
				restore = roomBPM
			}
		}
		if restore > 0 {
			s.link.SetTempo(restore)
		}
	}
	s.clearEpisode()
}

// emitState reports alignment to the UI on a bucket change, and also when δ
// itself has moved materially inside the same bucket. Bucket-only was a trap:
// the reported error froze at whatever δ was when the state last changed, so a
// grid recovering from 2.6 s to 60 ms still read "drifted 2605ms" — a number
// with no relationship to the present.
func (s *Steerer) emitState(deltaUs int64, now time.Time) {
	state := stateName(deltaUs)
	if state != s.lastState {
		s.publish(state, deltaUs, now)
		return
	}
	// Same bucket: report a materially different δ, but not more often than
	// emitMinInterval. The step alone does not bound anything — the reference
	// is the last *emitted* δ, so a value alternating by more than the step
	// (±12ms jitter is squarely the slew's own operating regime) clears it on
	// every single tick and streams events indefinitely.
	if abs64(deltaUs-s.lastEmittedUs) < emitDeltaUs {
		return
	}
	if !s.lastEmitAt.IsZero() && now.Sub(s.lastEmitAt) < emitMinInterval {
		return
	}
	s.publish(state, deltaUs, now)
}

// emitBucket reports a state change only, ignoring δ movement — for ticks
// where the measurement itself is not trustworthy enough to publish.
func (s *Steerer) emitBucket(deltaUs int64, now time.Time) {
	if state := stateName(deltaUs); state != s.lastState {
		// now, not the zero time: publishing with a zero stamp left lastEmitAt
		// zero, and the next emitState then saw IsZero() and skipped the
		// same-bucket rate limit entirely — a gated stretch quietly re-armed
		// the limiter it was supposed to be bounded by.
		s.publish(state, deltaUs, now)
	}
}

func (s *Steerer) publish(state string, deltaUs int64, now time.Time) {
	s.lastState, s.lastEmittedUs, s.lastEmitAt = state, deltaUs, now
	s.emit(state, float64(deltaUs)/1000)
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

// slewAllowed applies the slew gates: no steering in the settling window
// after an entry snap, and none shortly after any tempo commit (local user's
// hand or a remote adoption — both re-anchor or re-tempo the grids anyway).
func slewAllowed(now, lastSnapAt, lastTempoAt time.Time) bool {
	if !lastSnapAt.IsZero() && now.Sub(lastSnapAt) < snapSettle {
		return false
	}
	if !lastTempoAt.IsZero() && now.Sub(lastTempoAt) < tempoGate {
		return false
	}
	return true
}

// snapshotTempoAdopt is the pure decision behind SnapshotTempoAdopt (see its
// comment for the oscillator rationale).
func snapshotTempoAdopt(roomBPM float64, hasAnchor bool, msgBPM, localBPM float64) bool {
	if math.Abs(msgBPM-localBPM) <= tempoThreshold {
		return false
	}
	if !hasAnchor {
		return true
	}
	return math.Abs(msgBPM-roomBPM) <= tempoThreshold
}

// stateName buckets δ for the UI: aligned inside the deadband, aligning
// while the slew is working, drifted past the perceptual threshold.
func stateName(deltaUs int64) string {
	switch abs := abs64(deltaUs); {
	case abs > interval.AlignThresholdUs:
		return "drifted"
	case abs > interval.SlewDeadbandUs:
		return "aligning"
	default:
		return "aligned"
	}
}
