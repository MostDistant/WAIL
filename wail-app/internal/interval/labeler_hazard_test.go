package interval

import (
	"math"
	"testing"
)

// These tests document a PROTOCOL hazard in how the engine aligns the
// RoomLabeler (SetRoomAnchor), not a defect in the labeler's arithmetic. The
// relay's interval_anchor carries the room index sampled at the relay at send
// time; each client aligns it against its own local index sampled at RECEIPT
// time (the anchor's server_now_micros field is currently discarded). The
// offset a peer fixes therefore depends on its receipt instant and on its
// local grid's phase — so two peers holding the same anchor can disagree by a
// whole interval, persistently, and every receiver then plays their audio one
// interval apart. Every new anchor re-rolls each peer's offset independently.
//
// The assertions below pin the CURRENT (hazardous) outcomes as executable
// documentation; a protocol fix (e.g. RTT-compensated or boundary-quantized
// alignment) should update them deliberately.

// stepGrid is a minimal interval clock: index k spans
// [start + k*period, start + (k+1)*period).
type stepGrid struct {
	startMicros  int64
	periodMicros int64
}

func (g stepGrid) indexAt(tMicros int64) int64 {
	return int64(math.Floor(float64(tMicros-g.startMicros) / float64(g.periodMicros)))
}

// alignLikeEngine reproduces SetRoomAnchor: fix offset = anchorRoomIndex - localNow.
func alignLikeEngine(l *RoomLabeler, anchorRoomIndex int64, local stepGrid, recvMicros int64) int64 {
	l.Align(anchorRoomIndex, local.indexAt(recvMicros))
	return l.Offset()
}

// Scenario: same anchor, zero network delay, but the peers' local grids sit at
// different phases. Their fixed offsets differ by one — so the SAME musical
// moment leaves B labeled one room interval behind C, for every interval
// until the next anchor re-rolls the dice.
func TestLabelerHazardGridPhaseDisagreement(t *testing.T) {
	const period = 8_000_000 // 8s intervals
	room := stepGrid{0, period}
	localB := stepGrid{2_000_000, period} // B's boundaries 2s after the room's
	localC := stepGrid{6_000_000, period} // C's 6s after

	const anchorAt = 4_000_000
	roomIdx := room.indexAt(anchorAt) // 0 — what the relay broadcasts

	var lb, lc RoomLabeler
	offB := alignLikeEngine(&lb, roomIdx, localB, anchorAt)
	offC := alignLikeEngine(&lc, roomIdx, localC, anchorAt)

	if offB == offC {
		t.Fatalf("expected offsets to disagree, both = %d", offB)
	}
	// For every later moment, the same absolute-time audio carries room labels
	// one interval apart: receivers release B's copy at boundary N+D and C's at
	// N+1+D. Persistent, not jitter.
	for _, tm := range []int64{7_000_000, 15_000_000, 23_000_000, 31_000_000} {
		b, _ := lb.RoomIndex(localB.indexAt(tm))
		c, _ := lc.RoomIndex(localC.indexAt(tm))
		if c-b != 1 {
			t.Fatalf("at %dus labels B=%d C=%d — expected C one interval ahead of B", tm, b, c)
		}
	}
}

// Scenario: identical grids, one anchor, but the peers' one-way delays
// straddle a grid tick (WAN transit eats the design's "interval of slack").
// The peer receiving after the tick fixes an offset one lower — its audio is
// a full interval late at every receiver until the next anchor.
func TestLabelerHazardTransitStraddlesTick(t *testing.T) {
	const period = 8_000_000
	room := stepGrid{0, period}
	local := stepGrid{0, period} // grids phase-aligned with the room (best case)

	const sentAt = 7_900_000
	roomIdx := room.indexAt(sentAt) // 0 — valid at send time only

	var lb, lc RoomLabeler
	offB := alignLikeEngine(&lb, roomIdx, local, 7_950_000) // arrives before the tick
	offC := alignLikeEngine(&lc, roomIdx, local, 8_050_000) // arrives after it

	if offB-offC != 1 {
		t.Fatalf("expected offsets 0 and -1, got %d and %d", offB, offC)
	}
	// At t=9s the room is at index 1; B labels the moment 1 (correct) while C
	// labels it 0 — C reads as a full interval late from then on.
	b, _ := lb.RoomIndex(local.indexAt(9_000_000))
	c, _ := lc.RoomIndex(local.indexAt(9_000_000))
	if b != 1 || c != 0 {
		t.Fatalf("labels at 9s: B=%d (want 1), C=%d (want 0)", b, c)
	}
}
