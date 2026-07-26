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
	// tempoGate suppresses the slew after any tempo commit (local user's
	// hand, remote change, or entry adoption) so WAIL never fights a hand
	// on the tempo knob. Deliberately longer than the 150ms echo guard.
	tempoGate = 3 * time.Second
	// tempoThreshold mirrors the Link poller's tempoChangeThreshold (0.01
	// BPM): below it two tempos are "the same" for adoption decisions.
	tempoThreshold = 0.01
	// slewPersistenceTicks is how many consecutive ticks δ must hold outside
	// the deadband in the same direction before the slew acts. Real grid
	// drift persists for minutes; measurement spikes (a teleporting offset
	// estimate over a jittery WAN path — 2026-07-26 Australia VPN) last a
	// tick or two and often alternate sign. Chasing spikes flapped the tempo
	// every two seconds in the field. Settling stays immediate — restoring
	// the room tempo is always safe.
	slewPersistenceTicks = 2
)

// State is the slice of Link session state the steerer samples. Beat is
// phase-encoded at the room BPI (the interval-quantum lens, ADR-0003).
type State struct {
	BPM         float64
	Beat        float64
	TimestampUs int64
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

	aligner          *interval.GridAligner
	enabled          bool
	entryPending     bool
	lastSnapAt       time.Time
	lastTempoAt      time.Time
	slewTarget       float64 // 0 = sitting at exact room tempo
	slewDir          int     // sign of the active episode's δ (0 = not slewing)
	slewPendingDir   int     // sign of the δ being confirmed (0 = none)
	slewPendingCount int     // consecutive same-direction ticks past the deadband
	lastState        string
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
// A commit is an ownership move, so it also cancels any in-flight slew:
// pre-fix the stale slew target survived, and the same-rate gate then
// blocked the slew forever (the session at the new room tempo never matched
// the old target) — drift correction silently died for the rest of the
// session on every mid-slew tempo change.
func (s *Steerer) NoteTempoCommitted(bpm float64, now time.Time) {
	s.currentBPM = bpm
	s.lastTempoAt = now
	s.slewTarget = 0
	s.slewPendingDir, s.slewPendingCount = 0, 0
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
// Tick for the same-rate gate that keeps the slew from fighting tempo
// changes in flight.
func (s *Steerer) Tick(bpi float64, now time.Time) {
	if !s.enabled || !s.aligner.Ready() || s.entryPending {
		return
	}
	if !slewAllowed(now, s.lastSnapAt, s.lastTempoAt) {
		return
	}
	roomBPM, ok := s.aligner.RoomBPM()
	if !ok || roomBPM <= 0 {
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
	baseline := roomBPM
	if s.slewTarget != 0 {
		baseline = s.slewTarget
	}
	if st := s.link.State(); math.Abs(st.BPM-baseline) > tempoThreshold {
		return
	}
	delta, ok := s.measureDelta(bpi)
	if !ok {
		return
	}
	periodUs := int64(bpi * 60.0 / roomBPM * 1e6)
	target, active := interval.SlewTempo(roomBPM, delta, periodUs)
	if s.slewTarget != 0 && !active {
		// Settle hysteresis: an active slew restores only when δ is truly
		// closed (≤ SlewSettleUs) or has flipped sign — in between it holds
		// the nudge so the episode settles deep instead of stopping at the
		// deadband edge and re-firing when skew walks δ back out.
		if abs64(delta) <= interval.SlewSettleUs || (delta > 0) != (s.slewDir > 0) {
			s.link.SetTempo(roomBPM)
			s.slewTarget = 0
			s.slewDir = 0
			s.slewPendingDir, s.slewPendingCount = 0, 0
			s.logf("[align] slew settled (δ=%+.1f ms), restored %.1f BPM", float64(delta)/1000, roomBPM)
		}
		s.emitState(delta)
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
			s.emitState(delta)
			return
		}
		if target != s.slewTarget {
			s.link.SetTempo(target)
			s.slewTarget = target
			s.slewDir = dir
			s.logf("[align] slew: δ=%+.1f ms → tempo %.4f BPM", float64(delta)/1000, target)
		}
	} else {
		s.slewPendingDir, s.slewPendingCount = 0, 0
	}
	s.emitState(delta)
}

// SetEnabled toggles grid alignment (ADR-0006 debug control). Disabling
// stops all steering — restoring the committed tempo if mid-slew — and
// reports "off". Enabling re-arms entry conformance so the grid re-measures
// and snaps if needed.
func (s *Steerer) SetEnabled(on bool, bpi float64, now time.Time) {
	s.enabled = on
	if !on {
		if s.slewTarget != 0 {
			s.link.SetTempo(s.currentBPM)
			s.slewTarget = 0
		}
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
// back onto the room. A rejoin also cancels any in-flight slew target — the
// room may have moved while we were gone, and a stale target would wedge
// the same-rate gate exactly as a mid-slew tempo commit does.
func (s *Steerer) OnRejoin() {
	s.entryPending = true
	s.slewTarget = 0
	s.slewPendingDir, s.slewPendingCount = 0, 0
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
	s.emitState(delta)
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

// emitState reports the bucketed alignment state on change only.
func (s *Steerer) emitState(deltaUs int64) {
	state := stateName(deltaUs)
	if state == s.lastState {
		return
	}
	s.lastState = state
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
