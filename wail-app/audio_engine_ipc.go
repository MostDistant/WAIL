//go:build !linkstub

package main

import (
	"fmt"
	"log"

	"github.com/nicholasgasior/wail/wail-app/internal/affinity"
)

// ipcPluginStreamIDBase offsets plugin-send WAIF stream ids into the top half of
// the uint16 range so they never collide with Link Audio capture channels (which
// number up from 0). MVP: the WAIF stream id is base+streamIndex, so users pick a
// distinct Stream Index per Send-plugin instance. A unified allocator is a follow-up.
const ipcPluginStreamIDBase uint16 = 0x8000

// SetRecvPool gives the engine the writer pool shared by all connected Recv
// plugins and retro-attaches an ipcEmitSink to every already-published stream, so
// remote audio reaches the plugins as well as Link Audio. Called once, right after
// the IPC server starts.
func (e *linkAudioEngine) SetRecvPool(pool *IPCWriterPool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recvPool = pool
	for _, st := range e.emit {
		st.sinks = append(st.sinks, newIPCEmitSink(pool, st.key.Identity, st.key.Stream))
	}
}

// AddPluginSource registers a CLAP Send plugin as a capture channel and returns the
// source the server pushes decoded PCM into, plus the channel key and epoch for
// removal. A reconnecting instance (same streamIndex) reuses its channel + WAIF
// stream id so receivers keep the same published channel (affinity); the fresh epoch
// makes the prior connection's late RemovePluginSource a no-op.
func (e *linkAudioEngine) AddPluginSource(streamIndex uint16, name string) (*ipcCaptureSource, string, uint64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopping {
		return nil, "", 0, false
	}
	key := fmt.Sprintf("ipc:%d", streamIndex)
	// A prior connection for this index (e.g. a reconnect that raced the old
	// connection's cleanup) is torn down and REPLACED with a fresh channel — never
	// re-armed in place. The old drain goroutine winds down on its own detached
	// struct, so it can't race the new goroutine over a shared source/encoder (HIGH).
	if old, ok := e.capture[key]; ok {
		e.stopCaptureLocked(old)
		delete(e.capture, key)
	}
	e.pluginEpoch++
	epoch := e.pluginEpoch
	src := newIPCCaptureSource(e.link)
	ch := &captureChannel{
		name:     name,
		peerName: e.peerName,
		streamID: ipcPluginStreamIDBase + streamIndex, // deterministic ⇒ affinity preserved across reconnects
		epoch:    epoch,
	}
	if !e.startDrainLocked(ch, src, 2) {
		return nil, "", 0, false
	}
	e.capture[key] = ch
	log.Printf("[audio] plugin send stream %d registered (%s, WAIF stream %d)", streamIndex, key, ch.streamID)
	return src, key, epoch, true
}

// RemovePluginSource tears down a plugin capture channel on disconnect — but only if
// epoch still matches, so a dropped connection's cleanup can't remove the channel a
// newer reconnection already re-armed.
func (e *linkAudioEngine) RemovePluginSource(key string, epoch uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ch, ok := e.capture[key]; ok && ch.epoch == epoch {
		e.stopCaptureLocked(ch)
		delete(e.capture, key)
		log.Printf("[audio] plugin send %s disconnected", key)
	}
}

// SetPluginSourceName renames a plugin capture channel from the Send plugin's
// DAW track name (clap.track-info). Epoch-guarded like RemovePluginSource: a
// stale connection's late rename can't touch a newer reconnection's channel.
// The new name reaches the room on the next status tick via effectiveStreamNames.
func (e *linkAudioEngine) SetPluginSourceName(key string, epoch uint64, name string) {
	if name == "" {
		return // no track-info from the host: keep the "Plugin Send N" placeholder
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.capture[key]
	if !ok || ch.epoch != epoch || ch.name == name {
		return
	}
	log.Printf("[audio] plugin send %s renamed %q → %q (DAW track name)", key, ch.name, name)
	ch.name = name
}

// StreamNameFrames snapshots a length-framed StreamName message for every
// currently-published stream, so a freshly-connected Recv plugin can label the ports
// it assigns to streams whose audio is already flowing (their live SetName broadcasts
// predate this connection). Built under the lock (no blocking I/O); the caller
// enqueues them via the writer pool so they share the connection's one serialized
// writer with Broadcast.
func (e *linkAudioEngine) StreamNameFrames() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	frames := make([][]byte, 0, len(e.emit))
	for _, st := range e.emit {
		name := affinity.FormatName(st.lastDisplayName, streamLabel(st.lastStreamName, st.key.Stream))
		frames = append(frames, EncodeFrame(EncodeStreamName(st.key.Identity, st.key.Stream, name)))
	}
	return frames
}
