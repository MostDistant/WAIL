package align

import (
	"math"
	"testing"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// fakeGrid is a scripted LinkGrid: the room is 120 BPM, 16 BPI (8s period),
// its anchor boundary sits at server 8s, and the server runs 2s ahead of the
// local Link clock. The local grid's next boundary is timeAtBeat — 14s is
// exactly aligned (8s anchor − 2s offset + 8s period); anything later is a
// late grid by that amount.
type fakeGrid struct {
	state      State
	timeAtBeat int64
	snaps      []int64
	tempos     []float64
}

func (f *fakeGrid) State() State             { return f.state }
func (f *fakeGrid) TimeAtBeat(float64) int64 { return f.timeAtBeat }

// SetTempo models the real bridge: the session tempo actually moves.
func (f *fakeGrid) SetTempo(bpm float64) {
	f.tempos = append(f.tempos, bpm)
	f.state.BPM = bpm
}
func (f *fakeGrid) SnapGrid(deltaUs int64) { f.snaps = append(f.snaps, deltaUs) }

type emitted struct {
	state string
	errMs float64
}

// newSteerer returns a steerer over a fresh fake grid (local clock 8s, beat
// 15.5 → next boundary beat 16), plus the emit log. Nothing observed yet:
// entry conformance stays pending until both anchor and server pong arrive.
func newSteerer(timeAtBeat int64) (*Steerer, *fakeGrid, *[]emitted) {
	f := &fakeGrid{
		state:      State{BPM: 120, Beat: 15.5, TimestampUs: 8_000_000},
		timeAtBeat: timeAtBeat,
	}
	emits := &[]emitted{}
	s := NewSteerer(f, 120, func(state string, errMs float64) {
		*emits = append(*emits, emitted{state, errMs})
	}, func(string, ...any) {}, nil)
	return s, f, emits
}

// observe feeds the anchor and enough server pongs for Ready (entry fires
// if pending). The aligner requires 3 offset samples (wild-RTT guard).
func observe(s *Steerer, now time.Time) {
	s.OnAnchor(8_000_000, 0, 120, 16, now)
	for i := 0; i < 3; i++ {
		s.OnServerPong(10_000_000, 100_000, 16, now)
	}
}

func lastTempo(f *fakeGrid) float64 {
	if len(f.tempos) == 0 {
		return 0
	}
	return f.tempos[len(f.tempos)-1]
}

func lastEmit(emits *[]emitted) string {
	if len(*emits) == 0 {
		return ""
	}
	return (*emits)[len(*emits)-1].state
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEntrySnapFiresWhenLate(t *testing.T) {
	now := time.Now()
	s, f, emits := newSteerer(14_100_000) // 100ms late > 25ms threshold

	s.OnAnchor(8_000_000, 0, 120, 16, now)
	if len(f.snaps) != 0 {
		t.Fatal("snap before any server time sample — aligner not Ready yet")
	}
	for i := 0; i < 3; i++ {
		s.OnServerPong(10_000_000, 100_000, 16, now)
	}
	if len(f.snaps) != 1 || f.snaps[0] != 100_000 {
		t.Fatalf("snaps = %v, want [100000]", f.snaps)
	}
	if got := lastEmit(emits); got != "drifted" {
		t.Fatalf("last emit = %q, want drifted", got)
	}
}

// TestEntrySnapNotifiesEngine: the entry snap must notify the audio engine
// (the onSnapGrid hook) so emit feeders re-anchor silently — the snap moves
// the playhead, not the audio.
func TestEntrySnapNotifiesEngine(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000) // 100ms late → snap
	var snaps []int64
	s.onSnapGrid = func(deltaUs int64) { snaps = append(snaps, deltaUs) }
	observe(s, now)
	if len(f.snaps) != 1 {
		t.Fatalf("precondition: snaps = %v, want 1", f.snaps)
	}
	if len(snaps) != 1 || snaps[0] != 100_000 {
		t.Fatalf("onSnapGrid calls = %v, want [100000]", snaps)
	}
}

func TestEntryNoopWhenAligned(t *testing.T) {
	now := time.Now()
	s, f, emits := newSteerer(14_000_000) // exactly on the room grid
	observe(s, now)
	if len(f.snaps) != 0 {
		t.Fatalf("aligned grid snapped: %v", f.snaps)
	}
	if got := lastEmit(emits); got != "aligned" {
		t.Fatalf("last emit = %q, want aligned", got)
	}
}

func TestEntryRetriesWhenLinkUnready(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	f.state.BPM = 0 // Link still coming up: δ measurement unavailable
	observe(s, now)
	if len(f.snaps) != 0 {
		t.Fatal("snapped with unusable Link state")
	}
	// Link recovers; the next relay pong retries the pending entry.
	f.state.BPM = 120
	s.OnServerPong(10_000_000, 100_000, 16, now.Add(time.Second))
	if len(f.snaps) != 1 {
		t.Fatalf("pending entry did not retry: snaps = %v", f.snaps)
	}
}

func TestEntryAdoptsRoomTempo(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_000_000)
	f.state.BPM = 110 // local session disagrees with the room
	observe(s, now)
	if len(f.tempos) != 1 || f.tempos[0] != 120 {
		t.Fatalf("tempos = %v, want [120] (room tempo adopted)", f.tempos)
	}
	if got := s.CurrentBPM(); got != 120 {
		t.Fatalf("CurrentBPM = %v, want 120 after entry adoption", got)
	}
}

func TestSlewGatedAfterTempoCommit(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_000_000)
	observe(s, now)
	f.timeAtBeat = 14_100_000 // grid drifts 100ms late

	s.NoteTempoCommitted(120, now)
	s.Tick(16, now.Add(1*time.Second))
	if len(f.tempos) != 0 {
		t.Fatalf("slew fired inside the 3s tempo gate: %v", f.tempos)
	}
	s.Tick(16, now.Add(4*time.Second)) // first sighting past the gate
	s.Tick(16, now.Add(5*time.Second)) // persists → act
	if len(f.tempos) != 1 || !approxEq(f.tempos[0], 120*(1+interval.SlewMaxFraction)) {
		t.Fatalf("tempos = %v, want [120.06] (clamped slew)", f.tempos)
	}
}

func TestSlewGatedAfterSnap(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now) // entry snaps at now
	if len(f.snaps) != 1 {
		t.Fatalf("snaps = %v, want 1", f.snaps)
	}
	s.Tick(16, now.Add(1*time.Second))
	if len(f.tempos) != 0 {
		t.Fatalf("slew fired inside the 5s post-snap settle: %v", f.tempos)
	}
	s.Tick(16, now.Add(6*time.Second)) // first sighting after settle
	s.Tick(16, now.Add(7*time.Second)) // persists → act
	if len(f.tempos) != 1 || !approxEq(f.tempos[0], 120*(1+interval.SlewMaxFraction)) {
		t.Fatalf("tempos = %v, want [120.06] after settle", f.tempos)
	}
}

func TestSlewSettleRestoresRoomTempo(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // sighting
	s.Tick(16, now.Add(7*time.Second)) // slew active
	if len(f.tempos) != 1 {
		t.Fatalf("tempos = %v, want slew active", f.tempos)
	}
	// The slew must not touch the committed-tempo record.
	if got := s.CurrentBPM(); got != 120 {
		t.Fatalf("CurrentBPM = %v after slew nudge, want 120 (committed tempo untouched)", got)
	}
	f.timeAtBeat = 14_000_000 // drift closed
	s.Tick(16, now.Add(8*time.Second))
	if got := lastTempo(f); got != 120 {
		t.Fatalf("settled tempo = %v, want 120 (exact room tempo restored)", got)
	}
}

func TestSlewNudgeDoesNotArmTempoGate(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // sighting
	s.Tick(16, now.Add(7*time.Second)) // slew → 120.06 (clamped)
	if len(f.tempos) != 1 {
		t.Fatalf("tempos = %v, want slew active", f.tempos)
	}
	// The slew's own SetTempo must not arm the 3s gate against itself:
	// a changed δ still steers. (Opposite-sign δ: post cruise-clamp every
	// active same-sign δ maps to the same target, which can't prove a second
	// nudge fired. The flip restarts persistence — the nudge lands on the
	// second confirming tick.)
	f.timeAtBeat = 13_985_000          // δ=−15ms → target 119.94 ≠ current slew target
	s.Tick(16, now.Add(8*time.Second)) // flip sighting
	s.Tick(16, now.Add(9*time.Second)) // confirmed → retarget
	if len(f.tempos) != 2 || !approxEq(f.tempos[1], 120*(1-interval.SlewMaxFraction)) {
		t.Fatalf("tempos = %v, want a second nudge — the slew gated itself", f.tempos)
	}
}

func TestTickNeverFiresWhileEntryPending(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	// Anchor + pong never arrive: not Ready, entry pending.
	s.Tick(16, now)
	observe(s, now) // entry consumes the misalignment (snap), still no slew
	s.Tick(16, now)
	if len(f.tempos) != 0 {
		t.Fatalf("slew fired around entry conformance: %v", f.tempos)
	}
}

func TestSlewHoldsOffWhenSessionTempoDiverges(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000) // grid 100ms late: slew WOULD fire
	observe(s, now)
	// The session runs at 122 while the anchor says 120 (mid tempo change,
	// LAN drag, pending hold-down broadcast): the grids tick at different
	// rates, so δ is meaningless and the slew must hold off.
	f.state.BPM = 122
	s.Tick(16, now.Add(10*time.Second))
	if len(f.tempos) != 0 {
		t.Fatalf("slew steered while session tempo diverged from anchor: %v", f.tempos)
	}
	// Session returns to the anchor tempo: the slew may run.
	f.state.BPM = 120
	s.Tick(16, now.Add(11*time.Second)) // sighting
	s.Tick(16, now.Add(12*time.Second)) // persists → act
	if len(f.tempos) != 1 {
		t.Fatalf("slew did not resume after tempo agreement: %v", f.tempos)
	}
}

func TestSlewHoldsOffWhenDraggedFromOwnTarget(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // sighting
	s.Tick(16, now.Add(7*time.Second)) // slew active: target 120.06 (clamped)
	if len(f.tempos) != 1 {
		t.Fatalf("tempos = %v, want slew active", f.tempos)
	}
	// The session sits at the slew's own target: keep steering (no gate).
	f.state.BPM = lastTempo(f)
	f.timeAtBeat = 13_985_000          // δ=−15ms → different nudge (opposite sign)
	s.Tick(16, now.Add(8*time.Second)) // flip sighting
	s.Tick(16, now.Add(9*time.Second)) // confirmed → retarget
	if len(f.tempos) != 2 {
		t.Fatalf("slew gated itself at its own target: %v", f.tempos)
	}
	// Something drags the session away from the slew target: hold off.
	f.state.BPM = 119.0
	s.Tick(16, now.Add(8*time.Second))
	if len(f.tempos) != 2 {
		t.Fatalf("slew steered after the session was dragged off target: %v", f.tempos)
	}
}

func TestSnapshotTempoAdoptSteerer(t *testing.T) {
	now := time.Now()
	// With an anchor: only anchor-matching snapshots are adopted.
	s, _, _ := newSteerer(14_000_000)
	s.OnAnchor(8_000_000, 0, 120, 16, now)
	if s.SnapshotTempoAdopt(110) {
		t.Fatal("anchor-diverging snapshot must be ignored (oscillator)")
	}
	if s.SnapshotTempoAdopt(120) {
		t.Fatal("same tempo is always a no-op")
	}
	s.NoteTempoCommitted(110, now)
	if !s.SnapshotTempoAdopt(120) {
		t.Fatal("anchor-matching snapshot should be adopted")
	}
	// Without an anchor (old server): pre-anchor convergence behavior.
	fresh, _, _ := newSteerer(14_000_000)
	if !fresh.SnapshotTempoAdopt(110) {
		t.Fatal("no anchor: differing tempo should be adopted")
	}
	if fresh.SnapshotTempoAdopt(120) {
		t.Fatal("same tempo is a no-op even without an anchor")
	}
}

func TestDisableMidSlewRestores(t *testing.T) {
	now := time.Now()
	s, f, emits := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // sighting
	s.Tick(16, now.Add(7*time.Second)) // slew active at 120.06 (clamped)
	s.SetEnabled(false, 16, now.Add(8*time.Second))
	if got := lastTempo(f); got != 120 {
		t.Fatalf("disable restore tempo = %v, want 120 (committed tempo)", got)
	}
	if got := lastEmit(emits); got != "off" {
		t.Fatalf("last emit = %q, want off", got)
	}
	s.Tick(16, now.Add(12*time.Second))
	if len(f.tempos) != 2 {
		t.Fatalf("steering continued while disabled: %v", f.tempos)
	}
	// Re-enable: entry re-arms, re-measures, and the still-late grid snaps.
	s.SetEnabled(true, 16, now.Add(13*time.Second))
	if len(f.snaps) != 2 {
		t.Fatalf("re-enable did not re-run entry: snaps = %v", f.snaps)
	}
}

func TestTempoCommitMidSlewClearsSlewTarget(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // sighting
	s.Tick(16, now.Add(7*time.Second)) // slew active
	if s.slewTarget == 0 {
		t.Fatal("precondition: slew should be active")
	}
	// A tempo commit lands mid-slew (user knob, or a remote TempoChange —
	// the room moved to 121). Ownership cancels steering: the in-flight
	// slew target must be dropped. Pre-fix it stayed stuck, and the
	// tempo-settling gate then blocked the slew FOREVER — the session at the new
	// room tempo never matched the stale target, so drift correction
	// silently died for the rest of the session.
	s.NoteTempoCommitted(121, now.Add(8*time.Second))
	if s.slewTarget != 0 {
		t.Fatalf("slewTarget = %v after tempo commit, want 0 — a commit cancels the in-flight slew", s.slewTarget)
	}
	// The room re-anchors at 121 and the session follows. Past the tempo
	// gate the slew must steer against the new room grid (not stay wedged
	// behind the stale target).
	f.state.BPM = 121
	period := 16 * 60.0 / 121
	// Anchor boundary chosen so the same local boundary (14.1s, offset 2s)
	// wraps to δ=+100ms at the new period.
	anchor := int64(math.Round((16.1 - 2*period - 0.1) * 1e6))
	s.OnAnchor(anchor, 1, 121, 16, now.Add(8*time.Second))
	s.Tick(16, now.Add(11*time.Second)) // past the 3s tempo gate: sighting
	s.Tick(16, now.Add(12*time.Second)) // persists → act
	if len(f.tempos) != 2 {
		t.Fatalf("slew did not steer against the new room grid after a mid-slew commit (wedged): tempos=%v", f.tempos)
	}
}

func TestRejoinMidSlewClearsSlewTarget(t *testing.T) {
	now := time.Now()
	s, _, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // sighting
	s.Tick(16, now.Add(7*time.Second)) // slew active
	if s.slewTarget == 0 {
		t.Fatal("precondition: slew should be active")
	}
	// A reconnect lands mid-slew: the in-flight target must be dropped, or
	// it wedges the tempo-settling gate if the room moved during the blip.
	s.OnRejoin()
	if s.slewTarget != 0 {
		t.Fatalf("slewTarget = %v after rejoin, want 0", s.slewTarget)
	}
}

// TestCruiseAbsorbsClockSkew is the 2026-07-25 field scenario in a closed
// loop: 67ppm relay↔local clock skew regenerates δ ≈ 12ms every ~3 min,
// and the old 0.3%-clamped slew ate each regrowth with an audible 0.15%
// tempo wobble. The cruise clamp must keep δ bounded near the deadband for
// 10 minutes of ticks while EVERY committed tempo stays within ±0.05%
// (0.86 cents — inaudible) of the room tempo.
func TestCruiseAbsorbsClockSkew(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_000_000) // aligned at entry
	observe(s, now)
	const skewUsPerSec = 67.0 // field-measured relay↔local clock skew
	maxFrac := 0.0
	for i := 1; i <= 600; i++ {
		// Close the loop: the grid responds to the committed tempo — net
		// drift = skew − (committed/120 − 1) × period, per second.
		committed := 120.0
		if s.slewTarget != 0 {
			committed = s.slewTarget
		}
		f.timeAtBeat += int64(skewUsPerSec - (committed/120.0-1)*8_000_000)
		s.Tick(16, now.Add(time.Duration(i+10)*time.Second))
		if n := len(f.tempos); n > 0 {
			if frac := math.Abs(f.tempos[n-1]/120.0 - 1); frac > maxFrac {
				maxFrac = frac
			}
		}
		if delta, ok := s.measureDelta(16); ok && delta > int64(interval.AlignThresholdUs) {
			t.Fatalf("tick %d: δ=%dµs exceeded the snap threshold under cruise", i, delta)
		}
	}
	if maxFrac > interval.SlewMaxFraction+1e-9 {
		t.Fatalf("tempo offset %.5f%% exceeded the %.3f%% cruise clamp", maxFrac*100, interval.SlewMaxFraction*100)
	}
	if delta, _ := s.measureDelta(16); delta > 11_000 {
		t.Fatalf("final δ=%dµs, want bounded near the 10ms deadband", delta)
	}
}

// TestSlewRequiresPersistence: δ must hold outside the deadband in the same
// direction for two consecutive ticks before the slew acts (field finding,
// 2026-07-26 Australia VPN: teleporting offset estimates produced one-tick
// δ spikes in alternating directions; chasing each one flapped the tempo
// 89.955↔90.045 every two seconds).
func TestSlewRequiresPersistence(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // δ=+100ms, first sighting
	if len(f.tempos) != 0 {
		t.Fatalf("slew acted on a single-tick δ: %v", f.tempos)
	}
	s.Tick(16, now.Add(7*time.Second)) // persists → act
	if len(f.tempos) != 1 {
		t.Fatalf("slew did not act on persistent δ: %v", f.tempos)
	}
}

func TestSlewIgnoresSingleTickSpike(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_000_000) // aligned
	observe(s, now)
	f.timeAtBeat = 14_100_000 // one-tick spike
	s.Tick(16, now.Add(6*time.Second))
	f.timeAtBeat = 14_000_000 // gone next tick
	s.Tick(16, now.Add(7*time.Second))
	s.Tick(16, now.Add(8*time.Second))
	if len(f.tempos) != 0 {
		t.Fatalf("slew chased a one-tick spike: %v", f.tempos)
	}
}

func TestSlewNoFlapOnSignFlip(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second))
	s.Tick(16, now.Add(7*time.Second)) // active at 120.06
	if len(f.tempos) != 1 {
		t.Fatalf("precondition: slew active, tempos=%v", f.tempos)
	}
	f.state.BPM = lastTempo(f)
	// A single opposite-sign tick must NOT retarget (no tempo flap).
	f.timeAtBeat = 13_900_000 // δ=−100ms for one tick
	s.Tick(16, now.Add(8*time.Second))
	if len(f.tempos) != 1 {
		t.Fatalf("tempo flapped on a one-tick sign flip: %v", f.tempos)
	}
	// If the flip persists, the slew retargets on the second tick.
	s.Tick(16, now.Add(9*time.Second))
	if len(f.tempos) != 2 || !approxEq(f.tempos[1], 120*(1-interval.SlewMaxFraction)) {
		t.Fatalf("slew did not retarget on a persistent flip: %v", f.tempos)
	}
}

func TestSlewSettleStaysImmediate(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second))
	s.Tick(16, now.Add(7*time.Second)) // active
	f.state.BPM = lastTempo(f)
	f.timeAtBeat = 14_000_000          // δ closed
	s.Tick(16, now.Add(8*time.Second)) // restore must not wait for persistence
	if got := lastTempo(f); got != 120 {
		t.Fatalf("settled tempo = %v, want 120 (immediate restore)", got)
	}
}

// TestVPNTeleportReplay is the 2026-07-26 ss3 field replay, end to end
// through OnServerPong + Tick: over a jittery high-RTT path (Australia VPN),
// new min-RTT pongs teleported the offset candidate +70ms every ~8s while
// clean samples pulled it back. Pre-fix the estimate jumped to each
// teleport and the slew chased every jump — tempo flapped 89.955↔90.045
// every two seconds and the grid random-walked ±70ms (audible ~100ms flam).
// With the slew-capped estimate + persistence, teleports become ≤1.5ms
// ripples: δ never leaves the deadband and the tempo never moves.
func TestVPNTeleportReplay(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_000_000) // aligned at offset 2s
	observe(s, now)                   // 3 bootstrap pongs
	// Drive past the 8-sample bootstrap with clean pongs (offset 2s, RTT
	// near the bootstrap's 100ms so the 1.5× gate accepts them).
	for i := 0; i < 6; i++ {
		f.state.TimestampUs += 2_000_000
		s.OnServerPong(f.state.TimestampUs+2_000_000, 120_000, 16, now)
	}
	flaps := 0
	lastSign := 0
	tick := func(step int) {
		s.Tick(16, now.Add(time.Duration(20+step)*time.Second))
		if n := len(f.tempos); n > 0 {
			sign := 1
			if f.tempos[n-1] < 120 {
				sign = -1
			}
			if lastSign != 0 && sign != lastSign {
				flaps++
			}
			lastSign = sign
		}
	}
	step := 0
	for round := 0; round < 6; round++ {
		// Teleport: a "new minimum" pong whose candidate jumps +70ms. Ticks
		// run BETWEEN pongs in reality (1Hz vs 2s) — the spike must meet the
		// tick before clean samples erase it.
		f.state.TimestampUs += 2_000_000
		s.OnServerPong(f.state.TimestampUs+2_070_000, 80_000, 16, now)
		tick(step)
		step++
		tick(step)
		step++
		// Clean samples at the true offset walk the estimate back (accepted:
		// 120ms ≤ 1.5× the 80ms best), interleaved with more ticks.
		for i := 0; i < 3; i++ {
			f.state.TimestampUs += 2_000_000
			s.OnServerPong(f.state.TimestampUs+2_000_000, 120_000, 16, now)
			tick(step)
			step++
			tick(step)
			step++
		}
	}
	if len(f.tempos) != 0 {
		t.Fatalf("slew engaged under teleporting offset estimate: tempos=%v", f.tempos)
	}
	if flaps != 0 {
		t.Fatalf("tempo flapped %d times", flaps)
	}
}

// TestSlewSettleHysteresis: the slew fires past 10ms but settles only under
// 5ms — in between it holds the nudge so episodes settle DEEP instead of
// stopping at the deadband edge and re-firing ~40s later (ss3 field pattern:
// settle at −9.1ms, skew walks it back out in under a minute).
func TestSlewSettleHysteresis(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // sighting
	s.Tick(16, now.Add(7*time.Second)) // fires at 120.06
	if len(f.tempos) != 1 {
		t.Fatalf("precondition: slew active, tempos=%v", f.tempos)
	}
	f.state.BPM = lastTempo(f)
	// δ closes to 7ms (same sign, inside the fire deadband but outside the
	// settle band): the nudge HOLDS — no restore.
	f.timeAtBeat = 14_007_000
	s.Tick(16, now.Add(8*time.Second))
	if got := lastTempo(f); !approxEq(got, 120*(1+interval.SlewMaxFraction)) {
		t.Fatalf("tempo in the hysteresis zone = %v, want the nudge held (120.06)", got)
	}
	// δ closes under 5ms: restore.
	f.timeAtBeat = 14_003_000
	s.Tick(16, now.Add(9*time.Second))
	if got := lastTempo(f); got != 120 {
		t.Fatalf("tempo under the settle band = %v, want 120 (restored)", got)
	}
	// Re-fire, then δ flips sign in the zone: restore (the nudge overshot).
	f.timeAtBeat = 14_100_000
	s.Tick(16, now.Add(10*time.Second))
	s.Tick(16, now.Add(11*time.Second))
	f.state.BPM = lastTempo(f)
	f.timeAtBeat = 13_993_000 // δ=−7ms
	s.Tick(16, now.Add(12*time.Second))
	if got := lastTempo(f); got != 120 {
		t.Fatalf("tempo after sign flip = %v, want 120 (restored, not held)", got)
	}
}

func TestRejoinRearmsEntry(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_000_000)
	observe(s, now)
	if len(f.snaps) != 0 {
		t.Fatal("aligned entry snapped")
	}
	// Rejoin with a genuinely diverged grid: the fresh pong re-runs entry.
	f.timeAtBeat = 14_100_000
	s.OnRejoin()
	s.OnServerPong(10_000_000, 100_000, 16, now.Add(time.Second))
	if len(f.snaps) != 1 {
		t.Fatalf("rejoin entry did not fire: snaps = %v", f.snaps)
	}
}

func TestStatusReadout(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	if _, _, ok := s.Status(16); ok {
		t.Fatal("Status ok before anchor/pong")
	}
	observe(s, now)
	state, errMs, ok := s.Status(16)
	if !ok || state != "drifted" || !approxEq(errMs, 100) {
		t.Fatalf("Status = %q, %v, %v — want drifted, 100, true", state, errMs, ok)
	}
	s.SetEnabled(false, 16, now)
	if state, _, ok = s.Status(16); !ok || state != "off" {
		t.Fatalf("disabled Status = %q, %v — want off, true", state, ok)
	}
	_ = f
}

func TestEntrySuppressedWhileDisabled(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	s.SetEnabled(false, 16, now)
	observe(s, now)
	if len(f.snaps) != 0 || len(f.tempos) != 0 {
		t.Fatalf("entry ran while disabled: snaps=%v tempos=%v", f.snaps, f.tempos)
	}
}

func TestEmitStateDedup(t *testing.T) {
	now := time.Now()
	s, f, emits := newSteerer(14_100_000)
	observe(s, now) // entry emits "drifted" (δ=100ms)
	if len(*emits) != 1 {
		t.Fatalf("emits = %v, want exactly 1 after entry", *emits)
	}
	// Slew ticks keep measuring δ=100ms → same bucket → no further events.
	s.Tick(16, now.Add(6*time.Second))
	s.Tick(16, now.Add(7*time.Second))
	if len(*emits) != 1 {
		t.Fatalf("emits = %v, same bucket must not re-emit", *emits)
	}
	_ = f
}

// --- Room label derivation by construction (ported from #426's align.go) ---

const timelineBeatPeriodUs = 500_000.0 // 120 BPM

// timelineGrid models a linear 120 BPM Link timeline: beat 0 occurs at
// beat0Us; State is sampled at nowUs. SnapGrid shifts the grid earlier by
// deltaUs — exactly what the real bridge's ForceBeatAtTime(t−δ) does — so
// label-derivation tests can move the grid like a real entry snap.
type timelineGrid struct {
	beat0Us int64
	nowUs   int64
	bpm     float64
}

func (g *timelineGrid) State() State {
	return State{
		BPM:         g.bpm,
		Beat:        float64(g.nowUs-g.beat0Us) / timelineBeatPeriodUs,
		TimestampUs: g.nowUs,
	}
}
func (g *timelineGrid) TimeAtBeat(b float64) int64 {
	return g.beat0Us + int64(math.Round(b*timelineBeatPeriodUs))
}
func (g *timelineGrid) SetTempo(float64)       {}
func (g *timelineGrid) SnapGrid(deltaUs int64) { g.beat0Us -= deltaUs }

// readyTimelineSteerer returns a steerer on a timeline grid with a Ready
// aligner: anchor (nextBoundary at server 440s, index 54, 120 BPM, 16 BPI)
// plus one offset sample putting the server 2s ahead of the local clock.
func readyTimelineSteerer(g *timelineGrid, alignRoomLabel func(int64, int64)) *Steerer {
	s := NewSteerer(g, 120, nil, nil, alignRoomLabel)
	now := time.Now()
	s.OnAnchor(440_000_000, 54, 120, 16, now)
	for i := 0; i < 3; i++ {
		s.OnServerPong(g.nowUs+2_000_000, 100_000, 16, now) // server = local + 2s
	}
	return s
}

func TestRoomLabelLocalIndexAlignedGrid(t *testing.T) {
	// Room boundary ending interval 54 at server 440s ↔ local 438s (offset
	// 2s). Aligned grid: local interval 1 (beats [16,32)) ends at local 438s.
	const beat0 = 438_000_000 - 32*500_000 // beat 32 ↔ 438s
	for _, nowUs := range []int64{430_000_000, 437_900_000, 438_100_000} {
		s := readyTimelineSteerer(&timelineGrid{beat0Us: beat0, nowUs: nowUs, bpm: 120}, nil)
		idx, ok := s.roomLabelLocalIndex(16)
		if !ok {
			t.Fatalf("roomLabelLocalIndex not ok at now=%d", nowUs)
		}
		if idx != 1 {
			t.Fatalf("now=%d: local index ending at room boundary = %d, want 1", nowUs, idx)
		}
	}
}

// The ADR-0006 hazard #426 fixes: the labeler was sample-aligned BEFORE the
// entry snap, and the snap shifted the grid past a boundary, leaving the
// offset off by one (labels one high → audio played one interval late).
func TestRoomLabelLocalIndexFixesSnapHazard(t *testing.T) {
	// Receipt at local 431s, early in room interval 54 (local [430,438)).
	// Pre-snap grid runs 3.9s late: beat 32 ↔ 441.9s. Sampling the local
	// index now (the old SetRoomAnchor path) reads interval 0 — but post-snap
	// the interval ending at the room boundary is 1.
	pre := &timelineGrid{beat0Us: 441_900_000 - 32*500_000, nowUs: 431_000_000, bpm: 120}
	if got := int64(math.Floor(pre.State().Beat / 16)); got != 0 {
		t.Fatalf("pre-snap sample = %d, want 0 (the hazard setup)", got)
	}
	// Post-snap (beat 32 ↔ 438s): the by-construction derivation reads 1.
	s := readyTimelineSteerer(&timelineGrid{beat0Us: 438_000_000 - 32*500_000, nowUs: 431_000_000, bpm: 120}, nil)
	idx, ok := s.roomLabelLocalIndex(16)
	if !ok {
		t.Fatal("roomLabelLocalIndex not ok")
	}
	if idx != 1 {
		t.Fatalf("post-snap by-construction index = %d, want 1 (pre-snap sample gave 0, off by one)", idx)
	}
}

func TestRoomLabelLocalIndexNotReady(t *testing.T) {
	g := &timelineGrid{beat0Us: 422_000_000, nowUs: 430_000_000, bpm: 120}
	s := NewSteerer(g, 120, nil, nil, nil)
	if _, ok := s.roomLabelLocalIndex(16); ok {
		t.Fatal("aligner without anchor/offset must not derive an index")
	}
	s = readyTimelineSteerer(g, nil)
	if _, ok := s.roomLabelLocalIndex(0); ok {
		t.Fatal("zero BPI must not derive an index")
	}
	dead := readyTimelineSteerer(&timelineGrid{beat0Us: 422_000_000, nowUs: 430_000_000, bpm: 0}, nil)
	if _, ok := dead.roomLabelLocalIndex(16); ok {
		t.Fatal("unusable Link state (BPM 0) must not derive an index")
	}
}

// Behavior level: entry conformance applies the by-construction label to the
// engine after the snap decision — the grid modeled moving under SnapGrid.
func TestEntryAppliesRoomLabelByConstruction(t *testing.T) {
	// Pre-snap grid 3.9s late (the #426 hazard setup).
	g := &timelineGrid{beat0Us: 441_900_000 - 32*500_000, nowUs: 431_000_000, bpm: 120}
	type alignment struct{ room, local int64 }
	var applied []alignment
	readyTimelineSteerer(g, func(roomIndex, localIndex int64) {
		applied = append(applied, alignment{roomIndex, localIndex})
	})
	if g.beat0Us != 422_000_000 {
		t.Fatalf("grid did not snap 3.9s earlier: beat0 = %d", g.beat0Us)
	}
	if len(applied) != 1 || applied[0] != (alignment{54, 1}) {
		t.Fatalf("applied = %v, want [{54 1}] (post-snap by-construction label)", applied)
	}
}

// --- Ported pure-helper tests (formerly align_test.go in package main) ---

func TestMeasureDeltaNotReady(t *testing.T) {
	now := time.Now()
	s, _, _ := newSteerer(14_000_000) // no anchor, no offset
	if _, ok := s.measureDelta(16); ok {
		t.Fatal("aligner without anchor/offset must not report a delta")
	}
	observe(s, now)
	if _, ok := s.measureDelta(0); ok {
		t.Fatal("zero BPI must not report a delta")
	}
}

func TestSlewGates(t *testing.T) {
	now := time.Now()
	if !slewAllowed(now, time.Time{}, time.Time{}) {
		t.Fatal("no history should allow slew")
	}
	if slewAllowed(now, now.Add(-time.Second), time.Time{}) {
		t.Fatal("settle window after snap should block slew")
	}
	if slewAllowed(now, time.Time{}, now.Add(-time.Second)) {
		t.Fatal("tempo gate should block slew")
	}
	if !slewAllowed(now, now.Add(-10*time.Second), now.Add(-10*time.Second)) {
		t.Fatal("settled snap + old tempo change should allow slew")
	}
}

func TestStateName(t *testing.T) {
	if s := stateName(5_000); s != "aligned" {
		t.Fatalf("5ms = %q, want aligned", s)
	}
	if s := stateName(15_000); s != "aligning" {
		t.Fatalf("15ms = %q, want aligning", s)
	}
	if s := stateName(-100_000); s != "drifted" {
		t.Fatalf("-100ms = %q, want drifted", s)
	}
}

func TestSnapshotTempoAdoptFunc(t *testing.T) {
	if !snapshotTempoAdopt(0, false, 110, 120) {
		t.Fatal("no anchor: differing tempo should be adopted")
	}
	if snapshotTempoAdopt(0, false, 120, 120) {
		t.Fatal("same tempo is always a no-op")
	}
	if !snapshotTempoAdopt(120, true, 120, 110) {
		t.Fatal("anchor-matching snapshot should be adopted")
	}
	if snapshotTempoAdopt(120, true, 110, 120) {
		t.Fatal("anchor-diverging snapshot must be ignored (oscillator)")
	}
}

// Two-peer snapshot oscillator (field report #424: 110↔120 flap every ~200ms
// with no DAWs involved). With naive adoption, crossing snapshots invert the
// pair every period forever; anchor-gated adoption converges on the room tempo.
func TestSnapshotAdoptionOscillator(t *testing.T) {
	step := func(a, b float64, adopt func(msg, local float64) bool) (float64, float64) {
		na, nb := a, b
		if adopt(b, a) {
			na = b
		}
		if adopt(a, b) {
			nb = a
		}
		return na, nb
	}

	naive := func(msg, local float64) bool { return math.Abs(msg-local) > tempoThreshold }
	a, b := 120.0, 110.0
	a, b = step(a, b, naive)
	if a != 110 || b != 120 {
		t.Fatalf("naive step 1: a=%v b=%v, want inverted 110/120", a, b)
	}
	a, b = step(a, b, naive)
	if a != 120 || b != 110 {
		t.Fatalf("naive step 2: a=%v b=%v, want re-inverted 120/110 (oscillation)", a, b)
	}

	gated := func(msg, local float64) bool { return snapshotTempoAdopt(120, true, msg, local) }
	a, b = 120.0, 110.0
	a, b = step(a, b, gated)
	if a != 120 || b != 120 {
		t.Fatalf("gated: a=%v b=%v, want both converged to anchor 120", a, b)
	}
	a, b = step(a, b, gated)
	if a != 120 || b != 120 {
		t.Fatalf("gated steady state drifted: a=%v b=%v", a, b)
	}
}

// The audibility invariant ADR-0006 promises and ADR-0008 makes structural:
// every tempo the slew writes is within SlewMaxFraction of the tempo it
// OBSERVED, so no single write can be heard. Keyed to the room tempo instead,
// the write is an absolute teleport whenever the session sits away from the
// room — a 120.2 session was written to 119.94, 3.75 cents, over four times the
// cap's own budget, and the user's change silently reverted.
//
// Driven over a long randomised walk of session tempo and grid error rather than
// one scripted case: the property has to hold for every combination, and the
// combinations are where the old design failed.
func TestEveryWriteIsWithinSlewAuthorityOfWhatWeObserved(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_000_000)
	observe(s, now)
	now = now.Add(snapSettle + time.Second)

	// A cheap deterministic walk — no math/rand, so a failure is reproducible.
	seed := uint64(12345)
	next := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	for i := 0; i < 4000; i++ {
		// The DAW/LAN moves the tempo around, sometimes far from the room's 120.
		if next(10) == 0 {
			f.state.BPM = 118 + float64(next(400))/100 // 118.00 .. 121.99
		}
		// And the grid error wanders across the deadband, both signs.
		f.timeAtBeat = 14_000_000 + int64(next(200_000)) - 100_000

		// Both references have to be captured BEFORE the tick: a settle writes
		// the base and clears it in the same tick, so reading episodeBase
		// afterwards would leave only the nudged tempo to measure against — and
		// a restore looks like a full-cap move from there.
		baseBefore, sessionBefore := s.episodeBase, f.state.BPM
		nWrites := len(f.tempos)
		s.Tick(16, now.Add(time.Duration(i)*time.Second))

		for _, wrote := range f.tempos[nWrites:] {
			// The reference is the tempo the episode is nudging around: the base
			// it already had, or the reading it just took if this write opened one.
			base := baseBefore
			if base == 0 {
				base = sessionBefore
			}
			if off := math.Abs(wrote-base) / base; off > interval.SlewMaxFraction+1e-9 {
				t.Fatalf("tick %d: wrote %.6f from base %.6f — %.6f off, past the %.6f cap",
					i, wrote, base, off, interval.SlewMaxFraction)
			}
		}
	}
	if len(f.tempos) == 0 {
		t.Fatal("no tempo writes at all over 4000 ticks — the walk never engaged the slew")
	}
	t.Logf("%d writes, all within %.4f%% of what was observed", len(f.tempos), interval.SlewMaxFraction*100)
}

// --- recovery from a grid that moved out from under us ---

// The wedge, and why it can no longer form: the gate used to compare the
// session tempo against the slew's own target and return before the settle that
// would clear it, so once something else moved the tempo the stale target held
// the gate shut against itself. Sixteen minutes of no alignment, in the field.
//
// Now an episode is dropped the moment the session stops sitting at our nudge,
// and the gate judges the session against the ROOM tempo, so the loop has no
// way to close. Dropping is free — no tempo write, no snap — so it needs no
// timeout to wait out an ordinary tempo change, which is what the old escape
// had to do. What must NOT happen is unchanged: seizing the tempo, or forcing
// an audible re-entry over a tempo nobody asked us to take.
func TestStaleSlewTargetIsDroppedNotWedged(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_020_000)
	observe(s, now)
	now = now.Add(snapSettle + time.Second)
	for i := 0; i < slewPersistenceTicks; i++ {
		s.Tick(16, now)
	}
	if s.slewTarget == 0 {
		t.Fatal("expected an active slew to set up the wedge")
	}

	// A real tempo move, well outside the tempo-settling band, shuts the gate.
	f.state.BPM = 130
	f.snaps, f.tempos = nil, nil

	s.Tick(16, now)
	if s.slewTarget != 0 {
		t.Fatal("stale slew target survived — it is what holds the gate shut against itself")
	}
	if s.episodeBase != 0 {
		t.Fatalf("stale episode base survived: %v", s.episodeBase)
	}
	if len(f.tempos) != 0 {
		t.Fatalf("wrote the tempo while another owner held it: %v", f.tempos)
	}
	if len(f.snaps) != 0 {
		t.Fatalf("%d audible snaps over a tempo change we should have waited out", len(f.snaps))
	}
	if s.entryPending {
		t.Fatal("forced a re-entry over a tempo nobody asked us to take")
	}

	// Once the room adopts the new tempo, the gate opens and steering resumes
	// against the correct baseline.
	f.timeAtBeat = 14_020_000
	s.OnAnchor(8_000_000, 0, 130, 16, now.Add(gateWedgeTimeout+2*time.Second))
	for i := 0; i < slewPersistenceTicks+1; i++ {
		s.Tick(16, now.Add(gateWedgeTimeout+time.Duration(20+i)*time.Second))
	}
	if len(f.tempos) == 0 {
		t.Fatal("still dormant after the room caught up to the new tempo")
	}
}

// While the gate is shut the UI must still get a live δ. Emitting only after
// the gate meant the badge froze on whatever δ was current when the state last
// changed, presenting a minutes-old reading as if it were now.
func TestGatedTicksStillReportState(t *testing.T) {
	now := time.Now()
	s, f, emits := newSteerer(14_000_000) // aligned
	observe(s, now)
	now = now.Add(snapSettle + time.Second)
	s.Tick(16, now)

	// Tempo moves away (gate shuts) AND the grid drifts badly.
	f.state.BPM = 130
	f.timeAtBeat = 16_600_000 // 2.6s late — the field's stuck badge
	*emits = nil

	s.Tick(16, now.Add(time.Second))
	if len(*emits) == 0 {
		t.Fatal("a gated tick reported nothing — the UI keeps showing a stale δ")
	}
	if got := lastEmit(emits); got != "drifted" {
		t.Fatalf("state = %q, want \"drifted\"", got)
	}
	if errMs := (*emits)[len(*emits)-1].errMs; math.Abs(errMs-2600) > 1 {
		t.Fatalf("reported δ = %.1f ms, want ~2600 (the live measurement)", errMs)
	}
}

// A beat jump is past anything the slew can walk back, so it has to re-enter
// conformance — the engine detects it, the session forwards it here.
func TestGridJumpReRunsEntryConformance(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_020_000)
	observe(s, now)
	now = now.Add(snapSettle + time.Second)
	for i := 0; i < slewPersistenceTicks; i++ {
		s.Tick(16, now)
	}

	s.OnGridJump(5.22, now)
	if !s.entryPending {
		t.Fatal("a grid jump must re-arm entry conformance")
	}
	if s.slewTarget != 0 {
		t.Fatal("a grid jump must abandon the in-flight slew")
	}

	// The re-entry snaps the grid onto the room again.
	f.snaps = nil
	f.timeAtBeat = 16_600_000 // the jump left us 2.6s late
	observe(s, now.Add(time.Second))
	if len(f.snaps) != 1 {
		t.Fatalf("expected exactly one entry snap after the jump, got %v", f.snaps)
	}
}

// Within one bucket the reported δ must track the real one. Emitting only on a
// bucket change froze the number at whatever δ was when the state last
// changed, so a grid recovering from 2.6s to 60ms still read 2605ms.
//
// This is the gate-OPEN path deliberately: while the gate is shut the rates
// differ and δ sweeps, so that path reports the bucket only.
func TestReportedErrorTracksDeltaInsideAState(t *testing.T) {
	now := time.Now()
	s, f, emits := newSteerer(16_600_000) // 2.6s late — "drifted"
	observe(s, now)
	now = now.Add(snapSettle + time.Second)
	s.Tick(16, now)
	if lastEmit(emits) != "drifted" {
		t.Fatalf("expected drifted, got %q", lastEmit(emits))
	}

	// Still drifted, but far closer than the number last reported.
	f.timeAtBeat = 14_060_000 // 60ms late
	*emits = nil
	now = now.Add(emitMinInterval + time.Second)
	s.Tick(16, now)

	if len(*emits) == 0 {
		t.Fatal("δ improved from 2.6s to 60ms inside one bucket and nothing was reported")
	}
	if errMs := (*emits)[len(*emits)-1].errMs; math.Abs(errMs-60) > 5 {
		t.Fatalf("reported δ = %.1f ms, want ~60", errMs)
	}

	// Movement below the step is jitter, not news.
	f.timeAtBeat = 14_062_000
	*emits = nil
	s.Tick(16, now.Add(emitMinInterval+time.Second))
	if len(*emits) != 0 {
		t.Fatalf("2ms of movement emitted %d events", len(*emits))
	}
}

// The wedge is specifically a stale slew target holding the gate shut against
// itself. With no slew in flight the gate is shut because something else
// genuinely owns the tempo — a DAW tempo ramp holds it shut for the length of
// the ramp — and that is the case the gate exists to respect. Escalating there
// yanks the tempo back and snaps the grid, audibly, every timeout.
func TestGateShutWithNoSlewNeverEscalates(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_000_000) // aligned: no slew will start
	observe(s, now)
	now = now.Add(snapSettle + time.Second)

	f.state.BPM = 130 // an external owner holds the tempo away from the room
	f.snaps, f.tempos = nil, nil
	for i := 0; i < 200; i++ { // ~3 minutes of ticks
		s.Tick(16, now.Add(time.Duration(i)*time.Second))
	}

	if len(f.snaps) != 0 {
		t.Fatalf("escalated with no slew in flight: %d grid snaps during a tempo ramp", len(f.snaps))
	}
	if len(f.tempos) != 0 {
		t.Fatalf("fought the tempo owner: %v", f.tempos)
	}
	if s.entryPending {
		t.Fatal("re-armed entry conformance over a tempo nobody asked us to take")
	}
}

// A disable/enable cycle must leave no episode state behind. This used to be a
// wedge-timer test; the timer no longer abandons anything, but the cycle it
// exercises exposed a sharper property: the persistence counter used to survive
// SetEnabled(false), so the first tick after re-enabling could write a tempo
// with no fresh confirmation at all — the very thing slewPersistenceTicks
// exists to prevent.
func TestDisableClearsEpisodeConfirmation(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_020_000)
	observe(s, now)
	now = now.Add(snapSettle + time.Second)
	for i := 0; i < slewPersistenceTicks; i++ {
		s.Tick(16, now)
	}
	if s.slewTarget == 0 {
		t.Fatal("expected an active slew")
	}

	s.SetEnabled(false, 16, now.Add(time.Second))
	if s.slewTarget != 0 || s.episodeBase != 0 || s.slewPendingCount != 0 {
		t.Fatalf("disable left episode state behind: target=%v base=%v pending=%d",
			s.slewTarget, s.episodeBase, s.slewPendingCount)
	}
	s.SetEnabled(true, 16, now.Add(time.Minute))
	observe(s, now.Add(time.Minute)) // entry runs, clearing entryPending
	f.tempos = nil

	// The first tick back must confirm from scratch, not inherit the old count.
	s.Tick(16, now.Add(time.Minute+snapSettle+time.Second))
	if len(f.tempos) != 0 {
		t.Fatalf("wrote a tempo on the first tick after re-enabling, with no fresh confirmation: %v", f.tempos)
	}
	s.Tick(16, now.Add(time.Minute+snapSettle+2*time.Second))
	if s.slewTarget == 0 {
		t.Fatal("slew never resumed after re-enabling")
	}
}

// Abandoning a slew must put the tempo back. The slew holds the session away
// from the room tempo, so clearing the bookkeeping alone strands it there with
// nothing tracking it — SetEnabled's restore keys off the very target cleared.
func TestAbandonedSlewRestoresTheTempo(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_020_000)
	observe(s, now)
	now = now.Add(snapSettle + time.Second)
	for i := 0; i < slewPersistenceTicks; i++ {
		s.Tick(16, now)
	}
	slewed := lastTempo(f)
	if s.slewTarget == 0 || approxEq(slewed, 120) {
		t.Fatalf("expected the slew to hold the tempo off 120, got %v", f.tempos)
	}

	s.OnGridJump(5.22, now)

	if got := lastTempo(f); !approxEq(got, 120) {
		t.Fatalf("tempo left at %v after abandoning the slew, want the room's 120", got)
	}
}

// A δ that oscillates by more than the step must not emit every tick: the
// reference is the last *emitted* value, so the step alone bounds nothing.
func TestOscillatingDeltaDoesNotStreamEvents(t *testing.T) {
	now := time.Now()
	s, f, emits := newSteerer(14_040_000) // 40ms: "drifted"
	observe(s, now)
	now = now.Add(snapSettle + time.Second)
	s.Tick(16, now)
	*emits = nil

	// ±12ms of jitter inside one bucket, one tick a second.
	for i := 0; i < 30; i++ {
		// 40ms vs 60ms: 20ms apart, so past the step, but both well inside
		// the same bucket — jitter, not a state change.
		if i%2 == 0 {
			f.timeAtBeat = 14_040_000
		} else {
			f.timeAtBeat = 14_060_000
		}
		s.Tick(16, now.Add(time.Duration(i+1)*time.Second))
	}
	if len(*emits) > 30/int(emitMinInterval/time.Second)+1 {
		t.Fatalf("jitter streamed %d events over 30 ticks", len(*emits))
	}
}

// --- long-run behaviour: one tempo, one hour, peers coming and going ---

// The real workload is an hour on a fixed tempo, not the handful of ticks the
// other tests cover. Over that span a quiet, aligned grid must stay quiet: no
// grid snaps (a snap is audible), no tempo writes, no spurious re-entry, and a
// UI event budget that doesn't grow with time.
func TestAnHourOnAFixedTempoStaysQuiet(t *testing.T) {
	start := time.Now()
	s, f, emits := newSteerer(14_000_000) // aligned
	observe(s, start)
	f.snaps, f.tempos, *emits = nil, nil, nil

	for i := 0; i < 3600; i++ {
		s.Tick(16, start.Add(snapSettle+time.Duration(i)*time.Second))
	}

	if len(f.snaps) != 0 {
		t.Fatalf("%d audible grid snaps during a quiet hour", len(f.snaps))
	}
	if len(f.tempos) != 0 {
		t.Fatalf("wrote the tempo %d times with nothing to correct: %v", len(f.tempos), f.tempos)
	}
	if s.entryPending {
		t.Fatal("entry conformance re-armed itself over a quiet hour")
	}
	if len(*emits) > 2 {
		t.Fatalf("%d UI events over a quiet hour", len(*emits))
	}
}

// Peers joining and leaving is the event that actually happens in these
// sessions, and each one re-phases the shared Link timeline. Every merge must
// recover — one snap, tempo back on the room's — and an hour of them must not
// leave the steerer wedged or drifting in its own state.
func TestRepeatedMergesOverAnHourEachRecover(t *testing.T) {
	start := time.Now()
	s, f, _ := newSteerer(14_000_000)
	observe(s, start)

	const merges = 12 // one every five minutes
	for m := 0; m < merges; m++ {
		at := start.Add(time.Duration(m) * 5 * time.Minute)

		// A joining peer shoves the grid 2.6s and parks the session tempo at
		// its own — exactly the field case.
		f.timeAtBeat = 16_600_000
		f.state.BPM = 120.0
		f.snaps = nil
		s.OnGridJump(5.22, at)
		if !s.entryPending {
			t.Fatalf("merge %d did not re-arm entry conformance", m)
		}

		// Entry runs on the next anchor/pong and snaps the grid back.
		observe(s, at.Add(time.Second))
		if len(f.snaps) != 1 {
			t.Fatalf("merge %d: %d snaps, want exactly 1", m, len(f.snaps))
		}
		if s.entryPending {
			t.Fatalf("merge %d: entry still pending after conformance ran", m)
		}

		// Grid is aligned again; the rest of the five minutes is steady state.
		f.timeAtBeat = 14_000_000
		for i := 0; i < 300; i++ {
			s.Tick(16, at.Add(snapSettle+time.Duration(i)*time.Second))
		}
		if s.slewTarget != 0 {
			t.Fatalf("merge %d: still slewing five minutes later (target %v)", m, s.slewTarget)
		}
		if got := lastTempo(f); got != 0 && !approxEq(got, 120) {
			t.Fatalf("merge %d: tempo left at %v, want the room's 120", m, got)
		}
	}

	if s.gateShutSince != (time.Time{}) {
		t.Fatal("wedge timer left running after an hour of recoveries")
	}
}

// A peer on a jittery clock measures a noisy δ — the WAN offset estimate
// teleports, which is what slewPersistenceTicks exists for. Over an hour that
// noise must not turn into tempo flapping the whole room hears, and must never
// snap the grid: a snap is audible, and noise is not evidence of a real move.
func TestJitteryClockDoesNotFlapTempoOrSnap(t *testing.T) {
	start := time.Now()
	s, f, emits := newSteerer(14_000_000)
	observe(s, start)
	f.snaps, f.tempos, *emits = nil, nil, nil

	// δ spikes alternating either side of the deadband, ±40ms, once a second
	// for an hour — a teleporting offset estimate, not real drift.
	offsets := []int64{40_000, -40_000, 30_000, -50_000, 45_000, -35_000}
	for i := 0; i < 3600; i++ {
		f.timeAtBeat = 14_000_000 + offsets[i%len(offsets)]
		s.Tick(16, start.Add(snapSettle+time.Duration(i)*time.Second))
	}

	t.Logf("hour of jitter: %d snaps, %d tempo writes, %d UI events",
		len(f.snaps), len(f.tempos), len(*emits))
	if len(f.snaps) != 0 {
		t.Fatalf("clock jitter snapped the grid %d times — audible, and on noise", len(f.snaps))
	}
	// The slew may act on a confirmed direction, but alternating noise must not
	// keep it moving the tempo: real drift persists, spikes flip sign.
	if len(f.tempos) > 60 {
		t.Fatalf("tempo written %d times in an hour of jitter — flapping the room's tempo", len(f.tempos))
	}
	if len(*emits) > 3600/int(emitMinInterval/time.Second)+2 {
		t.Fatalf("%d UI events over an hour of jitter", len(*emits))
	}
}

// Detection is a heuristic over a sampled clock, so a single bad reading must
// not be able to snap the grid over and over on a peer whose machine stalls.
func TestRepeatedJumpReportsDoNotSnapRepeatedly(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(16_600_000) // 2.6s out, so entry would snap
	observe(s, now)
	f.snaps = nil

	// Ten "jumps" in quick succession, as a stalling machine might report.
	for i := 0; i < 10; i++ {
		at := now.Add(time.Duration(i) * 200 * time.Millisecond)
		s.OnGridJump(5.22, at)
		observe(s, at)
	}
	if len(f.snaps) > 1 {
		t.Fatalf("%d grid snaps from a burst of jump reports, want at most 1", len(f.snaps))
	}

	// Past the settle window a genuine new jump is still honoured.
	f.snaps = nil
	s.OnGridJump(5.22, now.Add(snapSettle+time.Second))
	if !s.entryPending {
		t.Fatal("a real jump after the window was ignored")
	}
}

// A peer whose clock wanders around one intended tempo used to gate the slew
// off entirely — the flat 0.01 BPM band treated a tenth of a BPM the same as a
// deliberate 120→122 move — so phase drifted uncorrected for the whole hour.
// With a proportional band the wobble is inside the noise and the slew keeps
// working, without WAIL ever fighting the peer for the tempo knob.
func TestWanderingPeerTempoStillLetsTheSlewCorrectPhase(t *testing.T) {
	start := time.Now()
	s, f, _ := newSteerer(14_020_000) // a real 20ms phase error to close
	observe(s, start)
	f.snaps, f.tempos = nil, nil

	for i := 0; i < 120; i++ {
		// The peer re-asserts their own wandering tempo every tick.
		if i%2 == 0 {
			f.state.BPM = 120.0
		} else {
			f.state.BPM = 119.9
		}
		s.Tick(16, start.Add(snapSettle+time.Duration(i)*time.Second))
	}

	if len(f.tempos) == 0 {
		t.Fatal("the slew stayed dormant through the wobble — phase would drift all session")
	}
	if len(f.snaps) != 0 {
		t.Fatalf("%d audible snaps during an ordinary clock wobble", len(f.snaps))
	}
	if s.entryPending {
		t.Fatal("re-armed entry conformance over a wobble")
	}

	// A genuine tempo move must still gate the slew: 120→124 is 3.3%, far
	// outside the band, and WAIL must not fight it.
	f.tempos = nil
	for i := 0; i < 30; i++ {
		f.state.BPM = 124
		s.Tick(16, start.Add(10*time.Minute+time.Duration(i)*time.Second))
	}
	if len(f.tempos) != 0 {
		t.Fatalf("fought a real tempo change: %v", f.tempos)
	}
}
