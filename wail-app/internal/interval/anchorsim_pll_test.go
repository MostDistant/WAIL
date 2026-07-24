package interval

// PLL-labeler simulation: the proposed redesign on top of the two-independent-
// Link harness (anchorsim_test.go). Steady state keeps today's baked offset —
// labels advance as localIdx + off, integer-exact and jitter-free — while a
// server-time evaluation of the relay clock acts as a correction signal that
// only re-locks the offset after N CONSECUTIVE disagreeing boundaries
// (hysteresis). Server-time estimates carry RTT/2-bounded error (110ms RTT →
// ±55ms), drawn per boundary.
//
// Verifies, vs the one-shot labeler:
//  (a) steady state: labels never flip (noise is filtered, never baked)
//  (b) a 64-beat grid jump (~4 intervals, the field symptom) heals in ≤3
//      boundaries instead of persisting forever
//  (c) no oscillation: at most one re-lock per event, including a worst-case
//      constant RTT-asymmetry bias swept across all grid phases

import (
	"math"
	"math/rand"
	"testing"
)

// pllLabeler is the proposed room labeler: baked offset + hysteresis re-lock.
type pllLabeler struct {
	off       int64
	aligned   bool
	disagree  int
	threshold int
	relocks   int
}

func newPLLLabeler(threshold int) *pllLabeler { return &pllLabeler{threshold: threshold} }

// label returns the room label for a local boundary, given the shared-clock
// evaluation of what the room index is at that boundary's server time.
func (p *pllLabeler) label(localIdx int64, clockEval int64) int64 {
	if !p.aligned {
		p.off, p.aligned = clockEval-localIdx, true
		return clockEval
	}
	candidate := localIdx + p.off
	if clockEval != candidate {
		p.disagree++
		if p.disagree >= p.threshold {
			p.off = clockEval - localIdx
			candidate = clockEval
			p.disagree = 0
			p.relocks++
		}
	} else {
		p.disagree = 0
	}
	return candidate
}

// simPeer couples a mock Link grid with a PLL labeler and an error-bearing
// server-time estimator (error = constant bias + uniform jitter, both in µs).
type simPeer struct {
	link      mockLink
	cfg       Config
	pll       *pllLabeler
	estBiasUs int64
	estJittUs int64
	rng       *rand.Rand
}

func (s *simPeer) evalClock(tUs int64, rc *relayClock) int64 {
	eps := s.estBiasUs
	if s.estJittUs > 0 {
		eps += s.rng.Int63n(2*s.estJittUs) - s.estJittUs
	}
	return rc.indexAt(tUs + eps)
}

// boundaryEvent is one detected local-boundary crossing on a peer.
type boundaryEvent struct {
	tUs      int64
	localIdx int64
}

// walkBoundaries steps the peer's grid forward in 50ms increments over
// [t0Us, t0Us+spanUs), emitting one event per newly entered local interval
// (a grid jump registers as a single event at the jumped-to index, matching
// the engine's one-onBoundary-per-tick rule). jumpAtUs/jumpBeats optionally
// dislocate the grid mid-walk (Link session merge / transport reset).
func (s *simPeer) walkBoundaries(t0Us, spanUs, jumpAtUs int64, jumpBeats, tempo float64) []boundaryEvent {
	const stepUs = 50_000
	var out []boundaryEvent
	prevIdx := int64(math.MinInt64)
	for t := t0Us; t < t0Us+int64(spanUs); t += stepUs {
		if jumpAtUs > 0 && t >= jumpAtUs {
			// apply once
			jumpAtUs = -1
			s.link.origin += jumpBeats
			_ = t
		}
		idx := s.cfg.IndexAtBeat(s.link.beatAt(t, tempo))
		if idx != prevIdx && prevIdx != math.MinInt64 {
			out = append(out, boundaryEvent{tUs: t, localIdx: idx})
		}
		prevIdx = idx
	}
	return out
}

const (
	simTempo   = 120.0
	simBPI     = 16.0                                  // bars 4 × quantum 4 — "quantum 16" from the field
	simIntvlUs = int64(simBPI * 60.0 / simTempo * 1e6) // 8s
)

func TestPLLLabelerSteadyStateNeverFlips(t *testing.T) {
	rc := newRelayClock(0, simTempo, simBPI)
	cfg := Config{Bars: 4, Quantum: 4}
	// Worst-case jitter on every evaluation: ±55ms (110ms RTT, fully
	// asymmetric draw each boundary).
	p := &simPeer{
		link: mockLink{origin: 7.31}, cfg: cfg,
		pll: newPLLLabeler(3), estJittUs: 55_000,
		rng: rand.New(rand.NewSource(42)),
	}
	events := p.walkBoundaries(1_000_000, 200*simIntvlUs, 0, 0, simTempo)
	flips := 0
	for i, ev := range events {
		label := p.pll.label(ev.localIdx, p.evalClock(ev.tUs, rc))
		if want := rc.indexAt(ev.tUs); label != want {
			// Allow the initial-alignment heal (first `threshold` boundaries);
			// after that labels must track the true room index exactly.
			if i >= p.pll.threshold {
				flips++
			}
		}
	}
	t.Logf("200 boundaries, ±55ms noise per eval: %d flips after initial heal, %d re-locks", flips, p.pll.relocks)
	if flips > 0 {
		t.Errorf("steady-state labels flipped %d times", flips)
	}
	if p.pll.relocks > 1 {
		t.Errorf("re-locked %d times in steady state (want ≤1: the initial alignment heal)", p.pll.relocks)
	}
}

func TestPLLLabelerGridJumpHeals(t *testing.T) {
	for _, jumpBeats := range []float64{64, -80, 20} { // +4, −5, +1.25 intervals
		rc := newRelayClock(0, simTempo, simBPI)
		cfg := Config{Bars: 4, Quantum: 4}
		p := &simPeer{
			link: mockLink{origin: 3.7}, cfg: cfg,
			pll: newPLLLabeler(3), estJittUs: 55_000,
			rng: rand.New(rand.NewSource(7)),
		}
		span := 120 * simIntvlUs
		jumpAt := int64(60 * simIntvlUs)
		events := p.walkBoundaries(1_000_000, span, jumpAt, jumpBeats, simTempo)

		healedAt := -1
		for i, ev := range events {
			label := p.pll.label(ev.localIdx, p.evalClock(ev.tUs, rc))
			if ev.tUs >= jumpAt && healedAt < 0 && label == rc.indexAt(ev.tUs) {
				// first boundary AT/after the jump whose label is correct
				// (re-lock uses the evaluated label immediately)
				boundariesSinceJump := 0
				for _, prev := range events[:i+1] {
					if prev.tUs >= jumpAt {
						boundariesSinceJump++
					}
				}
				healedAt = boundariesSinceJump
			}
		}
		t.Logf("jump %+.0f beats: healed at boundary %d after the jump, %d re-locks total",
			jumpBeats, healedAt, p.pll.relocks)
		if healedAt < 0 {
			t.Errorf("jump %+.0f beats: never healed", jumpBeats)
		} else if healedAt > 3 {
			t.Errorf("jump %+.0f beats: healed at boundary %d (want ≤3)", jumpBeats, healedAt)
		}
	}
}

func TestPLLLabelerNoOscillationUnderConstantBias(t *testing.T) {
	// Worst-case systematic RTT asymmetry: every evaluation biased +55ms.
	// Sweep the grid phase finely against the relay tick — the pathological
	// case is a boundary phase that lands inside the bias window every time.
	for phase := 0.0; phase < simBPI; phase += 0.5 {
		rc := newRelayClock(0, simTempo, simBPI)
		cfg := Config{Bars: 4, Quantum: 4}
		p := &simPeer{
			link: mockLink{origin: phase}, cfg: cfg,
			pll: newPLLLabeler(3), estBiasUs: 55_000,
			rng: rand.New(rand.NewSource(1)),
		}
		events := p.walkBoundaries(1_000_000, 100*simIntvlUs, 0, 0, simTempo)
		for _, ev := range events {
			p.pll.label(ev.localIdx, p.evalClock(ev.tUs, rc))
		}
		if p.pll.relocks > 1 {
			t.Errorf("phase %.1f beats: %d re-locks (want ≤1 — re-lock must be sticky)", phase, p.pll.relocks)
		}
	}
}

func TestPLLLabelerPairDelayAfterJump(t *testing.T) {
	// The field scenario: sender's grid jumps +64 beats mid-jam. With the
	// one-shot labeler the receiver is stuck +4 intervals forever; the PLL
	// labeler should restore D≈1 within 3 intervals.
	rc := newRelayClock(0, simTempo, simBPI)
	cfg := Config{Bars: 4, Quantum: 4}
	sender := &simPeer{link: mockLink{origin: 100.4}, cfg: cfg, pll: newPLLLabeler(3), estJittUs: 55_000, rng: rand.New(rand.NewSource(11))}
	receiver := &simPeer{link: mockLink{origin: -8.25}, cfg: cfg, pll: newPLLLabeler(3), estJittUs: 55_000, rng: rand.New(rand.NewSource(13))}
	const D = 1

	// Establish alignment.
	for _, ev := range sender.walkBoundaries(1_000_000, 5*simIntvlUs, 0, 0, simTempo) {
		sender.pll.label(ev.localIdx, sender.evalClock(ev.tUs, rc))
	}
	for _, ev := range receiver.walkBoundaries(1_000_000, 5*simIntvlUs, 0, 0, simTempo) {
		receiver.pll.label(ev.localIdx, receiver.evalClock(ev.tUs, rc))
	}

	delayAt := func(tCapUs int64) float64 {
		capIdx := cfg.IndexAtBeat(sender.link.beatAt(tCapUs, simTempo))
		capLabel := capIdx + sender.pll.off
		li := capLabel + D - receiver.pll.off
		tPlay := int64(math.Round((float64(li)*simBPI - receiver.link.origin) * 60.0 / simTempo * 1e6))
		return float64(tPlay-tCapUs) / float64(simIntvlUs)
	}

	base := 30 * simIntvlUs
	before := delayAt(base)
	// Sender grid jumps +64 beats (~4 intervals).
	sender.link.origin += 64
	during := delayAt(base + 2*simIntvlUs)
	// Walk sender boundaries so the PLL observes the disagreement and re-locks.
	events := sender.walkBoundaries(base+2*simIntvlUs, 10*simIntvlUs, 0, 0, simTempo)
	for _, ev := range events {
		sender.pll.label(ev.localIdx, sender.evalClock(ev.tUs, rc))
	}
	after := delayAt(base + 8*simIntvlUs)

	t.Logf("delay before jump: %.2f — right after: %.2f — 6 intervals later (PLL healed): %.2f", before, during, after)
	if math.Abs(during-before) < 3 {
		t.Errorf("expected the +64-beat jump to shift delay ~4 intervals, got %.2f → %.2f", before, during)
	}
	if math.Abs(after-before) > 1.5 {
		t.Errorf("delay did not return to baseline after heal: before %.2f, after %.2f", before, after)
	}
}
