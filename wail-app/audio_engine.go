package main

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
	// SetRoomAnchor applies a fresh relay interval_anchor: aligns the local→room
	// interval labeler and adopts the room tempo/config.
	SetRoomAnchor(currentIndex int64, bpm float64, bars uint32, quantum float64)
	// RoomIndex maps a local interval index to the shared room index via the
	// labeler aligned by SetRoomAnchor; ok is false until an anchor has arrived.
	// The session uses it for boundary logging and to tag in-app-sender frames,
	// so there is a single source of truth for the local→room mapping.
	RoomIndex(localIndex int64) (roomIndex int64, ok bool)
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
	// SetMetronome publishes (or tears down) a locally-generated "WAIL Metronome"
	// Link Audio channel — a click on every beat (accented on bar downbeats) on
	// the local Link grid, for aligning against the DAW's own metronome.
	SetMetronome(enabled bool)
	// SetCushionMs live-adjusts the emit feed-ahead depth (ms) for all streams
	// and the metronome; returns the effective clamped value.
	SetCushionMs(ms int) int
	// Health snapshots the engine's cumulative diagnostic counters. Each
	// increment marks an event that risks an audible artifact; the session
	// diffs snapshots to surface them in the log panel and Network tab.
	Health() EngineHealth
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
	EmitFramesConcealed     uint64 `json:"emit_frames_concealed"`       // missing frames masked by Opus PLC
	EmitSinkWriteRejected   uint64 `json:"emit_sink_write_rejected"`    // sink refused a chunk mid-stream (queue full / listener left) — hole in delivered audio
	WireDecodeFailures      uint64 `json:"wire_decode_failures"`        // WAIF wire-decode errors
	OpusDecodeFailures      uint64 `json:"opus_decode_failures"`        // Opus decode errors
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
