//go:build !linkstub

package main

import "github.com/nicholasgasior/wail/wail-app/internal/abllink"

// captureSource is the send-side transport the engine drains: it yields PCM
// buffers already mapped to a local Link beat. *abllink.Source (Link Audio) is
// one implementation; a future IPC-backed CLAP send plugin is another, feeding the
// same capture→assemble→encode→relay path (ADR-0005).
type captureSource interface {
	// PopMapped drains the next buffer and resolves its begin beat in ss's local
	// Link timeline at the given quantum. popped is false when the ring is empty;
	// beatOK is false for a cross-session buffer that can't be placed (the caller
	// still observes its Count for loss tracking, then skips it). Merging the drain
	// and the beat resolution lets the Link adapter map while the C buffer_info is
	// still in hand — a package-main struct can't retain that C type.
	PopMapped(ss *abllink.SessionState, quantum float64) (buf captureBuffer, beat float64, beatOK, popped bool)
	// Dropped is the cumulative count of buffers the source's ring dropped.
	Dropped() uint64
	Close()
}

// captureBuffer is one drained buffer, transport-agnostic: the abllink.CaptureBuffer
// fields the drain loop needs, minus the C buffer_info (consumed inside PopMapped).
type captureBuffer struct {
	Count       uint64
	NumFrames   int
	NumChannels int
	SampleRate  uint32
	TempoBPM    float64
	Samples     []int16
}

// linkCaptureSource adapts a Link Audio *abllink.Source to captureSource, doing the
// SDK's session-aware begin-beat mapping (Pop + BeginBeats) in one step.
type linkCaptureSource struct{ s *abllink.Source }

func (l linkCaptureSource) PopMapped(ss *abllink.SessionState, quantum float64) (captureBuffer, float64, bool, bool) {
	b, ok := l.s.Pop()
	if !ok {
		return captureBuffer{}, 0, false, false
	}
	beat, beatOK := l.s.BeginBeats(&b, ss, quantum)
	return captureBuffer{
		Count:       b.Count,
		NumFrames:   b.NumFrames,
		NumChannels: b.NumChannels,
		SampleRate:  b.SampleRate,
		TempoBPM:    b.TempoBPM,
		Samples:     b.Samples,
	}, beat, beatOK, true
}

func (l linkCaptureSource) Dropped() uint64 { return l.s.Dropped() }
func (l linkCaptureSource) Close()          { l.s.Close() }

// emitSink is the playback-side transport the emit loop feeds paced chunks to.
// *abllink.Sink (Link Audio) satisfies it directly; a future IPC-backed CLAP recv
// plugin is the other implementation (ADR-0005). Each emit stream fans its chunks to
// all of its sinks.
type emitSink interface {
	WriteInterleaved(samples []int16, ss *abllink.SessionState, beatsAtBegin, quantum float64, numFrames, numChannels int, sampleRate uint32) bool
	SetName(name string)
	Close()
}

// fifoFlusher is implemented by emitSinks that render arrival order (FIFO —
// the CLAP recv plugin, ADR-0005) and therefore timed-release their queued
// chunks: the emit loop calls Flush each tick with the current beat so
// delivery time matches stamped time. Beat-stamped sinks (*abllink.Sink) don't
// need it — their subscribers render at the stamp, not at arrival.
type fifoFlusher interface {
	Flush(nowBeat, leadBeats float64)
}

// Compile-time checks that the Link Audio adapters satisfy the transport seams.
var (
	_ captureSource = linkCaptureSource{}
	_ emitSink      = (*abllink.Sink)(nil)
)
