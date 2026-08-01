package main

// The jitter shapes the tempo simulation replays (ADR-0008). Every one is a
// disturbance this repo has actually observed in the field or in a bug report;
// the citation on each is where the evidence lives. They move the session tempo
// or the local grid the way a DAW, a LAN Link peer or a WAN path would — never
// through WAIL's own write path, so WAIL has to discover them by observation
// exactly as it does in production.

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// --- injectors -------------------------------------------------------------

// afterStep runs f from step `at` onward, and nothing before.
func afterStep(at int, f func(p *simPeer, step int)) func(*simPeer, int) {
	return func(p *simPeer, step int) {
		if step >= at {
			f(p, step)
		}
	}
}

func stepsIn(d time.Duration) int { return int(d.Microseconds() / simStepUs) }

// convergenceNudge: a Link session merge drags the tempo ±2% for a handful of
// polls, then it settles back (link_types.go:36 — the reason the hold-down
// exists at all). Fires once a minute so several land in a run.
func convergenceNudge(magnitude float64) func(*simPeer, int) {
	const every, width = 3000, 5 // 60s apart, 100ms wide
	var restore float64
	return func(p *simPeer, step int) {
		switch phase := step % every; {
		case phase == 0:
			restore = p.link.bpm
			p.link.disturb(restore * (1 + magnitude))
		case phase == width:
			p.link.disturb(restore)
		}
	}
}

// insistentLAN: a non-WAIL Link peer on this LAN holds its own tempo and drags
// the session back toward it every poll (tradeoffs.md:249 — the 120↔122 flap
// between two insistent LANs). Rate is Link's convergence, not a step.
func insistentLAN(want, rate float64) func(*simPeer, int) {
	return func(p *simPeer, _ int) {
		p.link.disturb(p.link.bpm + (want-p.link.bpm)*rate)
	}
}

// wobble: the reported field bug — a clock wandering between two values around
// one intended tempo, dwelling `dwell` at each (ADR-0008, 2026-07-31).
func wobble(low, high float64, dwell time.Duration) func(*simPeer, int) {
	half := stepsIn(dwell)
	return func(p *simPeer, step int) {
		if half <= 0 {
			return
		}
		if (step/half)%2 == 0 {
			p.link.disturb(high)
		} else {
			p.link.disturb(low)
		}
	}
}

// lanLinkDevice models the third participant a real LAN usually has: another
// Ableton Link device alongside the DAW and WAIL — an Ableton Move, a Missing
// Link bridging an analogue clock, a Torso T-1. It measures its own clock and
// re-asserts the result as the session tempo, so Link arbitrates continuously
// and the tempo the DAW and WAIL see is dragged toward the device's number every
// poll. Nobody is touching a tempo control.
//
// This is what the field "wobble" almost certainly was. A square wave between
// two setpoints — which is how this harness first modelled it — is a user
// toggling the tempo, and behaves differently: the tug never stops, so WAIL's
// nudges are overwritten before they can accumulate. The amplitude fits too;
// 0.1 BPM is 833ppm, far too large for a crystal (50ppm) but unremarkable for a
// measured clock being republished.
func lanLinkDevice(nominal, amplitude, convergeRate float64, wanderSteps int) func(*simPeer, int) {
	return func(p *simPeer, step int) {
		// Two incommensurate components, so the wander does not land on a tidy
		// repeating cycle the way a square wave does. Deterministic: no RNG.
		t := float64(step)
		w := math.Sin(2*math.Pi*t/float64(wanderSteps)) +
			0.5*math.Sin(2*math.Pi*t/(float64(wanderSteps)*0.37))
		want := nominal + amplitude*w/1.5
		p.link.disturb(p.link.bpm + (want-p.link.bpm)*convergeRate)
	}
}

// knobTurn: one deliberate change, held forever after — a human's hand, not a
// disturbance. Nothing re-asserts it, so anything that moves it away wins.
func knobTurn(at int, to float64) func(*simPeer, int) {
	fired := false
	return func(p *simPeer, step int) {
		if step == at && !fired {
			fired = true
			p.link.disturb(to)
		}
	}
}

// insistentDAW: automation or an external clock re-asserting its tempo after
// every correction. Enforcement must yield to this rather than fight forever
// (the #424 two-enforcer lesson, ADR-0008 Consequences).
func insistentDAW(at int, want float64) func(*simPeer, int) {
	return afterStep(at, func(p *simPeer, _ int) { p.link.disturb(want) })
}

// gridShove: the local timeline moves without a tempo change — a post-re-anchor
// aftershock (ADR-0006's 2026-07-25 amendment) or a transport relocate.
func gridShove(at int, ms float64) func(*simPeer, int) {
	return func(p *simPeer, step int) {
		if step == at {
			p.link.jumpBeats(ms / 1000 * p.link.bpm / 60)
		}
	}
}

// vpnTeleport: a jittery WAN path where an occasional low-RTT sample carries a
// badly skewed one-way split, so min-RTT re-selection teleports the offset
// estimate (grid.go:94 — the 2026-07-26 Australia session, ±70ms).
func vpnTeleport(period int, biasUs int64) func(*simPeer, int) {
	return func(p *simPeer, step int) {
		if step%period == 0 {
			// A "new best" RTT whose split is wrong by biasUs either way.
			p.upUs, p.downUs = 40_000+biasUs, 40_000-biasUs
		} else if step%period == simPingEvery {
			p.upUs, p.downUs = 60_000, 60_000 // honest, higher-RTT samples
		}
	}
}

// --- the scenario table ----------------------------------------------------

const simRun = 10 * time.Minute

func shapeScenarios() []simConfig {
	oneMin := stepsIn(time.Minute)
	return []simConfig{
		{
			name: "0-quiet-room",
			peers: []simPeerSpec{
				{name: "A", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			name: "1-link-convergence",
			peers: []simPeerSpec{
				{name: "A", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: convergenceNudge(0.02)},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			name: "2-adoption-oscillator",
			peers: []simPeerSpec{
				{name: "A@120", beat0: 3.7, bpm: 120, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000},
				{name: "B@110", beat0: 100.4, bpm: 110, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			name: "3-two-insistent-LANs",
			peers: []simPeerSpec{
				{name: "A-LAN120", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: afterStep(oneMin, insistentLAN(120, 0.02))},
				{name: "B-LAN122", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000,
					disturb: afterStep(oneMin, insistentLAN(122, 0.02))},
			},
		},
		{
			name: "4-vpn-teleport",
			peers: []simPeerSpec{
				{name: "A", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 60_000, downUs: 60_000,
					disturb: vpnTeleport(500, 70_000)},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			name: "5-crystal-drift-50ppm",
			peers: []simPeerSpec{
				{name: "A+50ppm", beat0: 3.7, ppm: 50, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000},
				{name: "B-50ppm", beat0: 100.4, ppm: -50, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			name: "6-aftershock-47ms",
			peers: []simPeerSpec{
				{name: "A", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: gridShove(2*oneMin, 47)},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			// (up−down)/2 = −12ms, so the offset estimate is biased by −12ms and
			// the peer sees a standing δ that is not physically there.
			name: "7-rtt-bias-12ms",
			peers: []simPeerSpec{
				{name: "A-biased", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 13_000, downUs: 37_000},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			name: "8-wobble-119.9-120",
			peers: []simPeerSpec{
				{name: "A-wobble", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: afterStep(oneMin, wobble(119.9, 120.0, 2*time.Second))},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			name: "9-insistent-DAW-121.5",
			peers: []simPeerSpec{
				{name: "A-DAW", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: insistentDAW(oneMin, 121.5)},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
	}
}

// TestTempoSimCharacterise runs every shape and prints the table the ADR-0008
// thresholds are set from. It asserts nothing: the point is to measure what
// today's code does before changing it. The two assertions that DO gate this
// work live in TestTempoSimFidelityGate below.
func TestTempoSimCharacterise(t *testing.T) {
	t.Logf("%-24s %-10s %8s %8s %8s %7s %6s %6s %9s %8s",
		"scenario", "peer", "maxδms", "p95δms", "meanδms", "drift%", "rprts", "wrts", "maxfrac", "endBPM")
	for _, cfg := range shapeScenarios() {
		cfg.duration = simRun
		res := runSim(cfg)
		for _, pr := range res.peers {
			t.Logf("%-24s %-10s %8.1f %8.1f %8.1f %7.1f %6d %6d %9.6f %8.3f",
				res.name, pr.name, pr.maxAbsDeltaMs, pr.p95AbsDeltaMs, pr.meanAbsDeltaMs,
				pr.timeDriftedPct, pr.reports, pr.steerWrites, pr.maxSteerFraction, pr.endBPM)
		}
		t.Logf("%-24s %-10s re-anchors=%d roomΔ=%.3f roomEnd=%.3f heardSkewMax=%.1fms",
			"", "", res.tempos, res.roomExcursionBPM, res.roomEndBPM, res.heardSkewMaxMs)
	}
}

// TestTempoSimDeclaredOnly compares the two architectures over every shape:
// today's, where WAIL infers tempo intent from what it observes, against
// grid-alignment-only, where the room tempo moves only when someone declares one
// and alignment is left to hold the grids together.
//
// The grid is the integral of tempo, so the two cannot be fully separated: a
// standing tempo difference is a phase ramp, and the slew may only correct it at
// SlewMaxFraction (500 ppm, 0.06 BPM at 120 — of which crystal drift already
// spends 50-100). The column that decides the question is endδ against maxδ: if
// alignment is holding, |δ| ends small whatever it peaked at; if inference was
// load-bearing, δ ends at its peak and is still climbing.
func TestTempoSimDeclaredOnly(t *testing.T) {
	// Rebuild the scenarios for every run. Several injectors are stateful — the
	// one-shot ones latch a `fired` flag — so reusing a config across the two
	// modes silently skips the disturbance on the second pass and reports a
	// flawless run that never happened.
	build := func() []simConfig { return append(shapeScenarios(), declaredOnlyScenarios()...) }

	t.Logf("%-24s %-10s %-9s %8s %8s %8s %7s %6s", "scenario", "peer", "mode", "maxδms", "endδms", "meanδms", "drift%", "rprts")
	for i := range build() {
		for _, declared := range []bool{false, true} {
			run := build()[i]
			run.duration = simRun
			run.declaredOnly = declared
			mode := "inferred"
			if declared {
				mode = "declared"
			}
			res := runSim(run)
			for _, pr := range res.peers {
				t.Logf("%-24s %-10s %-9s %8.1f %8.1f %8.1f %7.1f %6d",
					res.name, pr.name, mode, pr.maxAbsDeltaMs, pr.endAbsDeltaMs,
					pr.meanAbsDeltaMs, pr.timeDriftedPct, pr.reports)
			}
			t.Logf("%-24s %-10s %-9s roomΔ=%.3f roomEnd=%.3f heardSkewMax=%.1fms",
				"", "", mode, res.roomExcursionBPM, res.roomEndBPM, res.heardSkewMaxMs)
		}
	}
}

// TestTempoSimAbsorbableOffset finds the largest standing tempo difference grid
// alignment can absorb on its own, with no tempo propagation at all. This is the
// number that decides how much of the tempo machinery is needed: a divergence
// the slew can hold needs nothing, and one it cannot must be propagated, because
// the grid is the integral of tempo and the error ramps without bound.
//
// Predicted ceiling: SlewMaxFraction is 500 ppm (0.06 BPM at 120), of which
// crystal drift already spends 50-100, so ~0.04-0.06 BPM.
func TestTempoSimAbsorbableOffset(t *testing.T) {
	oneMin := stepsIn(time.Minute)
	// Two ways a tempo can sit away from the room, and they behave nothing alike.
	// "set once" is a DAW whose project tempo is simply a different number: Link
	// holds one session tempo, so WAIL's first nudge changes it and the offset
	// ceases to exist. "held" is automation or an external clock re-asserting
	// every buffer, which overwrites each nudge before it can accumulate.
	for _, mode := range []string{"set-once", "held"} {
		t.Logf("--- %s", mode)
		t.Logf("%-8s %8s %8s %8s %8s %8s", "offset", "ppm", "maxδms", "endδms", "meanδms", "drift%")
		for _, off := range []float64{0.02, 0.04, 0.05, 0.055, 0.06, 0.1, 0.2, 0.5} {
			inject := knobTurn(oneMin, 120+off)
			if mode == "held" {
				inject = insistentDAW(oneMin, 120+off)
			}
			res := runSim(simConfig{
				name: fmt.Sprintf("%s-%.2f", mode, off), duration: simRun, declaredOnly: true,
				peers: []simPeerSpec{
					{name: "A-off", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
						disturb: inject},
					{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
				},
			})
			pr := res.peers[0]
			t.Logf("%-8.2f %8.0f %8.1f %8.1f %8.1f %8.1f",
				off, off/120*1e6, pr.maxAbsDeltaMs, pr.endAbsDeltaMs, pr.meanAbsDeltaMs, pr.timeDriftedPct)
		}
	}
	t.Logf("slew authority = %.0f ppm (%.3f BPM at 120); perceptual threshold %d ms",
		interval.SlewMaxFraction*1e6, interval.SlewAuthorityBPM(120), interval.AlignThresholdUs/1000)
}

// declaredOnlyScenarios are the two cases that decide the trade the other way
// round: the sanctioned path working, and the cost of losing inference.
func declaredOnlyScenarios() []simConfig {
	oneMin := stepsIn(time.Minute)
	return []simConfig{
		{
			// The sanctioned path: a peer declares 122 in WAIL's own UI. Must
			// propagate and leave both grids aligned in either mode.
			name: "10-declared-change-122",
			peers: []simPeerSpec{
				{name: "A-declares", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: declareOnce(2*oneMin, 122)},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
		{
			// The cost: a DAW knob turn of 2 BPM that inference would have
			// propagated. Declared-only cannot, so the grids should ramp apart at
			// 2/120 = 16667 ppm — 33x the slew's authority, audible in ~1.5s.
			// This is the case a prompt has to catch immediately.
			name: "11-unpropagated-DAW-2bpm",
			peers: []simPeerSpec{
				{name: "A-knob", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: knobTurn(2*oneMin, 122)},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		},
	}
}

// declareOnce fires one WAIL-UI tempo change, the path that bypasses inference.
func declareOnce(at int, to float64) func(*simPeer, int) {
	fired := false
	return func(p *simPeer, step int) {
		if step == at && !fired {
			fired = true
			p.declare(to)
		}
	}
}

// TestTempoSimFidelityGate is the check that makes every other number in this
// file worth reading: the harness must reproduce the two failures we already
// know today's code has. A model that cannot show us a bug we have seen in the
// field cannot be trusted to tell us a threshold is right.
//
// Both assertions stay after ADR-0008 lands, inverted — the wobble stops
// reporting, and the deliberate nudge survives instead of being reverted.
func TestTempoSimFidelityGate(t *testing.T) {
	oneMin := stepsIn(time.Minute)

	// 1. The reported field bug, at the threshold regime that produced it: a
	//    peer wobbling 119.9↔120 broadcasts its excursions as tempo changes and
	//    drags the whole room with it. #499 suppressed this by raising the bar
	//    0.01 → 0.25, which is why the regime has to be named explicitly — the
	//    bar it created is also the top of the dead band in case 2, so the two
	//    failures below are the same fix pulling in opposite directions.
	t.Run("wobble drags the room at the pre-499 bar", func(t *testing.T) {
		res := runSim(simConfig{
			name: "gate-wobble", duration: 5 * time.Minute,
			peers: []simPeerSpec{
				{name: "A-wobble", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					detector: tempoDetectorConfig{reportBarFixed: 0.01, noIntegerSnap: true},
					disturb:  afterStep(oneMin, wobble(119.9, 120.0, 2*time.Second))},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		})
		t.Logf("reports=%d roomΔ=%.3f roomEnd=%.3f", res.peers[0].reports, res.roomExcursionBPM, res.roomEndBPM)
		if res.peers[0].reports == 0 {
			t.Errorf("harness does not reproduce the wobble bug: 0 tempo reports from a wobbling peer")
		}
		if res.roomExcursionBPM == 0 {
			t.Errorf("harness does not reproduce the wobble bug: the room tempo never moved")
		}
	})

	// 1b. …and today's bar suppresses it. Pins what #499 actually bought, so a
	//     later threshold change cannot quietly give it back.
	t.Run("today's bar suppresses the wobble", func(t *testing.T) {
		res := runSim(simConfig{
			name: "gate-wobble-today", duration: 5 * time.Minute,
			peers: []simPeerSpec{
				{name: "A-wobble", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: afterStep(oneMin, wobble(119.9, 120.0, 2*time.Second))},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		})
		t.Logf("reports=%d roomΔ=%.3f", res.peers[0].reports, res.roomExcursionBPM)
		if res.peers[0].reports != 0 || res.roomExcursionBPM != 0 {
			t.Errorf("the 0.25 bar should hold the wobble: reports=%d roomΔ=%.3f",
				res.peers[0].reports, res.roomExcursionBPM)
		}
	})

	// 2. The dead band that used to sit between the slew's authority (0.06 BPM at
	//    120) and the reporting bar (0.25). Measured on the pre-ADR-0008 code,
	//    this scenario ended with the session back at exactly 120.0000 and a
	//    single steering write of 0.002163 — 0.216%, 3.75 cents, four times the
	//    cap's own audibility budget — with the room never told. The user's
	//    deliberate change vanished silently.
	//
	//    Now: the slew nudges from what it observed, so it cannot write over the
	//    user's tempo at any threshold setting; and the reporting bar is the
	//    slew's authority, so what the slew cannot hold is what the room hears.
	//    The change survives locally AND reaches the room.
	t.Run("deliberate nudge survives and reaches the room", func(t *testing.T) {
		res := runSim(simConfig{
			name: "gate-nudge", duration: 5 * time.Minute,
			peers: []simPeerSpec{
				{name: "A-nudge", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: knobTurn(oneMin, 120.2)},
				{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		})
		got := res.peers[0].endBPM
		t.Logf("reports=%d endBPM=%.4f roomEnd=%.4f steerWrites=%d maxSteerFraction=%.6f",
			res.peers[0].reports, got, res.roomEndBPM, res.peers[0].steerWrites, res.peers[0].maxSteerFraction)
		// The user's tempo survives: nearer 120.2 than 120.
		if math.Abs(got-120.2) > math.Abs(got-120.0) {
			t.Errorf("the nudge was reverted: session held %.4f, want ~120.2", got)
		}
		// And the room hears it, so no peer is left steering against a tempo the
		// room does not know about.
		if res.peers[0].reports == 0 {
			t.Errorf("the 0.2 BPM nudge was never reported — the dead band is still open")
		}
		if math.Abs(res.roomEndBPM-120.2) > 0.01 {
			t.Errorf("room tempo ended at %.4f, want ~120.2", res.roomEndBPM)
		}
		// Pre-fix this was 0.002163 (3.75 cents). The audibility invariant itself
		// is pinned in internal/align, where the episode base is visible —
		// maxSteerFraction here is the step between consecutive writes, which a
		// mid-episode sign flip can legitimately double.
		if f := res.peers[0].maxSteerFraction; f > 2*interval.SlewMaxFraction+1e-9 {
			t.Errorf("steering step of %.6f is more than a sign flip's worth of the %.6f cap",
				f, interval.SlewMaxFraction)
		}
	})
}

// TestTempoSimWobbleSweep maps where today's protection actually holds, over
// both axes of a wobble: how far it swings and how long it dwells at each
// extreme. Amplitude has to vary — a sweep at 0.1 alone measures nothing,
// because tempoIntegerSnap pulls 119.9 back onto 120 at every dwell, so the
// curve is flat for a reason that has nothing to do with dwell.
//
// The two columns to read together: `reports` is what leaks into the room, and
// `maxfrac` is the largest single tempo write, against the 0.0005 (0.86 cent)
// bound the slew cap is supposed to guarantee. A shape can be silent to the
// room and still be audible locally.
func TestTempoSimWobbleSweep(t *testing.T) {
	oneMin := stepsIn(time.Minute)
	t.Logf("%-6s %-8s %8s %8s %10s %8s", "amp", "dwell", "reports", "roomΔ", "maxfrac", "endBPM")
	for _, amp := range []float64{0.1, 0.15, 0.3, 0.5, 1.0} {
		for _, dwell := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
			res := runSim(simConfig{
				name:     fmt.Sprintf("wobble-%.2f-%s", amp, dwell),
				duration: 5 * time.Minute,
				peers: []simPeerSpec{
					{name: "A-wobble", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
						disturb: afterStep(oneMin, wobble(120-amp, 120.0, dwell))},
					{name: "B", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
				},
			})
			t.Logf("%-6.2f %-8s %8d %8.3f %10.6f %8.3f",
				amp, dwell, res.peers[0].reports, res.roomExcursionBPM,
				res.peers[0].maxSteerFraction, res.peers[0].endBPM)
		}
	}
}

// TestTempoSimWobbleRoomTempo asks whether the wobble's harm comes from its
// excursions or from the room sitting at the wrong centre. Against a room at
// 120 the low excursion accrues error while the high one cannot recover it (our
// nudge is overwritten within a poll), so the error ratchets one way. Against a
// room at the wobble's own mean the two excursions should cancel instead.
func TestTempoSimWobbleRoomTempo(t *testing.T) {
	oneMin := stepsIn(time.Minute)
	for _, room := range []float64{120.0, 119.95} {
		res := runSim(simConfig{
			name: fmt.Sprintf("wobble-room-%.2f", room), duration: simRun,
			roomBPM: room, declaredOnly: true,
			peers: []simPeerSpec{
				{name: "A-wobble", beat0: 3.7, bpm: room, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: afterStep(oneMin, wobble(119.9, 120.0, 2*time.Second))},
				{name: "B", beat0: 100.4, bpm: room, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		})
		pr := res.peers[0]
		t.Logf("room=%.2f  maxδ=%.1fms endδ=%.1fms meanδ=%.1fms drift=%.1f%% heardSkew=%.1fms",
			room, pr.maxAbsDeltaMs, pr.endAbsDeltaMs, pr.meanAbsDeltaMs, pr.timeDriftedPct, res.heardSkewMaxMs)
	}
}

// TestTempoSimLanDeviceTug replays the real LAN topology — WAIL, a DAW, and a
// third Link device republishing its own measured clock — and asks the two
// questions that decide whether the room should follow a peer's actual mean.
//
//  1. Does following help, with a continuous tug rather than a square wave?
//  2. With three peers whose devices sit at different tempos, does following
//     ping-pong the room the way the two-insistent-LANs flap does?
func TestTempoSimLanDeviceTug(t *testing.T) {
	oneMin := stepsIn(time.Minute)
	// 119.90..120.00, mean 119.95 — the field wobble, as a tug.
	device := func(nominal float64) func(*simPeer, int) {
		return afterStep(oneMin, lanLinkDevice(nominal, 0.05, 0.02, 250))
	}

	t.Log("--- one wobbling LAN, room at 120 vs at the device's mean")
	for _, room := range []float64{120.0, 119.95} {
		res := runSim(simConfig{
			name: fmt.Sprintf("tug-room-%.2f", room), duration: simRun,
			roomBPM: room, declaredOnly: true,
			peers: []simPeerSpec{
				{name: "A-tugged", beat0: 3.7, bpm: room, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
					disturb: device(119.95)},
				{name: "B", beat0: 100.4, bpm: room, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000},
			},
		})
		pr := res.peers[0]
		t.Logf("room=%.2f  maxδ=%.1fms endδ=%.1fms meanδ=%.1fms drift=%.1f%% heardSkew=%.1fms",
			room, pr.maxAbsDeltaMs, pr.endAbsDeltaMs, pr.meanAbsDeltaMs, pr.timeDriftedPct, res.heardSkewMaxMs)
	}

	t.Log("--- three LANs, devices at 119.90 / 120.00 / 120.10, room follows")
	res := runSim(simConfig{
		name: "tug-three-way", duration: simRun, roomBPM: 120,
		peers: []simPeerSpec{
			{name: "A@119.90", beat0: 3.7, baseOffsetUs: -2_000_000, upUs: 25_000, downUs: 25_000,
				disturb: device(119.90)},
			{name: "B@120.00", beat0: 100.4, baseOffsetUs: 5_000_000, upUs: 30_000, downUs: 30_000,
				disturb: device(120.00)},
			{name: "C@120.10", beat0: -8.25, baseOffsetUs: 1_500_000, upUs: 20_000, downUs: 20_000,
				disturb: device(120.10)},
		},
	})
	for _, pr := range res.peers {
		t.Logf("  %-10s maxδ=%.1fms meanδ=%.1fms drift=%.1f%% reports=%d endBPM=%.4f",
			pr.name, pr.maxAbsDeltaMs, pr.meanAbsDeltaMs, pr.timeDriftedPct, pr.reports, pr.endBPM)
	}
	t.Logf("  room re-anchors=%d roomΔ=%.3f roomEnd=%.4f heardSkewMax=%.1fms",
		res.tempos, res.roomExcursionBPM, res.roomEndBPM, res.heardSkewMaxMs)
}
