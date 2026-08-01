package playout

import "testing"

// The adaptive scheduler (ADR-0009): rounds are the sender's own indices,
// released at the receiver's next boundary once ready, freshest-wins when a
// backlog forms. These tests drive it the way the engine will: NoteFrame-style
// candidate state in, one release decision per local boundary out.

func cand(idx int64, complete bool, firstSeen int64) RoundState {
	return RoundState{Index: idx, Complete: complete, FirstSeen: firstSeen}
}

func TestAdaptiveWaitsOneBoundaryForAStreamingRound(t *testing.T) {
	a := &Adaptive{}
	// Round 5's first frames arrived during this boundary's window: not ready —
	// it has had no streaming time. (A complete round would release, below.)
	if _, _, adv := a.OnBoundary(10, []RoundState{cand(5, false, 10)}); adv {
		t.Fatal("released a round that started arriving this boundary")
	}
	// One boundary later it is ready.
	release, skipped, adv := a.OnBoundary(11, []RoundState{cand(5, false, 10)})
	if !adv || release != 5 {
		t.Fatalf("boundary 11 → (%d,%v), want (5,true)", release, adv)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped %v on a single candidate", skipped)
	}
}

func TestAdaptiveCompleteRoundReleasesImmediately(t *testing.T) {
	a := &Adaptive{}
	// Complete means the final frame is in hand — no streaming time needed.
	release, _, adv := a.OnBoundary(10, []RoundState{cand(5, true, 10)})
	if !adv || release != 5 {
		t.Fatalf("complete round → (%d,%v), want (5,true)", release, adv)
	}
}

func TestAdaptiveSteadyStateAdvancesInOrder(t *testing.T) {
	a := &Adaptive{}
	a.OnBoundary(11, []RoundState{cand(5, true, 10)})
	for b, want := int64(12), int64(6); want <= 8; b, want = b+1, want+1 {
		release, skipped, adv := a.OnBoundary(b, []RoundState{cand(want, true, b-1)})
		if !adv || release != want || len(skipped) != 0 {
			t.Fatalf("boundary %d → (%d,%v,skip=%v), want (%d,true,none)", b, release, adv, skipped, want)
		}
	}
}

func TestAdaptiveFreshestWinsOnBacklog(t *testing.T) {
	a := &Adaptive{}
	a.OnBoundary(11, []RoundState{cand(5, true, 10)})
	// The network stalled and recovered: three unplayed rounds are ready.
	// NINJAM's rule (njclient.cpp:1305): the freshest plays, the stale queue is
	// skipped at the speakers (the archive already has them — recording taps
	// frames at receipt, not at playout).
	release, skipped, adv := a.OnBoundary(15, []RoundState{
		cand(6, true, 12), cand(7, true, 13), cand(8, true, 14),
	})
	if !adv || release != 8 {
		t.Fatalf("backlog → (%d,%v), want (8,true)", release, adv)
	}
	if len(skipped) != 2 || skipped[0] != 6 || skipped[1] != 7 {
		t.Fatalf("skipped = %v, want [6 7]", skipped)
	}
	if got := a.Skipped(); got != 2 {
		t.Fatalf("Skipped() = %d, want 2", got)
	}
}

func TestAdaptiveDoesNotSkipToAnUnreadyRound(t *testing.T) {
	a := &Adaptive{}
	a.OnBoundary(11, []RoundState{cand(5, true, 10)})
	// 6 is ready; 7 just started arriving. Play 6 — jumping to 7 would trade a
	// finished round for one whose tail is still in flight.
	release, skipped, adv := a.OnBoundary(12, []RoundState{
		cand(6, true, 11), cand(7, false, 12),
	})
	if !adv || release != 6 || len(skipped) != 0 {
		t.Fatalf("→ (%d,%v,skip=%v), want (6,true,none)", release, adv, skipped)
	}
}

func TestAdaptiveIdleSenderHolds(t *testing.T) {
	a := &Adaptive{}
	a.OnBoundary(11, []RoundState{cand(5, true, 10)})
	if _, _, adv := a.OnBoundary(12, nil); adv {
		t.Fatal("advanced with nothing buffered")
	}
	// Playing position is unchanged; a straggler for 5 still live-appends.
	if d := a.OnFrame(5); d != LiveAppend {
		t.Fatalf("frame for playing round = %v, want live-append", d)
	}
}

func TestAdaptiveDispositions(t *testing.T) {
	a := &Adaptive{}
	// Before any release everything buffers.
	if d := a.OnFrame(3); d != Buffer {
		t.Fatalf("pre-playout frame = %v, want buffer", d)
	}
	a.OnBoundary(11, []RoundState{cand(5, true, 10)})
	if d := a.OnFrame(6); d != Buffer {
		t.Fatalf("future round = %v, want buffer", d)
	}
	if d := a.OnFrame(5); d != LiveAppend {
		t.Fatalf("playing round = %v, want live-append", d)
	}
	if d := a.OnFrame(4); d != TooLate {
		t.Fatalf("finished round = %v, want too-late", d)
	}
}

func TestAdaptiveNonMonotonicBoundaryIsIgnored(t *testing.T) {
	a := &Adaptive{}
	a.OnBoundary(11, []RoundState{cand(5, true, 10)})
	if _, _, adv := a.OnBoundary(11, []RoundState{cand(6, true, 10)}); adv {
		t.Fatal("advanced on a duplicate boundary")
	}
	if _, _, adv := a.OnBoundary(9, []RoundState{cand(6, true, 8)}); adv {
		t.Fatal("advanced on a boundary from the past")
	}
}

func TestAdaptiveSenderRestartRepins(t *testing.T) {
	a := &Adaptive{}
	a.OnBoundary(11, []RoundState{cand(500, true, 10)})
	// The sender's app restarted: its indices reset near zero. Far-below
	// candidates would read as TooLate forever without a reset rule.
	release, _, adv := a.OnBoundary(13, []RoundState{cand(3, true, 12)})
	if !adv || release != 3 {
		t.Fatalf("post-restart → (%d,%v), want (3,true)", release, adv)
	}
	if d := a.OnFrame(3); d != LiveAppend {
		t.Fatalf("frame for re-pinned round = %v, want live-append", d)
	}
}

func TestAdaptiveStalenessIsBoundedProperty(t *testing.T) {
	// Whatever happened before, a boundary that sees a ready round strictly
	// newer than the playing one must advance to the newest ready round. This
	// is the property that makes staleness self-healing (the pin's ratchet was
	// the reason ADR-0009 rejected pinning without skips).
	a := &Adaptive{}
	b := int64(10)
	playing := int64(-1)
	for i := int64(0); i < 200; i++ {
		b++
		// Rounds arrive in bursts of 0..3, always ready.
		var cands []RoundState
		for j := int64(0); j <= i%4; j++ {
			cands = append(cands, cand(i+j, true, b-1))
		}
		release, _, adv := a.OnBoundary(b, cands)
		if adv {
			if release <= playing {
				t.Fatalf("boundary %d: released %d after %d — went backwards", b, release, playing)
			}
			playing = release
			want := cands[len(cands)-1].Index
			if release != want {
				t.Fatalf("boundary %d: released %d, want newest ready %d", b, release, want)
			}
		}
	}
}
