// Package abllink is a cgo binding to Ableton Link's `abl_link` C API (sync +
// audio), compiled directly against the vendored Link 4.0 SDK at vendor/link.
//
// It replaces the external abletonlink-go dependency and owns the cross-platform
// C++ build (ADR-0002). A single abl_link handle carries both Link sync and Link
// Audio, because WAIL is exactly one LAN peer and sync/audio must share it.
//
// The realtime Link Audio capture callback must remain a pure C function (see
// source.go / the C ring), never a Go //export: a Go callback would block Link's
// audio thread through a GC pause and drop incoming UDP. The sync callbacks here
// are control-plane only and WAIL polls state at 50Hz rather than using them.
package abllink

/*
#cgo CXXFLAGS: -std=c++17
#cgo CXXFLAGS: -I${SRCDIR}/../../../vendor/link/extensions/abl_link/include
#cgo CXXFLAGS: -I${SRCDIR}/../../../vendor/link/extensions/abl_link/src
#cgo CXXFLAGS: -I${SRCDIR}/../../../vendor/link/include
#cgo CXXFLAGS: -I${SRCDIR}/../../../vendor/link/modules/asio-standalone/asio/include
#cgo CFLAGS: -I${SRCDIR}/../../../vendor/link/extensions/abl_link/include
#cgo darwin CXXFLAGS: -DLINK_PLATFORM_MACOSX=1 -DLINK_PLATFORM_UNIX=1
#cgo linux CXXFLAGS: -DLINK_PLATFORM_LINUX=1 -DLINK_PLATFORM_UNIX=1
#cgo windows CXXFLAGS: -DLINK_PLATFORM_WINDOWS=1
#cgo linux LDFLAGS: -latomic
#cgo windows LDFLAGS: -liphlpapi -lws2_32 -lwinmm

#include <stdlib.h>
#include <stdint.h>
#include "abl_link.h"

int64_t wail_mono_micros();
*/
import "C"

import (
	"unsafe"
)

// Link wraps a single abl_link instance (one LAN peer: sync + audio).
type Link struct {
	h C.struct_abl_link
}

// New constructs a new abl_link instance with the given initial tempo.
func New(bpm float64) *Link {
	return &Link{h: C.abl_link_create(C.double(bpm))}
}

// Close destroys the underlying abl_link instance.
func (l *Link) Close() {
	C.abl_link_destroy(l.h)
}

// Enable enables or disables Link network communication.
func (l *Link) Enable(enable bool) {
	C.abl_link_enable(l.h, C.bool(enable))
}

// IsEnabled reports whether Link is currently enabled.
func (l *Link) IsEnabled() bool {
	return bool(C.abl_link_is_enabled(l.h))
}

// NumPeers returns the number of peers currently connected in the Link session.
func (l *Link) NumPeers() uint64 {
	return uint64(C.abl_link_num_peers(l.h))
}

// MonoMicros returns the machine monotonic clock in microseconds — the domain
// shared with the CLAP plugins for stamp conversion (see wrap.cpp). Not the
// Link session clock.
func MonoMicros() int64 {
	return int64(C.wail_mono_micros())
}

// ClockMicros returns the current Link clock time in microseconds.
func (l *Link) ClockMicros() int64 {
	return int64(C.abl_link_clock_micros(l.h))
}

// SessionState is a captured snapshot of the Link timeline + transport state.
// Capture into it, read/mutate, and (for mutations) commit it back.
type SessionState struct {
	s C.abl_link_session_state
}

// NewSessionState allocates a reusable session-state snapshot.
func NewSessionState() *SessionState {
	return &SessionState{s: C.abl_link_create_session_state()}
}

// Close frees the session-state snapshot.
func (ss *SessionState) Close() {
	C.abl_link_destroy_session_state(ss.s)
}

// CaptureAppSessionState snapshots the current Link state from an application
// thread (thread-safe, not realtime-safe).
func (l *Link) CaptureAppSessionState(ss *SessionState) {
	C.abl_link_capture_app_session_state(l.h, ss.s)
}

// CommitAppSessionState commits mutations back to the Link session from an
// application thread.
func (l *Link) CommitAppSessionState(ss *SessionState) {
	C.abl_link_commit_app_session_state(l.h, ss.s)
}

// Tempo returns the timeline tempo in BPM.
func (ss *SessionState) Tempo() float64 {
	return float64(C.abl_link_tempo(ss.s))
}

// SetTempo sets the timeline tempo, taking effect at the given Link clock time.
func (ss *SessionState) SetTempo(bpm float64, atTime int64) {
	C.abl_link_set_tempo(ss.s, C.double(bpm), C.int64_t(atTime))
}

// BeatAtTime returns the beat value at the given time for the given quantum.
func (ss *SessionState) BeatAtTime(time int64, quantum float64) float64 {
	return float64(C.abl_link_beat_at_time(ss.s, C.int64_t(time), C.double(quantum)))
}

// PhaseAtTime returns the session phase (in [0, quantum)) at the given time.
func (ss *SessionState) PhaseAtTime(time int64, quantum float64) float64 {
	return float64(C.abl_link_phase_at_time(ss.s, C.int64_t(time), C.double(quantum)))
}

// TimeAtBeat returns the time at which the given beat occurs for the given quantum.
func (ss *SessionState) TimeAtBeat(beat, quantum float64) int64 {
	return int64(C.abl_link_time_at_beat(ss.s, C.double(beat), C.double(quantum)))
}

// RequestBeatAtTime maps the given beat to the given time in a socially-aware way
// (quantized launch when other peers are present).
func (ss *SessionState) RequestBeatAtTime(beat float64, time int64, quantum float64) {
	C.abl_link_request_beat_at_time(ss.s, C.double(beat), C.int64_t(time), C.double(quantum))
}

// ForceBeatAtTime rudely re-maps the beat/time relationship for all peers.
// WAIL is a passive peer (ADR-0003) and does not use this in normal operation;
// it is retained for parity with the previous binding.
func (ss *SessionState) ForceBeatAtTime(beat float64, time int64, quantum float64) {
	C.abl_link_force_beat_at_time(ss.s, C.double(beat), C.int64_t(time), C.double(quantum))
}

// --- Link Audio: non-realtime control + discovery surface ---
// Sink (emit) and Source (capture) live in sink.go / source.go.

// EnableLinkAudio enables or disables Link Audio on top of an enabled Link
// instance. Audio sharing is opt-in.
func (l *Link) EnableLinkAudio(enable bool) {
	C.abl_link_audio_enable_link_audio(l.h, C.bool(enable))
}

// IsLinkAudioEnabled reports whether Link Audio is enabled.
func (l *Link) IsLinkAudioEnabled() bool {
	return bool(C.abl_link_audio_is_link_audio_enabled(l.h))
}

// SetPeerName sets the local peer name used to identify WAIL in the session.
func (l *Link) SetPeerName(name string) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	C.abl_link_audio_set_peer_name(l.h, cName)
}

// PeerName returns the local peer name.
func (l *Link) PeerName() string {
	// Query required size, then fill.
	n := C.abl_link_audio_peer_name(l.h, nil, 0)
	if n == 0 {
		return ""
	}
	buf := make([]byte, int(n)+1)
	C.abl_link_audio_peer_name(l.h, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)))
	return C.GoString((*C.char)(unsafe.Pointer(&buf[0])))
}

// ChannelID / PeerID / SessionID are 8-byte identifiers, persistent for the
// lifetime of a channel/peer/session.
type ChannelID [8]byte

// PeerID identifies the publishing Link peer of a channel.
type PeerID [8]byte

// Channel describes a discovered Link Audio channel. IDs are stable; names are
// mutable display strings.
type Channel struct {
	ID       ChannelID
	Name     string
	PeerID   PeerID
	PeerName string
}

// Channels returns the currently-available Link Audio channels.
func (l *Link) Channels() []Channel {
	list := C.abl_link_audio_get_channels(l.h)
	defer C.abl_link_audio_free_channel_list(list)
	n := int(list.count)
	if n == 0 {
		return nil
	}
	cChans := unsafe.Slice(list.channels, n)
	out := make([]Channel, n)
	for i := 0; i < n; i++ {
		c := cChans[i]
		out[i] = Channel{
			ID:       channelID(c.id),
			Name:     C.GoString(c.name),
			PeerID:   peerID(c.peer_id),
			PeerName: C.GoString(c.peer_name),
		}
	}
	return out
}

func channelID(id C.struct_abl_link_audio_channel_id) ChannelID {
	var out ChannelID
	for i := 0; i < 8; i++ {
		out[i] = byte(id.bytes[i])
	}
	return out
}

func peerID(id C.struct_abl_link_audio_peer_id) PeerID {
	var out PeerID
	for i := 0; i < 8; i++ {
		out[i] = byte(id.bytes[i])
	}
	return out
}
