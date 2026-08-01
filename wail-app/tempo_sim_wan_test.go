package main

// The WAN tempo-declaration group, simulated over a delayed relay (ADR-0009).
//
// Tempo is the one musical value that must cross the internet, and it travels
// as declarations arbitrated by Link's own timeline-priority rule — the half
// of Link that survives WAN latency. (The other half, its time transfer,
// volleys pings under a 50ms per-exchange deadline and gives up after five
// misses — Measurement.hpp:116 — so at 100ms+ RTT Link itself never joins.
// Priority comparison has no timing assumption at all: Sessions.hpp:224.)
//
// This file drives the PRODUCTION arbitration (tempoDeclareAdopts, the exact
// rule session.go runs on TempoDeclare) through a latency-and-jitter relay
// model and measures the properties the design leans on:
//
//   1. convergence — one declared change reaches every peer in ~one RTT
//   2. conflict — near-simultaneous declares converge to a single value with
//      no split-brain, each peer's tempo changing at most twice
//   3. echo safety — duplicated delivery and late echoes are rejected by the
//      rule alone; zero adoptions after convergence. No echo guard exists on
//      this path, so this property carries the whole load.
//
// The earlier, larger simulations that measured the retired steering stack
// (grid slew, thresholds, phase floors) served their purpose as ADR-0009's
// evidence base and live in git history at claude/tempo-sim-harness.

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// --- delayed message delivery -------------------------------------------------

// wanNet delivers scheduled closures in (time, insertion) order — the WAN's
// latency, deterministic (jitter draws happen at schedule time from a seeded
// source, so identical runs replay identically).
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

// --- peers and relay ------------------------------------------------------------

type wanDeclaration struct {
	bpm    float64
	origin int64
	owner  string
}

// wanPeer is one WAIL's tempo state: the adopted value plus the (origin,
// owner) stamp arbitrating it — exactly the session loop's tempoOrigin/
// tempoOwner/currentBPM trio.
type wanPeer struct {
	name         string
	relay        *wanRelay
	upUs, downUs int64
	jitterUs     int64

	bpm      float64
	origin   int64
	owner    string
	declares int
	adopts   []int64 // relay-time stamps of remote adoptions
	rejects  int
	tempoLog []tempoAt
}

type tempoAt struct {
	atUs int64
	bpm  float64
}

type wanRelay struct {
	net      *wanNet
	rng      *rand.Rand
	peers    []*wanPeer
	nowUs    int64
	forwards int
	dup      bool // adversary: deliver every declaration twice
}

func (p *wanPeer) jitter() int64 {
	if p.jitterUs <= 0 {
		return 0
	}
	return p.relay.rng.Int63n(p.jitterUs + 1)
}

// declare is session.go's declareTempo: stamp past the incumbent, apply
// locally, broadcast.
func (p *wanPeer) declare(bpm float64) {
	origin := p.relay.nowUs // the sim's shared clock stands in for wall time
	if origin <= p.origin {
		origin = p.origin + 1
	}
	p.origin, p.owner = origin, p.name
	p.declares++
	p.apply(bpm)
	d := wanDeclaration{bpm: bpm, origin: origin, owner: p.name}
	pp := p
	p.net().at(p.relay.nowUs+p.upUs+p.jitter(), func() { pp.relay.onDeclare(pp, d) })
}

func (p *wanPeer) net() *wanNet { return p.relay.net }

// onDeclare is session.go's TempoDeclare handler, verbatim: the production
// arbitration decides, duplicates are inert by the rule.
func (p *wanPeer) onDeclare(d wanDeclaration) {
	if tempoDeclareAdopts(d.origin, d.owner, p.origin, p.owner) {
		p.origin, p.owner = d.origin, d.owner
		p.adopts = append(p.adopts, p.relay.nowUs)
		p.apply(d.bpm)
	} else {
		p.rejects++
	}
}

func (p *wanPeer) apply(bpm float64) {
	p.bpm = bpm
	if n := len(p.tempoLog); n == 0 || p.tempoLog[n-1].bpm != bpm {
		p.tempoLog = append(p.tempoLog, tempoAt{atUs: p.relay.nowUs, bpm: bpm})
	}
}

// onDeclare (relay side): dumb transport — forward to every other peer.
func (r *wanRelay) onDeclare(from *wanPeer, d wanDeclaration) {
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
			r.net.at(r.nowUs+qq.downUs+qq.jitter(), func() { qq.onDeclare(d) })
		}
	}
}

func wanGroup(n int, rttUs, jitterUs int64, seed int64) (*wanNet, *wanRelay) {
	net := &wanNet{}
	relay := &wanRelay{net: net, rng: rand.New(rand.NewSource(seed))}
	for i := 0; i < n; i++ {
		relay.peers = append(relay.peers, &wanPeer{
			name: fmt.Sprintf("%c", 'A'+i), relay: relay,
			upUs: rttUs / 2, downUs: rttUs / 2, jitterUs: jitterUs,
			bpm: 120,
		})
	}
	return net, relay
}

const wanStepUs = 20_000 // one Link poll

func runWan(net *wanNet, relay *wanRelay, dur time.Duration, script func(step int)) {
	steps := int(dur.Microseconds() / wanStepUs)
	for step := 0; step < steps; step++ {
		relay.nowUs = int64(step) * wanStepUs
		net.deliverThrough(relay.nowUs)
		if script != nil {
			script(step)
		}
	}
}

// convergence returns when the group last changed tempo, whether every peer
// ended on want, and adoptions after that instant (the echo-safety violation).
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

// One declared change at each RTT, plain and with every message duplicated.
// The rule has no timing assumption, so the only cost should be propagation
// itself — and duplicates must change nothing, because a copy of the current
// declaration never wins.
func TestWanDeclareConvergence(t *testing.T) {
	t.Logf("%-8s %-6s %10s %8s %8s %8s", "rtt", "dup", "converged", "timeMs", "adopts", "rejects")
	for _, rttMs := range []int64{100, 200, 300} {
		for _, dup := range []bool{false, true} {
			net, relay := wanGroup(4, rttMs*1000, 20_000, 42)
			relay.dup = dup
			declareAt := int64(-1)
			runWan(net, relay, time.Minute, func(step int) {
				if step == 500 { // t = 10s
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
			t.Logf("%-8d %-6v %10v %8.0f %8d %8d", rttMs, dup, ok, ms, adopts, rejects)
			if !ok {
				t.Errorf("rtt=%dms dup=%v: group did not converge on 124", rttMs, dup)
			}
			if convUs-declareAt > (rttMs+100)*1000 {
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

// Two peers declare different tempos within one relay window. The origin
// stamp arbitrates; the owner id breaks the exact tie that identical stamps
// produce (both declares in the same step share the sim clock — the analogue
// of two wall clocks agreeing to the microsecond, which the tie-break makes
// deterministic instead of split-brained). Nobody's tempo changes more than
// twice: their own declare, then the winner.
func TestWanDeclareConflict(t *testing.T) {
	t.Logf("%-8s %-8s %8s %10s %8s", "rtt", "skewMs", "final", "converged", "maxFlips")
	for _, rttMs := range []int64{100, 300} {
		for _, skewSteps := range []int{0, 3, 8} {
			net, relay := wanGroup(4, rttMs*1000, 20_000, 42)
			runWan(net, relay, time.Minute, func(step int) {
				if step == 500 {
					relay.peers[0].declare(124)
				}
				if step == 500+skewSteps {
					relay.peers[1].declare(116)
				}
			})
			final := relay.peers[2].tempoLog[len(relay.peers[2].tempoLog)-1].bpm
			_, ok, post := convergence(relay, final)
			maxFlips := 0
			for _, p := range relay.peers {
				if n := len(p.tempoLog); n > maxFlips {
					maxFlips = n
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
