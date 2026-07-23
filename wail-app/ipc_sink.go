//go:build !linkstub

package main

import "github.com/nicholasgasior/wail/wail-app/internal/abllink"

// ipcEmitSink is an emitSink that ships a stream's paced playout chunks to any
// connected CLAP Recv plugins as RemotePCM over IPC (ADR-0005). One is attached to
// each emit stream alongside its Link Audio sink; both receive the same beat-paced,
// already-one-interval-late chunks. A recv plugin pulls at its device clock, so the
// Link beat/quantum are irrelevant here and ignored.
type ipcEmitSink struct {
	pool     *IPCWriterPool
	peerID   string
	streamID uint16
}

func newIPCEmitSink(pool *IPCWriterPool, peerID string, streamID uint16) *ipcEmitSink {
	return &ipcEmitSink{pool: pool, peerID: peerID, streamID: streamID}
}

func (s *ipcEmitSink) WriteInterleaved(samples []int16, _ *abllink.SessionState, _, _ float64, numFrames, numChannels int, sampleRate uint32) bool {
	if s.pool.IsEmpty() {
		return true // no recv plugin connected — skip the encode entirely
	}
	n := numFrames * numChannels
	if n > len(samples) {
		n = len(samples)
	}
	s.pool.Broadcast(EncodeFrame(EncodeRemotePCM(s.peerID, s.streamID, byte(numChannels), sampleRate, 0, samples[:n])))
	return true
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
