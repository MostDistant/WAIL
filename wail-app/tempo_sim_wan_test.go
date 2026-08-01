package main

// The WAN Link group, simulated on its own: N WAIL instances forming an
// isolated Link-LIKE session across the relay at 100–300ms RTT, with no LAN
// devices in the picture at all (the dual-session model — WAIL is a peer of
// its LAN Link session AND of a WAN session with the other WAILs).
//
// "Does Link even work at that latency?" — measured against the vendored
// source, half of it does and half refuses by construction:
//
//   - Tempo arbitration is a priority comparison on a monotonic beat-origin
//     counter (Sessions.hpp:224 "We use beat origin magnitude to prioritize
//     sessions"; ClientSessionTimelines.hpp:96 forces a client change past
//     the incumbent). No timing assumption anywhere. Transplants intact.
//   - Time transfer (the GhostXForm measurement) volleys pings under a 50ms
//     per-exchange deadline and gives up after 5 misses (Measurement.hpp:116,
//     kNumberMeasurements=5). At 100ms+ RTT every exchange misses, fail() is
//     reached in ~250ms, and the session is never joined. It does not
//     degrade; it refuses.
//
// So the WAN group borrows exactly the half that survives: Link's
// conflict-resolution rule, run over WAIL's own time transfer (relay pongs,
// min-RTT offset filter — the ADR-0006 stack). StateSnapshot adoption is
// deliberately absent here: in this model the WAN timeline IS that path.
//
// What this file measures:
//
//   1. convergence — one declared change reaches every peer in ~one RTT
//   2. conflict — near-simultaneous declares converge to a single value with
//      no split-brain, each peer's tempo changing at most twice
//   3. echo safety — duplicate deliveries and late echoes are rejected by
//      priority alone; zero adoptions after convergence
//   4. the phase floor — what grid agreement the ADR-0006 stack achieves at
//      this RTT under jitter and path asymmetry, with tempo held still. If
//      the floor exceeded the 25ms perceptual threshold, tempo precision
//      would be moot — the transport would already have spent the budget.

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/align"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// --- Link's rule, transplanted ----------------------------------------------

// wanTimeline is the WAN session's shared state: a tempo and the priority
// counter arbitrating it, in micro-beats. A declaration stamps the declarer's
// estimate of the current WAN beat, forced past the incumbent — exactly
// ClientSessionTimelines.hpp:96 ("we must also make sure that it's >
// curSession.beatOrigin because otherwise it will get ignored").
type wanTimeline struct {
	tempo    float64
	originUb int64 // micro-beats
	owner    string
}

// adoptOver is Link's updateTimeline rule (Sessions.hpp:227, strictly greater
// wins) plus the tie-break Link itself only needs at session-merge time
// (Sessions.hpp:163, lowest id wins). Within a LAN session two clients never
// stamp identical beat origins — their live beat estimates differ by sync
// error — but two WAN declarations from peers with identical tempo histories
// tie exactly, and strict comparison alone would leave each side keeping its
// own: split-brain. The owner id breaks the tie deterministically; identical
// owners never adopt, so a duplicate delivery of the current timeline is
// rejected for free.
func (in wanTimeline) adoptOver(cur wanTimeline) bool {
	if in.originUb != cur.originUb {
		return in.originUb > cur.originUb
	}
	if cur.owner == "" {
		return in.owner != ""
	}
	return in.owner != "" && in.owner < cur.owner
}

// --- delayed message delivery ------------------------------------------------

// wanNet delivers scheduled closures in (time, insertion) order — the WAN's
// latency, with determinism (jitter draws happen at schedule time from a
// seeded source, so identical runs replay identically).
type wanNet struct {
	events []wanEvent
	seq    int
}

type wanEvent struct {
	atUs int64
	seq  int
	fn   func()
}

func (n *wanNet) at(atUs int64, fn func()) {
	n.seq++
	n.events = append(n.events, wanEvent{atUs: atUs, seq: n.seq, fn: fn})
}

func (n *wanNet) deliverThrough(nowUs int64) {
	for {
		best := -1
		for i, e := range n.events {
			if e.atUs > nowUs {
				continue
			}
			if best == -1 || e.atUs < n.events[best].atUs ||
				(e.atUs == n.events[best].atUs && e.seq < n.events[best].seq) {
				best = i
			}
		}
		if best == -1 {
			return
		}
		e := n.events[best]
		n.events = append(n.events[:best], n.events[best+1:]...)
		e.fn()
	}
}

// --- the relay ----------------------------------------------------------------

// wanRelay is dumb transport (pillar 5) plus the room clock: timelines are
// forwarded to every other peer regardless of arbitration; the relay's own
// adoption decision drives only its clock, which re-anchors on adoption and
// broadcasts a fresh anchor.
type wanRelay struct {
	net       *wanNet
	rng       *rand.Rand
	clk       simRoomClock
	tl        wanTimeline
	peers     []*wanPeer
	nowUs     int64
	reanchors int
	forwards  int
	dup       bool // adversary: deliver every timeline twice
}

func (r *wanRelay) onTimeline(from *wanPeer, tl wanTimeline, arriveUs int64) {
	copies := 1
	if r.dup {
		copies = 2
	}
	for _, q := range r.peers {
		if q == from {
			continue
		}
		for c := 0; c < copies; c++ {
			qq := q
			r.forwards++
			r.net.at(arriveUs+qq.downLeg(), func() { qq.onTimeline(tl) })
		}
	}
	if tl.adoptOver(r.tl) {
		r.tl = tl
		r.reanchors++
		r.clk.reanchor(arriveUs, tl.tempo)
		r.anchorAll(arriveUs)
	}
}

// anchorAll broadcasts the room anchor as interval_clock.go does after a
// clock event; values are captured at send time (they are server-clock
// anchored, so transit delay is exactly what OnServerPong's offset absorbs).
func (r *wanRelay) anchorAll(nowUs int64) {
	idx := r.clk.indexAt(nowUs)
	next := r.clk.boundaryUs(idx + 1)
	bpm := r.clk.tempo
	for _, q := range r.peers {
		qq := q
		r.net.at(nowUs+qq.downLeg(), func() {
			qq.steer.OnAnchor(next, idx, bpm, simBPI(), qq.now())
		})
	}
}

// --- one WAN peer ---------------------------------------------------------------

type wanPeer struct {
	name  string
	relay *wanRelay
	net   *wanNet
	link  *simLink
	steer *align.Steerer

	baseOffsetUs int64
	ppm          float64
	upUs, downUs int64 // one-way base latencies
	jitterUs     int64 // uniform [0, jitterUs] added per leg

	tl       wanTimeline
	wanBeat  float64 // advanced at the adopted tempo; the origin stamp source
	declares int
	adopts   []int64 // server-time stamps of remote adoptions
	rejects  int
	tempoLog []tempoAt

	deltas  []int64
	drifted int
}

type tempoAt struct {
	atUs int64
	bpm  float64
}

func (p *wanPeer) localUs(serverUs int64) int64 {
	return int64(float64(serverUs)*(1+p.ppm*1e-6)) + p.baseOffsetUs
}
func (p *wanPeer) trueOffsetUs(serverUs int64) int64 { return serverUs - p.localUs(serverUs) }
func (p *wanPeer) now() time.Time                    { return simWall(p.relay.nowUs) }

func (p *wanPeer) jitter() int64 {
	if p.jitterUs <= 0 {
		return 0
	}
	return p.relay.rng.Int63n(p.jitterUs + 1)
}
func (p *wanPeer) upLeg() int64   { return p.upUs + p.jitter() }
func (p *wanPeer) downLeg() int64 { return p.downUs + p.jitter() }

func (p *wanPeer) step(step int) {
	serverUs := p.relay.nowUs
	p.link.advanceTo(p.localUs(serverUs))
	p.wanBeat += p.tl.tempo / 60.0 * float64(simStepUs) / 1e6

	if step%simPingEvery == 0 {
		p.sendPing(serverUs)
	}
	if step%simAlignEvery == 0 {
		p.steer.Tick(simBPI(), p.now())
	}
	p.sample(serverUs)
}

// sendPing models one relay time exchange with per-leg jitter: the server
// stamps on arrival, so the estimate carries ((up+ju) − (down+jd))/2 of bias —
// the min-RTT filter's job is to pick the exchange where ju+jd was smallest.
func (p *wanPeer) sendPing(serverUs int64) {
	up, down := p.upLeg(), p.downLeg()
	stamp := serverUs + up
	deliverAt := serverUs + up + down
	sentLocal := p.localUs(serverUs)
	p.net.at(deliverAt, func() {
		recvLocal := p.localUs(deliverAt)
		rtt := recvLocal - sentLocal
		p.link.advanceTo(recvLocal)
		p.steer.OnServerPong(stamp+rtt/2, rtt, simBPI(), p.now())
	})
}

// declare is a tempo change made in WAIL's own UI: stamp the current WAN beat
// (forced past the incumbent), apply locally, broadcast. Steering nudges never
// come through here — the slew is not a declaration and does not touch the
// WAN timeline.
func (p *wanPeer) declare(bpm float64) {
	serverUs := p.relay.nowUs
	origin := int64(math.Round(p.wanBeat * 1e6))
	if origin <= p.tl.originUb {
		origin = p.tl.originUb + 1
	}
	p.tl = wanTimeline{tempo: bpm, originUb: origin, owner: p.name}
	p.declares++
	p.applyTempo("declared", bpm, serverUs)
	tl := p.tl
	pp := p
	p.net.at(serverUs+p.upLeg(), func() { pp.relay.onTimeline(pp, tl, pp.relay.nowUs) })
}

func (p *wanPeer) onTimeline(tl wanTimeline) {
	if tl.adoptOver(p.tl) {
		p.tl = tl
		p.adopts = append(p.adopts, p.relay.nowUs)
		p.applyTempo("remote", tl.tempo, p.relay.nowUs)
	} else {
		p.rejects++
	}
}

// applyTempo is the session loop's remote-TempoChange path (session.go:846-847).
func (p *wanPeer) applyTempo(source string, bpm float64, atUs int64) {
	p.steer.NoteTempoCommitted(bpm, p.now())
	p.link.source = source
	p.link.SetTempo(bpm)
	p.link.source = "steer"
	if n := len(p.tempoLog); n == 0 || p.tempoLog[n-1].bpm != bpm {
		p.tempoLog = append(p.tempoLog, tempoAt{atUs: atUs, bpm: bpm})
	}
}

// wanWarmup covers entry conformance at high RTT (3 pongs at 2s cadence) plus
// the post-snap settle, so floor statistics measure the steady state.
const wanWarmup = 60 * time.Second

func (p *wanPeer) sample(serverUs int64) {
	if serverUs < wanWarmup.Microseconds() {
		return
	}
	d := p.trueDelta(serverUs)
	p.deltas = append(p.deltas, d)
	if abs64i(d) > interval.AlignThresholdUs {
		p.drifted++
	}
}

// trueDelta: ground truth against the relay clock, as simPeer.trueDelta.
func (p *wanPeer) trueDelta(serverUs int64) int64 {
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

func (p *wanPeer) floorStats() (meanMs, p95Ms, maxMs, driftPct float64) {
	if len(p.deltas) == 0 {
		return 0, 0, 0, 0
	}
	abs := make([]float64, len(p.deltas))
	var sum float64
	for i, d := range p.deltas {
		abs[i] = math.Abs(float64(d)) / 1000
		sum += abs[i]
		if abs[i] > maxMs {
			maxMs = abs[i]
		}
	}
	meanMs = sum / float64(len(abs))
	sortFloats(abs)
	p95Ms = abs[int(0.95*float64(len(abs)-1))]
	driftPct = 100 * float64(p.drifted) / float64(len(p.deltas))
	return meanMs, p95Ms, maxMs, driftPct
}

func sortFloats(v []float64) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// --- group construction ---------------------------------------------------------

// wanGroup builds n peers behind one relay, all at the same base RTT, with
// per-peer asymmetry fractions (up = rtt/2·(1+a), down = rtt/2·(1−a)).
func wanGroup(n int, rttUs, jitterUs int64, asym []float64, seed int64) (*wanNet, *wanRelay) {
	net := &wanNet{}
	relay := &wanRelay{
		net: net, rng: rand.New(rand.NewSource(seed)),
		clk: simRoomClock{index: 0, atUs: 0, tempo: simRoomBPM, bpi: simBPI()},
		tl:  wanTimeline{tempo: simRoomBPM},
	}
	beat0s := []float64{3.7, 100.4, -8.25, 55.55, 12.0, 77.7}
	offsets := []int64{-2_000_000, 5_000_000, 1_500_000, -7_000_000, 3_300_000, -400_000}
	for i := 0; i < n; i++ {
		a := 0.0
		if i < len(asym) {
			a = asym[i]
		}
		p := &wanPeer{
			name: fmt.Sprintf("%c", 'A'+i), relay: relay, net: net,
			baseOffsetUs: offsets[i%len(offsets)],
			upUs:         int64(float64(rttUs) / 2 * (1 + a)),
			downUs:       int64(float64(rttUs) / 2 * (1 - a)),
			jitterUs:     jitterUs,
			tl:           wanTimeline{tempo: simRoomBPM},
		}
		d := newTunedTempoChangeDetector(simRoomBPM, tempoDetectorConfig{})
		p.link = newSimLink(simRoomBPM, beat0s[i%len(beat0s)], p.localUs(0), d, p.now)
		p.steer = align.NewSteerer(alignBridge{lb: p.link}, simRoomBPM, nil, nil, nil)
		relay.peers = append(relay.peers, p)
	}
	relay.anchorAll(0) // founder seeding, as runSim does
	return net, relay
}

func runWan(net *wanNet, relay *wanRelay, dur time.Duration, script func(step int)) {
	steps := int(dur.Microseconds() / simStepUs)
	for step := 0; step < steps; step++ {
		relay.nowUs = int64(step) * simStepUs
		net.deliverThrough(relay.nowUs)
		if script != nil {
			script(step)
		}
		for _, p := range relay.peers {
			p.step(step)
		}
	}
}

// convergence returns when the group last changed tempo and whether every peer
// ended on `want`; adoptions after that instant are the echo-safety violation.
func convergence(relay *wanRelay, want float64) (atUs int64, converged bool, postAdopts int) {
	converged = true
	for _, p := range relay.peers {
		n := len(p.tempoLog)
		if n == 0 || p.tempoLog[n-1].bpm != want {
			converged = false
		}
		if n > 0 && p.tempoLog[n-1].atUs > atUs {
			atUs = p.tempoLog[n-1].atUs
		}
	}
	for _, p := range relay.peers {
		for _, a := range p.adopts {
			if a > atUs {
				postAdopts++
			}
		}
	}
	return atUs, converged, postAdopts
}

// --- the measurements -------------------------------------------------------------

// TestWanLinkGroupConvergence: one declared change at each RTT, plain and with
// every message duplicated. Priority arbitration has no timing assumption, so
// the only latency cost should be the propagation itself — and duplicates must
// change nothing, because a copy of the current timeline never wins.
func TestWanLinkGroupConvergence(t *testing.T) {
	oneMin := stepsIn(time.Minute)
	t.Logf("%-8s %-6s %10s %8s %8s %8s %8s", "rtt", "dup", "converged", "timeMs", "adopts", "rejects", "fwd")
	for _, rttMs := range []int64{100, 200, 300} {
		for _, dup := range []bool{false, true} {
			net, relay := wanGroup(4, rttMs*1000, 20_000, nil, 42)
			relay.dup = dup
			declareAt := int64(-1)
			runWan(net, relay, 3*time.Minute, func(step int) {
				if step == oneMin {
					declareAt = relay.nowUs
					relay.peers[0].declare(124)
				}
			})
			convUs, ok, post := convergence(relay, 124)
			adopts, rejects := 0, 0
			for _, p := range relay.peers {
				adopts += len(p.adopts)
				rejects += p.rejects
			}
			ms := float64(convUs-declareAt) / 1000
			t.Logf("%-8d %-6v %10v %8.0f %8d %8d %8d", rttMs, dup, ok, ms, adopts, rejects, relay.forwards)
			if !ok {
				t.Errorf("rtt=%dms dup=%v: group did not converge on 124", rttMs, dup)
			}
			if convUs-declareAt > (rttMs+100)*1000+4*20_000 {
				t.Errorf("rtt=%dms dup=%v: convergence took %.0fms, want ≲ one RTT", rttMs, dup, ms)
			}
			if adopts != 3 {
				t.Errorf("rtt=%dms dup=%v: %d adoptions, want exactly 3 (one per other peer)", rttMs, dup, adopts)
			}
			if post != 0 {
				t.Errorf("rtt=%dms dup=%v: %d adoptions after convergence — echoes are being believed", rttMs, dup, post)
			}
		}
	}
}

// TestWanLinkGroupConflict: two peers declare different tempos within one
// relay window. The origin stamp arbitrates — later beat estimate wins, owner
// id breaks the exact tie (identical histories, same step) that Link never
// sees on a LAN but a WAN relay window produces routinely. No split-brain,
// and nobody's tempo changes more than twice (own declare, then the winner).
func TestWanLinkGroupConflict(t *testing.T) {
	oneMin := stepsIn(time.Minute)
	t.Logf("%-8s %-8s %8s %10s %8s", "rtt", "skew", "final", "converged", "maxFlips")
	for _, rttMs := range []int64{100, 300} {
		for _, skewSteps := range []int{0, 3, 8} { // 0ms, 60ms, 160ms
			net, relay := wanGroup(4, rttMs*1000, 20_000, nil, 42)
			runWan(net, relay, 3*time.Minute, func(step int) {
				if step == oneMin {
					relay.peers[0].declare(124)
				}
				if step == oneMin+skewSteps {
					relay.peers[1].declare(116)
				}
			})
			// Whichever value won, every peer must hold it.
			final := relay.peers[2].tempoLog[len(relay.peers[2].tempoLog)-1].bpm
			_, ok, post := convergence(relay, final)
			maxFlips := 0
			for _, p := range relay.peers {
				if len(p.tempoLog) > maxFlips {
					maxFlips = len(p.tempoLog)
				}
			}
			t.Logf("%-8d %-8d %8.0f %10v %8d", rttMs, skewSteps*20, final, ok, maxFlips)
			if !ok {
				t.Errorf("rtt=%dms skew=%dms: split-brain — peers ended on different tempos", rttMs, skewSteps*20)
			}
			if maxFlips > 2 {
				t.Errorf("rtt=%dms skew=%dms: a peer's tempo changed %d times, want ≤2", rttMs, skewSteps*20, maxFlips)
			}
			if post != 0 {
				t.Errorf("rtt=%dms skew=%dms: %d post-convergence adoptions", rttMs, skewSteps*20, post)
			}
		}
	}
}

// TestWanLinkGroupPhaseFloor: tempo held still; what grid agreement does the
// ADR-0006 stack reach at WAN latency? Per NTP's own physics the offset error
// floor is the path asymmetry: (up−down)/2 survives every filter, because no
// two-way exchange can see it. The symmetric case must sit well inside the
// 25ms perceptual threshold at every RTT — if it did not, tempo handling
// would be irrelevant. The asymmetric rows are the honest price of the WAN:
// measured against prediction, they say when the floor itself eats the budget.
func TestWanLinkGroupPhaseFloor(t *testing.T) {
	t.Logf("%-8s %-8s %10s %8s %8s %8s %8s", "rtt", "asym", "predMs", "meanMs", "p95Ms", "maxMs", "drift%")
	for _, rttMs := range []int64{100, 200, 300} {
		for _, asym := range []float64{0, 0.10, 0.20} {
			net, relay := wanGroup(2, rttMs*1000, 20_000, []float64{asym, 0}, 42)
			runWan(net, relay, 10*time.Minute, nil)
			mean, p95, max, drift := relay.peers[0].floorStats()
			pred := asym * float64(rttMs) / 2
			t.Logf("%-8d %-8.2f %10.1f %8.1f %8.1f %8.1f %8.1f", rttMs, asym, pred, mean, p95, max, drift)
			if asym == 0 {
				if mean > 25 || p95 > 25 {
					t.Errorf("rtt=%dms symmetric: floor mean=%.1fms p95=%.1fms — past the perceptual threshold with a clean path", rttMs, mean, p95)
				}
			}
		}
	}
}
