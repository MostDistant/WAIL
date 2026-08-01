package main

import (
	"strings"
	"time"
)

// loopbackIdentitySuffix marks the server-echo monitor stream: our own frames
// round-tripped through the relay and republished under a distinct identity.
// It is not a room peer, so diagnostics phrased about "a peer" have to say
// something else about it.
const loopbackIdentitySuffix = ":loopback"

// isLoopbackIdentity reports whether an emit identity is our own server echo.
func isLoopbackIdentity(identity string) bool {
	return strings.HasSuffix(identity, loopbackIdentitySuffix)
}

// AudioEngine is WAIL's Link Audio audio path (ADR-0001/0002): capture subscribes
// to local Link Audio channels and ships them as WAIF over the relay; playback
// republishes remote streams as Link Audio channels one interval late. It
// replaces the plugin + TCP-IPC path.
//
// The concrete implementation lives in audio_engine_real.go (built without the
// linkstub tag, since it needs the cgo Link Audio binding). A no-op stub in
// audio_engine_stub.go keeps the app building under -tags linkstub for logic
// tests. session.go talks only to this interface.
type AudioEngine interface {
	// Start enables Link Audio and begins capture discovery + the emit loop.
	Start() error
	// Stop tears down sources, sinks, and goroutines.
	Stop()
	// HandleRemoteAudio feeds one WAIF frame received from a remote peer into the
	// playback path, keyed on the sender's persistent identity and stream.
	// streamName is the sender's display name for the stream (may be empty until
	// StreamNames sync arrives); it labels the republished channel.
	HandleRemoteAudio(fromIdentity, displayName, streamName string, waif []byte)
	// SetPeerStreams records the set of stream ids an identity says it is still
	// sending (their StreamNames sync). Published streams of theirs outside the
	// set are retired once drained and idle, so a channel the sender stopped
	// publishing stops holding a port on every WAIL Receive on the LAN. An
	// empty set means "sending nothing", not "unknown".
	SetPeerStreams(identity string, keep map[uint16]bool)
	// ClearPeerIntent forgets what an identity last told us, so nothing of
	// theirs is retirable again until it does. Needed wherever a DropPeer can
	// be undone by something other than a fresh StreamNames.
	ClearPeerIntent(identity string)
	// DropPeer marks everything an identity publishes for retirement on a
	// longer grace — they left the room (or, for the loopback identity, the
	// server echo was turned off). The grace is what lets affinity hold a
	// channel across a reconnect blip.
	DropPeer(identity string)
	// TakeGridJump reports (and clears) a local Link grid jump detected since
	// the last call — something moved the beat timeline out from under us.
	// Observability only (ADR-0009): playout re-quantizes onto the local grid
	// wherever it sits, so nothing corrects a jump — but a musician whose bar
	// lines just moved deserves the attribution, and so does the relay log.
	TakeGridJump() (GridJump, bool)
	// SetRoomConfig adopts the room's tempo and interval shape for the
	// engine's interval math. The anchor's one remaining job (ADR-0009).
	SetRoomConfig(bpm float64, bars uint32, quantum float64)
	// CaptureChannels lists discovered local Link Audio channels for the
	// send-mixer UI, marking which are currently bridged.
	CaptureChannels() []CaptureChannelInfo
	// SetCaptureEnabled toggles whether a discovered channel is bridged.
	SetCaptureEnabled(channelID string, on bool)
	// SetCaptureRestore replaces the remembered set of enabled capture
	// channels (keyed by peer/channel name, loaded at session start);
	// matching discovered channels auto-enable.
	SetCaptureRestore(keys []CaptureChannelKey)
	// SetCaptureDump toggles a debug dump: while on, each enabled capture channel
	// writes two WAV files — the PCM fed to Opus and that audio decoded as a
	// receiver would — for diagnosing where transmitted audio degrades.
	SetCaptureDump(enabled bool)
	// SetMetronome publishes (or tears down) a locally-generated room metronome
	// Link Audio channel — a click on every beat (accented on bar downbeats) on
	// the local Link grid, for aligning against the DAW's own metronome.
	SetMetronome(enabled bool)
	// SetIntervalOffset is retired (ADR-0009): playout is adaptive per sender,
	// so there is no D. Kept so the Debug control degrades gracefully.
	SetIntervalOffset(d int) int
	// SetCushionMs live-adjusts the emit feed-ahead depth (ms) for all streams
	// and the metronome; returns the effective clamped value.
	SetCushionMs(ms int) int
	// Health snapshots the engine's cumulative diagnostic counters. Each
	// increment marks an event that risks an audible artifact; the session
	// diffs snapshots to surface them in the log panel and Network tab.
	Health() EngineHealth
}

// GridJump is a detected discontinuity in the local Link beat timeline,
// with what the engine could attribute it to (observability only, ADR-0009).
type GridJump struct {
	Beats     float64 // signed jump, in beats
	Ms        float64 // the same jump in milliseconds at the room tempo
	Intervals int64   // ≈ how many interval boundaries that is
	Peers     uint64  // LAN Link peer count when it happened
	Cause     string  // short attribution, for the headline
	Detail    string  // the evidence behind it
}

const (
	// jumpDetectMaxGapSec bounds the tick spacing the jump detector will judge.
	// Beyond it the loop stalled (GC, a starved scheduler on a loaded machine)
	// and the expected-beat arithmetic is guesswork — a false jump would put a
	// phantom merge in the room log.
	jumpDetectMaxGapSec = 1.0
	// gridJumpEvidenceWindow is how far back peer/tempo movement still counts
	// as the explanation. Link's peer discovery and the timeline merge it
	// triggers are tens of milliseconds apart, so same-tick evidence misses it.
	gridJumpEvidenceWindow = 3 * time.Second
)

// jumpEvidence is when the attributable inputs last moved, carried across
// ticks by the emit loop so a jump can be explained by something that happened
// slightly before it.
type jumpEvidence struct {
	peersChangedAt time.Time
	peersFrom      uint64
	tempoChangedAt time.Time
	tempoFrom      float64
}

// EngineHealth is a snapshot of cumulative audio-path diagnostics.
type EngineHealth struct {
	CaptureRingDropped      uint64 `json:"capture_ring_dropped"`        // RT ring overwrote buffers (drainer stalled)
	CaptureLANLostBuffers   uint64 `json:"capture_lan_lost_buffers"`    // Link Audio buffers lost on the capture hop
	CaptureLANGapEvents     uint64 `json:"capture_lan_gap_events"`      // distinct capture-hop loss events
	CaptureResnaps          uint64 `json:"capture_resnaps"`             // assembler re-anchors (stamp discontinuity)
	CaptureSlews            uint64 `json:"capture_slews"`               // frames micro-slewed tracking clock drift (inaudible)
	CaptureDroppedLate      uint64 `json:"capture_dropped_late"`        // buffers for already-emitted intervals
	CaptureDroppedBackfill  uint64 `json:"capture_dropped_backfill"`    // buffers behind the emitted-window boundary
	EmitIntervalsIncomplete uint64 `json:"emit_intervals_incomplete"`   // released before the streaming tail arrived (expected; benign)
	EmitSinkUnderrunEvents  uint64 `json:"emit_sink_underrun_events"`   // paced feed fell behind the playhead past the cushion (audible)
	EmitSinkUnderrunFrames  uint64 `json:"emit_sink_underrun_frames"`   // frames skipped (played as silence) due to underrun
	EmitFramesMissingAtPlay uint64 `json:"emit_frames_missing_at_play"` // frames still absent when their interval retired (played as silence)
	EmitFramesTooLate       uint64 `json:"emit_frames_too_late"`        // frames dropped: sender labels already behind our playout (anchor offset mismatch)
	EmitFramesConcealed     uint64 `json:"emit_frames_concealed"`       // missing frames masked by Opus PLC
	EmitSinkWriteRejected   uint64 `json:"emit_sink_write_rejected"`    // sink refused a chunk mid-stream (queue full / listener left) — hole in delivered audio
	WireDecodeFailures      uint64 `json:"wire_decode_failures"`        // WAIF wire-decode errors
	OpusDecodeFailures      uint64 `json:"opus_decode_failures"`        // Opus decode errors
	// StreamOffsets is the debug-room per-stream phase-offset readout vs the
	// room grid (internal/offset), computed on demand by Health.
	StreamOffsets []StreamOffset `json:"stream_offsets,omitempty"`
	// EmitCushionMs is the current effective emit feed-ahead (config, not a
	// counter): it rides the snapshot so the UI can show/initialise the cushion
	// control. Not a HEALTH_FIELDS row.
	EmitCushionMs int `json:"emit_cushion_ms"`
}

// CaptureChannelInfo describes a discovered local Link Audio channel for the
// UI and for stream naming (StreamID labels the WAIF stream a bridged channel
// sends as; receivers republish it under the channel's name).
type CaptureChannelInfo struct {
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	PeerName  string `json:"peer_name"`
	Enabled   bool   `json:"enabled"`
	StreamID  uint16 `json:"stream_id"`
}
