package align

import (
	"math"
	"testing"
	"time"
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

// observe feeds the anchor and one server pong (entry fires if pending).
func observe(s *Steerer, now time.Time) {
	s.OnAnchor(8_000_000, 0, 120, 16, now)
	s.OnServerPong(10_000_000, 16, now)
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
	s.OnServerPong(10_000_000, 16, now)
	if len(f.snaps) != 1 || f.snaps[0] != 100_000 {
		t.Fatalf("snaps = %v, want [100000]", f.snaps)
	}
	if got := lastEmit(emits); got != "drifted" {
		t.Fatalf("last emit = %q, want drifted", got)
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
	s.OnServerPong(10_000_000, 16, now.Add(time.Second))
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
	s.Tick(16, now.Add(4*time.Second))
	if len(f.tempos) != 1 || !approxEq(f.tempos[0], 120*1.003) {
		t.Fatalf("tempos = %v, want [120.36] (clamped slew)", f.tempos)
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
	s.Tick(16, now.Add(6*time.Second))
	if len(f.tempos) != 1 || !approxEq(f.tempos[0], 120*1.003) {
		t.Fatalf("tempos = %v, want [120.36] after settle", f.tempos)
	}
}

func TestSlewSettleRestoresRoomTempo(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // slew active
	if len(f.tempos) != 1 {
		t.Fatalf("tempos = %v, want slew active", f.tempos)
	}
	// The slew must not touch the committed-tempo record.
	if got := s.CurrentBPM(); got != 120 {
		t.Fatalf("CurrentBPM = %v after slew nudge, want 120 (committed tempo untouched)", got)
	}
	f.timeAtBeat = 14_000_000 // drift closed
	s.Tick(16, now.Add(7*time.Second))
	if got := lastTempo(f); got != 120 {
		t.Fatalf("settled tempo = %v, want 120 (exact room tempo restored)", got)
	}
}

func TestSlewNudgeDoesNotArmTempoGate(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // slew → 120.36
	if len(f.tempos) != 1 {
		t.Fatalf("tempos = %v, want slew active", f.tempos)
	}
	// The slew's own SetTempo must not arm the 3s gate against itself:
	// a changed δ one second later still steers.
	f.timeAtBeat = 14_015_000 // δ=15ms → target 120.225 ≠ current slew target
	s.Tick(16, now.Add(7*time.Second))
	if len(f.tempos) != 2 || !approxEq(f.tempos[1], 120*1.001875) {
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
	s.Tick(16, now.Add(11*time.Second))
	if len(f.tempos) != 1 {
		t.Fatalf("slew did not resume after tempo agreement: %v", f.tempos)
	}
}

func TestSlewHoldsOffWhenDraggedFromOwnTarget(t *testing.T) {
	now := time.Now()
	s, f, _ := newSteerer(14_100_000)
	observe(s, now)
	s.Tick(16, now.Add(6*time.Second)) // slew active: target 120.36
	if len(f.tempos) != 1 {
		t.Fatalf("tempos = %v, want slew active", f.tempos)
	}
	// The session sits at the slew's own target: keep steering (no gate).
	f.state.BPM = 120 * 1.003
	f.timeAtBeat = 14_015_000 // δ=15ms → different nudge
	s.Tick(16, now.Add(7*time.Second))
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
	s.Tick(16, now.Add(6*time.Second)) // slew active at 120.36
	s.SetEnabled(false, 16, now.Add(7*time.Second))
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
	s.OnServerPong(10_000_000, 16, now.Add(time.Second))
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
	s.OnServerPong(g.nowUs+2_000_000, 16, now) // server = local + 2s
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
