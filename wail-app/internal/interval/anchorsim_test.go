package interval

// Two-independent-Link-session simulation of the room-anchor alignment chain
// (field hunt 2026-07-23: stable 4–5 interval receive delay with all timing
// fixes deployed, expected D=1).
//
// Per the Link SDK docs, beatAtTime's *magnitude* is unique to each Link
// instance and only its phase mod the asked quantum is session-shared — so two
// peers on different LANs run fully independent grids: beat(t) = tempo·t + c
// with unrelated constants. Everything in the room-index chain must bridge
// exactly that. This harness models the real components faithfully:
//
//   relay:   a copy of signaling-server/roomclock.go (post-#397, with the
//            transition pin) minting anchors on its own clock
//   clients: RoomLabeler + Config (the production types) aligning as
//            SetRoomAnchor does: sample local index at receipt, fix an offset
//   sender:  labels capture windows floor(beatS(t)/BPI) + offS
//   receiver: releases label−D at its own boundaries (playout.Scheduler rule)
//
// and measures the resulting capture→playout delay in intervals, sweeping grid
// origins, anchor transit times, join times, tempo changes, and beat jumps.

import (
	"math"
	"testing"
)

// --- mock Link session (one per peer; independent magnitude + phase) ---

type mockLink struct{ origin float64 } // beat(t=0), beats

func (l mockLink) beatAt(tUs int64, tempo float64) float64 {
	return tempo*float64(tUs)/60e6 + l.origin
}

// --- relay room clock: faithful copy of signaling-server/roomclock.go ---

type relayClock struct {
	index    int64
	atUs     int64
	tempo    float64
	bpi      float64
	pinIdx   int64
	pinUntil int64 // 0 = no pin
}

func newRelayClock(nowUs int64, tempo, bpi float64) *relayClock {
	return &relayClock{index: 0, atUs: nowUs, tempo: tempo, bpi: bpi}
}

func (rc *relayClock) boundaryUs(idx int64) int64 {
	secFromAnchor := float64(idx-rc.index) * rc.bpi * 60.0 / rc.tempo
	return rc.atUs + int64(math.Round(secFromAnchor*1e6))
}

func (rc *relayClock) indexAt(nowUs int64) int64 {
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

// reanchor mirrors the server: quantize to the next boundary, pin in between.
func (rc *relayClock) reanchor(nowUs int64, tempo, bpi float64) {
	next := rc.indexAt(nowUs) + 1
	nextBoundary := rc.boundaryUs(next)
	rc.pinIdx, rc.pinUntil = next-1, nextBoundary
	rc.index, rc.atUs, rc.tempo, rc.bpi = next, nextBoundary, tempo, bpi
}

// --- mock client ---

type mockClient struct {
	link  mockLink
	cfg   Config
	label RoomLabeler
}

// applyAnchor mirrors SetRoomAnchor: adopt the anchor's config, sample the
// local index at receipt, fix the offset.
func (c *mockClient) applyAnchor(roomIndex int64, tReceiptUs int64, tempo float64, bars uint32, quantum float64) {
	c.cfg = Config{Bars: bars, Quantum: quantum}
	localBeat := c.link.beatAt(tReceiptUs, tempo)
	localIdx := c.cfg.IndexAtBeat(localBeat)
	c.label.Align(roomIndex, localIdx)
}

func (c *mockClient) roomLabel(tUs int64, tempo float64) (int64, bool) {
	localBeat := c.link.beatAt(tUs, tempo)
	return c.label.RoomIndex(c.cfg.IndexAtBeat(localBeat))
}

// measureDelay computes the capture→playout delay, in intervals, for audio the
// sender captures at tCapUs: the receiver releases label L when its own room
// label reaches L+D, at the start of its corresponding local interval.
func measureDelay(t *testing.T, sender, receiver *mockClient, tCapUs int64, tempo float64, offsetD int64) float64 {
	bpi := sender.cfg.BeatsPerInterval()
	intervalUs := bpi * 60.0 / tempo * 1e6

	capLocalIdx := sender.cfg.IndexAtBeat(sender.link.beatAt(tCapUs, tempo))
	capLabel, ok := sender.label.RoomIndex(capLocalIdx)
	if !ok {
		t.Fatalf("sender unaligned")
	}
	// Receiver boundary whose label is capLabel+D: the receiver's local
	// interval li satisfies li + offR = capLabel + D, starting at beat li*BPI.
	offR := receiver.label.Offset()
	li := capLabel + offsetD - offR
	tPlayUs := int64(math.Round((float64(li)*bpi - receiver.link.origin) * 60.0 / tempo * 1e6))
	return float64(tPlayUs-tCapUs) / intervalUs
}

// --- scenarios ---

type simParams struct {
	name          string
	originA       float64 // founder grid origin, beats
	originB       float64 // joiner grid origin, beats
	joinUs        int64
	transitAUs    int64 // founder anchor transit
	transitBUs    int64 // joiner anchor transit
	tempoChangeAt int64 // 0 = none; founder nudges tempo +1 here
	beatJumpAt    int64 // 0 = none; joiner's grid jumps beatJump beats here
	beatJump      float64
}

func runSim(t *testing.T, p simParams) (minDelay, maxDelay float64) {
	const (
		bars, quantum = 4, 4.0 // BPI 16, "quantum 16" from the field report
		tempo         = 120.0
		offsetD       = 1
	)
	bpi := float64(bars) * quantum
	intervalUs := int64(bpi * 60.0 / tempo * 1e6) // 8s

	relay := newRelayClock(0, tempo, bpi)
	A := &mockClient{link: mockLink{origin: p.originA}}
	B := &mockClient{link: mockLink{origin: p.originB}}

	// Founder: broadcasts TempoChange at t=0; relay creates the clock and
	// broadcasts anchor indexAt(0)=0; A applies it after transit.
	A.applyAnchor(relay.indexAt(0), p.transitAUs, tempo, bars, quantum)
	// Joiner: private currentAnchor minted at join, applied after transit.
	B.applyAnchor(relay.indexAt(p.joinUs), p.joinUs+p.transitBUs, tempo, bars, quantum)

	// Optional mid-jam tempo change: relay re-anchors and broadcasts; both
	// peers re-apply after their transits (at the new tempo).
	if p.tempoChangeAt > 0 {
		relay.reanchor(p.tempoChangeAt, tempo+1, bpi)
		minted := relay.indexAt(p.tempoChangeAt)
		A.applyAnchor(minted, p.tempoChangeAt+p.transitAUs, tempo+1, bars, quantum)
		B.applyAnchor(minted, p.tempoChangeAt+p.transitBUs, tempo+1, bars, quantum)
	}
	// Optional joiner beat jump (transport reset): grid origin shifts, no
	// anchor arrives — the labeler offset is stale until the next one.
	if p.beatJumpAt > 0 {
		B.link.origin += p.beatJump
	}

	// Measure B→A delay at every joiner boundary across 8 intervals of span.
	start := p.joinUs + p.transitBUs
	if p.tempoChangeAt > start {
		start = p.tempoChangeAt + p.transitBUs
	}
	if p.beatJumpAt > start {
		start = p.beatJumpAt
	}
	minDelay, maxDelay = math.Inf(1), math.Inf(-1)
	for i := 0; i < 8; i++ {
		tCap := start + int64(i)*intervalUs + intervalUs/3 // mid-interval captures
		d := measureDelay(t, B, A, tCap, tempo, offsetD)
		if d < minDelay {
			minDelay = d
		}
		if d > maxDelay {
			maxDelay = d
		}
	}
	return minDelay, maxDelay
}

func TestAnchorSimBaselineDelay(t *testing.T) {
	origins := []float64{0, 3.7, -8.25, 100.4, 1e6 + 0.3}
	transits := []int64{0, 200_000, 1_000_000, 3_000_000, 7_900_000} // 0–7.9s
	worstMin, worstMax := math.Inf(1), math.Inf(-1)
	var worstCase string
	for _, oA := range origins {
		for _, oB := range origins {
			for _, dA := range transits {
				for _, dB := range transits {
					p := simParams{
						name: "baseline", originA: oA, originB: oB,
						joinUs: 137_000_000, transitAUs: dA, transitBUs: dB,
					}
					mn, mx := runSim(t, p)
					if mn < worstMin {
						worstMin = mn
					}
					if mx > worstMax {
						worstMax, worstCase = mx, p.name
					}
				}
			}
		}
	}
	t.Logf("baseline join: delay range over sweep = [%.3f, %.3f] intervals (D=1)", worstMin, worstMax)
	// The capture instant is mid-interval while playout is measured from the
	// release interval's start: a −0.5 bias is built in. On top of D=1 each
	// alignment can err ±1 interval (transit straddling a tick), so the honest
	// budget is D−0.5±2 = [−1.5, 3.5].
	if worstMin < -1.5 || worstMax > 3.5 {
		t.Errorf("baseline delay outside hazard budget [-1.5,3.5]: [%.3f, %.3f] (worst %s)", worstMin, worstMax, worstCase)
	}
}

func TestAnchorSimTempoChangeMidJam(t *testing.T) {
	mn, mx := runSim(t, simParams{
		name: "tempo-change", originA: 3.7, originB: 100.4,
		joinUs: 137_000_000, transitAUs: 300_000, transitBUs: 300_000,
		tempoChangeAt: 200_000_000,
	})
	t.Logf("tempo change mid-jam: delay range = [%.3f, %.3f]", mn, mx)
	if mn < -1.5 || mx > 3.5 {
		t.Errorf("delay outside hazard budget [-1.5,3.5]: [%.3f, %.3f]", mn, mx)
	}
}

func TestAnchorSimBeatJumpBetweenAnchors(t *testing.T) {
	// A whole-number-of-beats jump that is NOT a whole number of intervals
	// (e.g. transport relocate by 20 beats = 1.25 intervals at BPI 16).
	mn, mx := runSim(t, simParams{
		name: "beat-jump", originA: 3.7, originB: 100.4,
		joinUs: 137_000_000, transitAUs: 300_000, transitBUs: 300_000,
		beatJumpAt: 250_000_000, beatJump: 20,
	})
	t.Logf("joiner beat jump +20 beats, no anchor after: delay range = [%.3f, %.3f]", mn, mx)
}
