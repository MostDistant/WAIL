package main

// LossEvent describes a detected packet-loss gap on one (peer, stream) pair.
type LossEvent struct {
	StreamID    uint16
	IntervalIdx int64
	ExpectedSeq uint32
	GotSeq      uint32
	Lost        uint64
}

// recordFrame updates per-peer receive counters based on one incoming WAIF frame header.
//
// Returns a non-nil *LossEvent if the frame's sequence number is ahead of the
// last observed seq (i.e. one or more intervening frames were lost). Returns
// nil when the frame is the first seen on that stream, is exactly the expected
// next seq, or is a reordered arrival (seq < expected).
//
// Caller must hold the PeerRegistry lock (e.g. inside `peers.WithPeer`).
func recordFrame(p *PeerState, h *WaifHeaderPeek) *LossEvent {
	p.TotalFramesReceived++

	track, ok := p.StreamTracks[h.StreamID]
	if !ok {
		track = &StreamTrack{}
		p.StreamTracks[h.StreamID] = track
	}

	if !track.HasFirst {
		track.HasFirst = true
		track.NextExpectedSeq = h.FrameSeq + 1
		return nil
	}

	switch {
	case h.FrameSeq == track.NextExpectedSeq:
		track.NextExpectedSeq++
		return nil

	case seqLess(h.FrameSeq, track.NextExpectedSeq):
		// Arrived out of order (seq < expected). Already counted as "lost"
		// when we advanced past it; don't double-count. Ahead-counter stays put.
		p.ReorderEvents++
		return nil

	default:
		lost := uint64(h.FrameSeq - track.NextExpectedSeq)
		event := &LossEvent{
			StreamID:    h.StreamID,
			IntervalIdx: h.IntervalIndex,
			ExpectedSeq: track.NextExpectedSeq,
			GotSeq:      h.FrameSeq,
			Lost:        lost,
		}
		p.PacketsLost += lost
		p.LossEvents++
		track.NextExpectedSeq = h.FrameSeq + 1
		return event
	}
}

// seqLess reports whether a is "less than" b under u32 wrap-around arithmetic.
// A negative signed diff means a is before b.
func seqLess(a, b uint32) bool {
	return int32(a-b) < 0
}
