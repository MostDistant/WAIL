//go:build !linkstub

package main

import (
	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
	"github.com/nicholasgasior/wail/wail-app/internal/emit"
)

// ipcEmitSink is an emitSink that ships a stream's paced playout chunks to any
// connected CLAP Recv plugins as RemotePCM over IPC (ADR-0005). One is attached to
// each emit stream alongside its Link Audio sink; both receive the same beat-paced,
// already-one-interval-late chunks.
//
// Unlike a Link Audio subscriber, a recv plugin plays FIFO — it has no beat
// stamps to render against — so chunks must be *delivered* at their stamped
// beat, not cushion-ahead of it. Writes therefore go into a timed-release
// queue (emit.DueQueue) and Flush, driven by the emit loop each tick, releases
// what is due. Feeding the plugin cushion-ahead chunks directly made all
// remote audio play ~cushion late (the steady sub-interval offset bug).
type ipcEmitSink struct {
	pool     *IPCWriterPool
	peerID   string
	streamID uint16
	queue    emit.DueQueue
	channels int
	rate     uint32
}

func newIPCEmitSink(pool *IPCWriterPool, peerID string, streamID uint16) *ipcEmitSink {
	return &ipcEmitSink{pool: pool, peerID: peerID, streamID: streamID}
}

func (s *ipcEmitSink) WriteInterleaved(samples []int16, _ *abllink.SessionState, beatsAtBegin, _ float64, numFrames, numChannels int, sampleRate uint32) bool {
	if s.pool.IsEmpty() {
		return true // no recv plugin connected — skip the queue entirely
	}
	n := numFrames * numChannels
	if n > len(samples) {
		n = len(samples)
	}
	s.channels, s.rate = numChannels, sampleRate
	// PacedReader.Next hands us a fresh slice per chunk, safe to retain.
	s.queue.Push(beatsAtBegin, samples[:n])
	return true
}

// Flush releases every queued chunk whose stamped beat is due at nowBeat
// (minus leadBeats of transport-jitter margin) to all connected recv plugins.
func (s *ipcEmitSink) Flush(nowBeat, leadBeats float64) {
	if s.pool.IsEmpty() {
		return
	}
	s.queue.FlushDue(nowBeat, leadBeats, func(samples []int16) {
		s.pool.Broadcast(EncodeFrame(EncodeRemotePCM(s.peerID, s.streamID, byte(s.channels), s.rate, 0, samples)))
	})
}

func (s *ipcEmitSink) SetName(name string) {
	if s.pool.IsEmpty() {
		return // late-connecting plugins get names via the server's on-connect replay
	}
	s.pool.Broadcast(EncodeFrame(EncodeStreamName(s.peerID, s.streamID, name)))
}

func (s *ipcEmitSink) Close() {
	s.pool.Broadcast(EncodeFrame(EncodeStreamGone(s.peerID, s.streamID)))
}
