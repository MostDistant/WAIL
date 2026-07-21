// Package pace smooths interval frame bursts into a steady send cadence.
//
// The capture engine encodes a whole interval at the boundary, producing
// hundreds of WAIF frames at once. Bursting them into the relay socket
// overflows every fixed-size queue between encoder and wire (and trips
// server-side rate limiting), so a Sender spaces the frames out instead:
// enqueue the batch, and frames go to the send func one at a time with a
// fixed gap between them.
//
// The gap is half the frame duration (10ms for 20ms Opus frames), i.e. 2×
// real time. With the default offset D=1 the receiver starts playing an
// interval at the same boundary at which sending starts, so delivery cannot
// beat the boundary — the invariant is staying ahead of the playhead: frame
// k plays at offset k×20ms and is sent at k×10ms, a margin that grows
// through the interval. (The first few frames arrive ~network-latency late,
// which internal/playout live-appends — same as before pacing.) 1× would
// leave zero catch-up margin; much faster re-approaches the burst.
//
// The Sender never blocks the caller: Enqueue is non-blocking and drops the
// whole batch (reported via onDrop) if the small batch queue is full, which
// can only happen if delivery is slower than capture for several intervals.
package pace

import (
	"sync"
	"time"
)

// Sender paces frame batches to a send func. One Sender per outgoing stream.
type Sender struct {
	gap    time.Duration
	send   func([]byte)
	onDrop func(frames int)

	q         chan [][]byte
	done      chan struct{}
	closeOnce sync.Once
}

// New creates a Sender emitting one frame per gap. queueDepth bounds how many
// batches may wait behind the one being sent; onDrop (optional) is called with
// the frame count of any batch discarded because the queue was full.
func New(gap time.Duration, queueDepth int, send func([]byte), onDrop func(frames int)) *Sender {
	if queueDepth < 1 {
		queueDepth = 1
	}
	s := &Sender{
		gap:    gap,
		send:   send,
		onDrop: onDrop,
		q:      make(chan [][]byte, queueDepth),
		done:   make(chan struct{}),
	}
	go s.run()
	return s
}

// Enqueue queues a batch for paced sending. Non-blocking: if the Sender is
// closed the batch is ignored; if the queue is full the batch is dropped and
// reported via onDrop.
func (s *Sender) Enqueue(frames [][]byte) {
	if len(frames) == 0 {
		return
	}
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.q <- frames:
	default:
		if s.onDrop != nil {
			s.onDrop(len(frames))
		}
	}
}

// Close stops the Sender. Frames not yet sent are discarded; safe to call
// more than once.
func (s *Sender) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

func (s *Sender) run() {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-s.done:
			return
		case frames := <-s.q:
			for i, f := range frames {
				if i > 0 && s.gap > 0 {
					if timer == nil {
						timer = time.NewTimer(s.gap)
					} else {
						timer.Reset(s.gap)
					}
					select {
					case <-s.done:
						return
					case <-timer.C:
					}
				}
				s.send(f)
			}
		}
	}
}
