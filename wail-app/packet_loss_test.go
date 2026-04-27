package main

import "testing"

func newPeer() *PeerState {
	return NewPeerState(nil)
}

func hdr(streamID uint16, intervalIdx int64, seq uint32) *WaifHeaderPeek {
	return &WaifHeaderPeek{
		StreamID:      streamID,
		IntervalIndex: intervalIdx,
		FrameSeq:      seq,
	}
}

func TestRecordFrameContiguousNoLoss(t *testing.T) {
	p := newPeer()
	for i := uint32(0); i < 10; i++ {
		if ev := recordFrame(p, hdr(0, 0, i)); ev != nil {
			t.Fatalf("unexpected loss event at seq %d: %+v", i, ev)
		}
	}
	if p.PacketsLost != 0 {
		t.Fatalf("expected 0 packets lost, got %d", p.PacketsLost)
	}
	if p.LossEvents != 0 {
		t.Fatalf("expected 0 loss events, got %d", p.LossEvents)
	}
	if p.TotalFramesReceived != 10 {
		t.Fatalf("expected 10 frames received, got %d", p.TotalFramesReceived)
	}
}

func TestRecordFrameSingleGap(t *testing.T) {
	p := newPeer()
	recordFrame(p, hdr(0, 0, 0))
	recordFrame(p, hdr(0, 0, 1))
	ev := recordFrame(p, hdr(0, 0, 3))
	if ev == nil {
		t.Fatal("expected loss event for gap 2→3")
	}
	if ev.Lost != 1 || ev.ExpectedSeq != 2 || ev.GotSeq != 3 {
		t.Fatalf("bad event: %+v", ev)
	}
	if ev.StreamID != 0 {
		t.Fatalf("bad stream: %d", ev.StreamID)
	}
	recordFrame(p, hdr(0, 0, 4))
	if p.PacketsLost != 1 {
		t.Fatalf("expected 1 lost, got %d", p.PacketsLost)
	}
	if p.LossEvents != 1 {
		t.Fatalf("expected 1 loss event, got %d", p.LossEvents)
	}
}

func TestRecordFrameLargeJump(t *testing.T) {
	p := newPeer()
	for i := uint32(0); i < 10; i++ {
		recordFrame(p, hdr(0, 0, i))
	}
	ev := recordFrame(p, hdr(0, 5, 20))
	if ev == nil || ev.Lost != 10 {
		t.Fatalf("expected 10 lost, got %+v", ev)
	}
	if ev.IntervalIdx != 5 {
		t.Fatalf("expected interval 5 in event, got %d", ev.IntervalIdx)
	}
	if p.PacketsLost != 10 {
		t.Fatalf("expected 10 cumulative lost, got %d", p.PacketsLost)
	}
	if p.LossEvents != 1 {
		t.Fatalf("expected 1 loss event, got %d", p.LossEvents)
	}
}

func TestRecordFrameIndependentStreams(t *testing.T) {
	p := newPeer()
	// Stream 0 contiguous
	for i := uint32(0); i < 5; i++ {
		recordFrame(p, hdr(0, 0, i))
	}
	// Stream 1 with gap
	recordFrame(p, hdr(1, 0, 100))
	recordFrame(p, hdr(1, 0, 101))
	ev := recordFrame(p, hdr(1, 0, 105))
	if ev == nil {
		t.Fatal("expected loss on stream 1")
	}
	if ev.StreamID != 1 || ev.Lost != 3 {
		t.Fatalf("bad event: %+v", ev)
	}
	// Stream 0 continues contiguous
	if ev := recordFrame(p, hdr(0, 0, 5)); ev != nil {
		t.Fatalf("unexpected loss on stream 0: %+v", ev)
	}
	if p.PacketsLost != 3 {
		t.Fatalf("expected 3 total lost, got %d", p.PacketsLost)
	}
}

func TestRecordFrameReorderNoDoubleCount(t *testing.T) {
	p := newPeer()
	recordFrame(p, hdr(0, 0, 0))
	// Advance past seq 1 (marked lost), then seq 1 arrives late.
	ev := recordFrame(p, hdr(0, 0, 2))
	if ev == nil || ev.Lost != 1 {
		t.Fatalf("expected 1 lost for gap, got %+v", ev)
	}
	if reorder := recordFrame(p, hdr(0, 0, 1)); reorder != nil {
		t.Fatalf("reorder should not emit loss event, got %+v", reorder)
	}
	if p.PacketsLost != 1 {
		t.Fatalf("expected 1 lost (not 2), got %d", p.PacketsLost)
	}
	if p.ReorderEvents != 1 {
		t.Fatalf("expected 1 reorder event, got %d", p.ReorderEvents)
	}
	if p.LossEvents != 1 {
		t.Fatalf("expected exactly 1 loss event (not recounted), got %d", p.LossEvents)
	}
}

func TestRecordFrameFirstSeqNotZero(t *testing.T) {
	p := newPeer()
	if ev := recordFrame(p, hdr(0, 0, 500)); ev != nil {
		t.Fatalf("first frame should not report loss: %+v", ev)
	}
	if p.PacketsLost != 0 {
		t.Fatalf("expected 0 lost, got %d", p.PacketsLost)
	}
	if ev := recordFrame(p, hdr(0, 0, 501)); ev != nil {
		t.Fatalf("contiguous should not report loss: %+v", ev)
	}
	if ev := recordFrame(p, hdr(0, 0, 504)); ev == nil || ev.Lost != 2 {
		t.Fatalf("expected 2 lost on gap 502→504, got %+v", ev)
	}
}

func TestRecordFrameWrapAround(t *testing.T) {
	p := newPeer()
	const near = ^uint32(0) // 0xFFFFFFFF
	recordFrame(p, hdr(0, 0, near-1))
	if ev := recordFrame(p, hdr(0, 0, near)); ev != nil {
		t.Fatalf("contiguous wrap-setup: %+v", ev)
	}
	// Wrap from MAX to 0 is contiguous.
	if ev := recordFrame(p, hdr(0, 0, 0)); ev != nil {
		t.Fatalf("wrap 0xFFFFFFFF→0 should not be loss: %+v", ev)
	}
	if p.PacketsLost != 0 {
		t.Fatalf("expected 0 lost across wrap, got %d", p.PacketsLost)
	}
}
