package main

// Deterministic multi-LAN simulation of WAIL's tempo path (ADR-0009): one WAIL
// per LAN, coupled only through a simulated relay, replaying the jitter shapes
// this repo has actually observed so the tempo thresholds can be set from
// measurement instead of argument.
//
// It drives the REAL TempoChangeDetector, alignBridge, align.Steerer and
// interval.GridAligner — the question ADR-0009 asks is exactly how those
// interact, so a reimplementation of any of them would measure the wrong thing.
// Only the session select loop is transcribed (see simPeer.step, which cites
// the session.go line for each of its six call sites).
//
// Time is virtual and stepped at the Link poll rate; nothing here reads a wall
// clock or starts a goroutine, so a ten-minute scenario is deterministic and
// runs in milliseconds. Ground truth matters most: the sim knows both grids
// exactly, so it measures the TRUE alignment error, not the peer's
// RTT-corrupted estimate of it — the gap between the two is itself a metric.

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/align"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

const (
	// Cadences, in simulation steps. One step is linkPollInterval, so these
	// derive from the production constants rather than restating them in ms.
	simStepUs        = int64(linkPollInterval / time.Microsecond) // 20ms, 50Hz
	simSnapshotEvery = snapshotIntervalTicks                      // 200ms (link_types.go)
	simAlignEvery    = 50                                         // 1s (session.go:319)
	simPingEvery     = PingIntervalMs * 1000 / int(simStepUs)     // 2s (clock.go)

	// The room: 4 bars x 4 beats = BPI 16, so one interval is 8s at 120 BPM.
	simBars    = 4
	simQuantum = 4.0
	simRoomBPM = 120.0
)

func simBPI() float64 { return interval.Config{Bars: simBars, Quantum: simQuantum}.BeatsPerInterval() }

// simEpoch is the virtual wall clock's origin. Gates inside the detector and
// steerer are all relative durations, so a fixed origin keeps runs reproducible.
var simEpoch = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func simWall(serverUs int64) time.Time {
	return simEpoch.Add(time.Duration(serverUs) * time.Microsecond)
}

// --- the local Link session ------------------------------------------------

// tempoWrite records one SetTempo, tagged with who asked for it. The tag is how
// steering writes (which must stay inside the slew's authority to be inaudible)
// are told apart from room-wide tempo commits (which are supposed to be heard).
type tempoWrite struct {
	atUs     int64
	from, to float64
	source   string // "steer" | "remote" | "snapshot" | "declared"
}

func (w tempoWrite) fraction() float64 {
	if w.from <= 0 {
		return 0
	}
	return math.Abs(w.to-w.from) / w.from
}

// simLink is a Link session on one LAN, satisfying LinkBridgeInterface so the
// real alignBridge can wrap it. The beat is an accumulator advanced against the
// local clock rather than beat = (now-origin)/period: a tempo change must leave
// the beat where it is and only change its rate, which the closed form gets
// wrong retroactively.
type simLink struct {
	bpm      float64
	beat     float64
	atUs     int64 // LOCAL-clock time at which beat holds
	detector *TempoChangeDetector

	nowWall func() time.Time // virtual clock, for the echo guard
	source  string           // tag applied to the next SetTempo
	writes  []tempoWrite
	snaps   []int64
}

func newSimLink(bpm float64, beat float64, atUs int64, d *TempoChangeDetector, nowWall func() time.Time) *simLink {
	return &simLink{bpm: bpm, beat: beat, atUs: atUs, detector: d, nowWall: nowWall, source: "steer"}
}

// advanceTo rolls the beat forward to a local-clock instant. Every read of
// State/TimeAtBeat must be preceded by one, which the driver does per step.
func (l *simLink) advanceTo(localUs int64) {
	if localUs <= l.atUs {
		return
	}
	l.beat += float64(localUs-l.atUs) / 1e6 * l.bpm / 60.0
	l.atUs = localUs
}

func (l *simLink) State() LinkState {
	return LinkState{BPM: l.bpm, Beat: l.beat, TimestampUs: l.atUs, Quantum: simQuantum}
}

func (l *simLink) TimeAtBeat(beat float64) int64 {
	if l.bpm <= 0 {
		return l.atUs
	}
	return l.atUs + int64(math.Round((beat-l.beat)*60.0/l.bpm*1e6))
}

// SetTempo mirrors LinkBridge.SetTempo (link_real.go:56): the beat stays put and
// only its rate changes, and the detector baseline plus echo guard move with the
// write. That coupling is the thing ADR-0009's NoteSteering revises, so the sim
// has to carry it rather than assume it away.
func (l *simLink) SetTempo(bpm float64) {
	l.writes = append(l.writes, tempoWrite{atUs: l.atUs, from: l.bpm, to: bpm, source: l.source})
	l.bpm = bpm
	l.detector.SetLastTempo(bpm)
	l.detector.ArmEchoGuard(l.nowWall().Add(echoGuardDuration))
}

// disturb moves the session tempo the way the DAW or Link itself would —
// deliberately NOT through SetTempo, because the detector is supposed to
// discover this by observation, and the echo guard must not be armed by it.
func (l *simLink) disturb(bpm float64) { l.bpm = bpm }

// SnapGrid shifts the local grid earlier by deltaUs (positive = grid runs late),
// which in beat terms advances the beat by that much time at the current tempo.
func (l *simLink) SnapGrid(deltaUs int64) {
	l.snaps = append(l.snaps, deltaUs)
	l.beat += float64(deltaUs) / 1e6 * l.bpm / 60.0
}

// jumpBeats models the local timeline moving out from under us (a Link session
// merge, a transport relocate) without any tempo change.
func (l *simLink) jumpBeats(beats float64) { l.beat += beats }

func (l *simLink) Detector() *TempoChangeDetector { return l.detector }

// Unused by the tempo path, present to satisfy LinkBridgeInterface.
func (l *simLink) Enable()                   {}
func (l *simLink) Disable()                  {}
func (l *simLink) ForceBeat(float64, *int64) {}
func (l *simLink) SpawnPoller(context.Context) (chan<- LinkCommand, <-chan LinkEvent) {
	return nil, nil
}

// --- the relay -------------------------------------------------------------

// simRoomClock is a copy of signaling-server/roomclock.go, transition pin
// included. It is the third copy in the tree (internal/interval's anchorsim is
// the second) because the relay is a separate Go module; the server's own file
// header records that duplication as a deliberate trade-off.
type simRoomClock struct {
	index    int64
	atUs     int64
	tempo    float64
	bpi      float64
	pinIdx   int64
	pinUntil int64 // 0 = no pin
}

func (rc *simRoomClock) boundaryUs(idx int64) int64 {
	sec := float64(idx-rc.index) * rc.bpi * 60.0 / rc.tempo
	return rc.atUs + int64(math.Round(sec*1e6))
}

func (rc *simRoomClock) indexAt(nowUs int64) int64 {
	if rc.pinUntil > 0 && nowUs < rc.pinUntil {
		return rc.pinIdx
	}
	elapsedBeats := float64(nowUs-rc.atUs) / 1e6 * rc.tempo / 60.0
	k := rc.index + int64(math.Floor(elapsedBeats/rc.bpi))
	for rc.boundaryUs(k) > nowUs {
		k--
	}
	for rc.boundaryUs(k+1) <= nowUs {
		k++
	}
	return k
}

// reanchor quantizes a tempo change to the next boundary and pins the index
// across the transition, as the relay does.
func (rc *simRoomClock) reanchor(nowUs int64, tempo float64) {
	next := rc.indexAt(nowUs) + 1
	nextBoundary := rc.boundaryUs(next)
	rc.pinIdx, rc.pinUntil = next-1, nextBoundary
	rc.index, rc.atUs, rc.tempo = next, nextBoundary, tempo
}

func (rc *simRoomClock) periodUs() float64 { return rc.bpi * 60.0 / rc.tempo * 1e6 }

// simRelay owns the room clock and fans messages out to peers. Peers never see
// each other directly — separate LANs, coupled only here.
type simRelay struct {
	clk     simRoomClock
	peers   []*simPeer
	nowUs   int64
	anchors int
	tempos  int // TempoChange messages relayed (each re-anchors the room)
}

func newSimRelay(bpm float64) *simRelay {
	return &simRelay{clk: simRoomClock{index: 0, atUs: 0, tempo: bpm, bpi: simBPI()}}
}

// broadcastAnchor delivers an interval_anchor to every peer, as the relay does
// after any clock-affecting message (interval_clock.go:127).
func (r *simRelay) broadcastAnchor() {
	r.anchors++
	idx := r.clk.indexAt(r.nowUs)
	next := r.clk.boundaryUs(idx + 1)
	for _, p := range r.peers {
		p.onAnchor(idx, next, r.clk.tempo)
	}
}

// tempoChange relays a peer's TempoChange: the room clock re-anchors, the other
// peers apply it, and everyone gets the fresh anchor.
func (r *simRelay) tempoChange(from *simPeer, bpm float64) {
	r.tempos++
	r.clk.reanchor(r.nowUs, bpm)
	for _, p := range r.peers {
		if p != from { // mesh.Broadcast excludes the sender
			p.onRemoteTempo(bpm)
		}
	}
	r.broadcastAnchor()
}

func (r *simRelay) snapshot(from *simPeer, bpm float64) {
	for _, p := range r.peers {
		if p != from {
			p.onSnapshot(bpm)
		}
	}
}

// --- one WAIL peer ---------------------------------------------------------

// simPeer is one WAIL on its own LAN: a real detector, a real steerer over the
// real alignBridge, and the session loop's tempo wiring.
type simPeer struct {
	name  string
	relay *simRelay
	link  *simLink
	steer *align.Steerer

	// Clock domain. The peer's local clock is the server clock skewed by a
	// constant offset and a crystal rate error; the grid aligner only ever sees
	// it through RTT-biased pongs, which is where shape 7's false δ comes from.
	baseOffsetUs int64
	ppm          float64
	upUs, downUs int64 // one-way latencies; asymmetry biases the offset estimate

	// disturb injects a jitter shape each step, moving tempo or grid the way the
	// DAW or Link would — never through WAIL's own write path.
	disturb func(p *simPeer, step int)
	// declaredOnly suppresses inferred tempo reports (see simConfig).
	declaredOnly bool

	// Metrics.
	deltas    []int64 // ground-truth δ, sampled each step
	reports   []float64
	driftedAt int
	samples   int
}

// localUs maps a server-clock instant into this peer's local clock.
func (p *simPeer) localUs(serverUs int64) int64 {
	return int64(float64(serverUs)*(1+p.ppm*1e-6)) + p.baseOffsetUs
}

// trueOffsetUs is server − local: what the grid aligner is trying to estimate,
// known here exactly.
func (p *simPeer) trueOffsetUs(serverUs int64) int64 { return serverUs - p.localUs(serverUs) }

func (p *simPeer) now() time.Time { return simWall(p.relay.nowUs) }

// --- inbound relay messages (transcribed from session.go) ---

func (p *simPeer) onAnchor(index, nextBoundaryUs int64, bpm float64) {
	// session.go:827
	p.steer.OnAnchor(nextBoundaryUs, index, bpm, simBPI(), p.now())
}

func (p *simPeer) onRemoteTempo(bpm float64) {
	// session.go:846-847
	p.steer.NoteTempoCommitted(bpm, p.now())
	p.write("remote", bpm)
}

func (p *simPeer) onSnapshot(bpm float64) {
	// session.go:857-860
	if p.steer.SnapshotTempoAdopt(bpm) {
		p.steer.NoteTempoCommitted(bpm, p.now())
		p.write("snapshot", bpm)
	}
}

// declare models a tempo change made in WAIL's own UI: intent by construction,
// so it broadcasts immediately and never passes through the detector at all
// (ADR-0009 decision 3). This is the sanctioned path a declared-only
// architecture leaves for changing the room tempo — and it is the path
// App.ChangeBPM does NOT currently take, which is why a UI tempo change never
// reaches the room today.
func (p *simPeer) declare(bpm float64) {
	p.reports = append(p.reports, bpm)
	p.relay.tempoChange(p, bpm)
	p.steer.NoteTempoCommitted(bpm, p.now())
	p.write("declared", bpm)
}

// write applies a tempo commit through the raw bridge, tagged for the metrics.
// Steering writes bypass this — they arrive via alignBridge and keep the
// default "steer" tag.
func (p *simPeer) write(source string, bpm float64) {
	p.link.source = source
	p.link.SetTempo(bpm)
	p.link.source = "steer"
}

// --- the step: one 20ms Link poll ---

func (p *simPeer) step(step int) {
	serverUs := p.relay.nowUs
	p.link.advanceTo(p.localUs(serverUs))

	if p.disturb != nil {
		p.disturb(p, step)
	}

	// Poll → detect → broadcast (link_types.go:193, session.go:1010-1019).
	// Check runs even when its verdict is discarded: it is what feeds the running
	// mean the tempo-settling gate reads.
	bpm, changed := p.link.Detector().Check(p.link.State().BPM, p.now())
	if changed && !p.declaredOnly {
		if math.Abs(bpm-p.steer.CurrentBPM()) > 0.01 { // session.go:1011
			p.reports = append(p.reports, bpm)
			p.relay.tempoChange(p, bpm)              // session.go:1016
			p.steer.NoteTempoCommitted(bpm, p.now()) // session.go:1017
		}
	}

	if step%simSnapshotEvery == 0 {
		p.relay.snapshot(p, p.link.State().BPM) // session.go:1023
	}

	if step%simPingEvery == 0 {
		p.pong(serverUs) // session.go:805
	}

	if step%simAlignEvery == 0 {
		p.steer.Tick(simBPI(), p.now()) // session.go:1044
	}

	p.sample(serverUs)
}

// pong completes one relay time exchange. The server stamps on arrival, so the
// peer's estimate carries a (up−down)/2 bias — the whole of shape 7.
func (p *simPeer) pong(serverUs int64) {
	sentLocal := p.localUs(serverUs)
	stampServer := serverUs + p.upUs
	recvLocal := p.localUs(serverUs + p.upUs + p.downUs)
	rtt := recvLocal - sentLocal
	p.link.advanceTo(recvLocal)
	p.steer.OnServerPong(stampServer+rtt/2, rtt, simBPI(), p.now())
}

// --- ground truth ---

// trueDelta is how late this peer's grid runs against the room grid, in
// microseconds, computed from both clocks exactly. The steerer's own δ differs
// by its offset-estimate error, and comparing the two is how the harness
// measures RTT bias rather than assuming it.
func (p *simPeer) trueDelta(serverUs int64) int64 {
	bpi := simBPI()
	st := p.link.State()
	if st.BPM <= 0 {
		return 0
	}
	boundaryBeat := (math.Floor(st.Beat/bpi) + 1) * bpi
	tServer := p.link.TimeAtBeat(boundaryBeat) + p.trueOffsetUs(serverUs)

	rc := &p.relay.clk
	period := rc.periodUs()
	k := int64(math.Round(float64(tServer-rc.atUs) / period))
	return interval.WrapPhase(tServer-rc.boundaryUs(rc.index+k), int64(math.Round(period)))
}

// simWarmup is skipped by the δ statistics. A peer starts on an arbitrary grid
// phase and cannot measure anything until entry conformance has an anchor and
// three pongs, so the first seconds are a guaranteed multi-second error that
// would otherwise dominate every max and mean in the table with the same
// meaningless number. The entry snap itself is still counted, in `snaps`.
const simWarmup = 30 * time.Second

func (p *simPeer) sample(serverUs int64) {
	d := p.trueDelta(serverUs)
	if serverUs < simWarmup.Microseconds() {
		return
	}
	p.deltas = append(p.deltas, d)
	p.samples++
	if abs64i(d) > interval.AlignThresholdUs {
		p.driftedAt++
	}
}

// --- the run ---------------------------------------------------------------

type simPeerSpec struct {
	name         string
	beat0        float64 // starting beat, so grids do not begin coincident
	bpm          float64
	baseOffsetUs int64
	ppm          float64
	upUs, downUs int64
	disturb      func(p *simPeer, step int)
	// detector overrides the detector tuning for this peer; the zero value is
	// production's. Scenarios use it to replay a historical threshold regime.
	detector tempoDetectorConfig
}

type simConfig struct {
	name     string
	duration time.Duration
	roomBPM  float64
	peers    []simPeerSpec
	// declaredOnly models the grid-alignment-only architecture: WAIL never
	// infers a tempo change from what it observes, so the room tempo moves only
	// when someone declares one. Entry conformance still adopts the room tempo at
	// join and the grid slew is untouched — the question this mode answers is
	// whether alignment alone holds the grids together once tempo agreement is
	// established, or whether δ ramps away without inference to maintain it.
	declaredOnly bool
}

type simResult struct {
	name    string
	steps   int
	anchors int
	tempos  int // room re-anchors
	// roomExcursionBPM is how far the room tempo was ever dragged from the
	// tempo the room was trying to hold — the room-wide cost of a peer's local
	// noise being mistaken for intent.
	roomExcursionBPM float64
	roomEndBPM       float64
	// heardSkewMaxMs is the worst disagreement between the two peers' grids —
	// what a musician hears, as opposed to either peer's error against the room.
	heardSkewMaxMs float64
	peers          []peerResult
}

type peerResult struct {
	name string
	// Ground-truth phase error against the room grid.
	maxAbsDeltaMs, p95AbsDeltaMs, meanAbsDeltaMs float64
	// endAbsDeltaMs is |δ| at the end of the run. Against maxAbsDeltaMs it says
	// whether alignment recovered or is still ramping away — the question that
	// decides whether grid alignment alone can carry the architecture.
	endAbsDeltaMs  float64
	timeDriftedPct float64
	// What WAIL did about it.
	reports          int
	steerWrites      int
	maxSteerFraction float64 // audibility: 0.0005 is the ADR-0006 bound
	maxAnyWriteBPM   float64
	snaps            int
	endBPM           float64
}

// runSim executes one scenario and returns its measurements.
func runSim(cfg simConfig) simResult {
	roomBPM := cfg.roomBPM
	if roomBPM == 0 {
		roomBPM = simRoomBPM
	}
	relay := newSimRelay(roomBPM)

	for _, spec := range cfg.peers {
		bpm := spec.bpm
		if bpm == 0 {
			bpm = roomBPM
		}
		p := &simPeer{
			name: spec.name, relay: relay,
			baseOffsetUs: spec.baseOffsetUs, ppm: spec.ppm,
			upUs: spec.upUs, downUs: spec.downUs, disturb: spec.disturb,
			declaredOnly: cfg.declaredOnly,
		}
		d := newTunedTempoChangeDetector(bpm, spec.detector)
		p.link = newSimLink(bpm, spec.beat0, p.localUs(0), d, p.now)
		p.steer = align.NewSteerer(alignBridge{lb: p.link}, bpm, nil, nil, nil)
		relay.peers = append(relay.peers, p)
	}

	// The founder seeds the room tempo at join (ADR-0006), so every peer starts
	// with an anchor in hand — matching a room that is already up.
	relay.broadcastAnchor()

	steps := int(cfg.duration.Microseconds() / simStepUs)
	var roomExcursion float64
	for step := 0; step < steps; step++ {
		relay.nowUs = int64(step) * simStepUs
		for _, p := range relay.peers {
			p.step(step)
		}
		if d := math.Abs(relay.clk.tempo - roomBPM); d > roomExcursion {
			roomExcursion = d
		}
	}

	res := simResult{
		name: cfg.name, steps: steps, anchors: relay.anchors, tempos: relay.tempos,
		roomExcursionBPM: roomExcursion, roomEndBPM: relay.clk.tempo,
	}
	for _, p := range relay.peers {
		res.peers = append(res.peers, p.result())
	}
	for i := range relay.peers {
		for j := i + 1; j < len(relay.peers); j++ {
			if s := heardSkewMaxMs(relay.peers[i], relay.peers[j]); s > res.heardSkewMaxMs {
				res.heardSkewMaxMs = s
			}
		}
	}
	return res
}

func (p *simPeer) result() peerResult {
	r := peerResult{name: p.name, reports: len(p.reports), snaps: len(p.link.snaps), endBPM: p.link.bpm}
	if p.samples > 0 {
		r.timeDriftedPct = 100 * float64(p.driftedAt) / float64(p.samples)
	}
	abs := make([]float64, len(p.deltas))
	var sum float64
	for i, d := range p.deltas {
		abs[i] = math.Abs(float64(d)) / 1000
		sum += abs[i]
		if abs[i] > r.maxAbsDeltaMs {
			r.maxAbsDeltaMs = abs[i]
		}
	}
	if n := len(p.deltas); n > 0 {
		r.endAbsDeltaMs = math.Abs(float64(p.deltas[n-1])) / 1000
	}
	if len(abs) > 0 {
		r.meanAbsDeltaMs = sum / float64(len(abs))
		sort.Float64s(abs)
		r.p95AbsDeltaMs = abs[int(0.95*float64(len(abs)-1))]
	}
	for _, w := range p.link.writes {
		if d := math.Abs(w.to - w.from); d > r.maxAnyWriteBPM {
			r.maxAnyWriteBPM = d
		}
		if w.source == "steer" {
			r.steerWrites++
			if f := w.fraction(); f > r.maxSteerFraction {
				r.maxSteerFraction = f
			}
		}
	}
	return r
}

// heardSkewMaxMs is the worst pairwise disagreement between two peers' grids —
// what a musician actually hears, as opposed to either peer's error against the
// room. Peers are sampled in lockstep, so index i is the same instant for both.
func heardSkewMaxMs(a, b *simPeer) float64 {
	n := len(a.deltas)
	if len(b.deltas) < n {
		n = len(b.deltas)
	}
	var worst float64
	for i := 0; i < n; i++ {
		if s := math.Abs(float64(a.deltas[i]-b.deltas[i])) / 1000; s > worst {
			worst = s
		}
	}
	return worst
}

func abs64i(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
