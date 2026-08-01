package main

import "time"

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
	// The session re-arms grid alignment on it (the slew cannot walk back a
	// jump of whole beats) and reports it to the room. Jumps WAIL caused
	// itself are attributed and withheld rather than reported.
	TakeGridJump() (GridJump, bool)
	// SetRoomAnchor applies a fresh relay interval_anchor: aligns the local→room
	// interval labeler and adopts the room tempo/config.
	SetRoomAnchor(currentIndex int64, bpm float64, bars uint32, quantum float64)
	// AlignRoomLabel aligns the local→room labeler to an explicitly derived
	// local index (ADR-0006 "known by construction": the session computed
	// localIndex from the anchor's boundary time on an aligned grid). Exact
	// regardless of when in the interval it runs, unlike SetRoomAnchor's
	// sample align; overrides it whenever grid alignment is active.
	AlignRoomLabel(roomIndex, localIndex int64)
	// OnGridSnap re-anchors emit feeders after an entry-conformance grid
	// snap: the snap moved the playhead, not the audio — the jumped frames
	// skip silently, never counting as underruns.
	OnGridSnap(deltaUs int64)
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
	// SetMetronome publishes (or tears down) a locally-generated room metronome
	// Link Audio channel — a click on every beat (accented on bar downbeats) on
	// the local Link grid, for aligning against the DAW's own metronome.
	SetMetronome(enabled bool)
	// SetIntervalOffset live-adjusts the receive playout offset D for all
	// streams (the NINJAM latency/reliability knob); returns the effective D.
	SetIntervalOffset(d int) int
	// SetCushionMs live-adjusts the emit feed-ahead depth (ms) for all streams
	// and the metronome; returns the effective clamped value.
	SetCushionMs(ms int) int
	// LabelOffsetFor returns the worst interval-label verdict across one
	// identity's streams (ADR-0006 follow-up): 0 = the peer's labels agree
	// with our room index, k = their audio silently plays k intervals off.
	LabelOffsetFor(identity string) (int64, bool)
	// Health snapshots the engine's cumulative diagnostic counters. Each
	// increment marks an event that risks an audible artifact; the session
	// diffs snapshots to surface them in the log panel and Network tab.
	Health() EngineHealth
}

// GridJump is a detected discontinuity in the local Link beat timeline,
// with what the engine could attribute it to. The cause is the point: a jump
// is only actionable if you can tell a peer joining from a tempo change from
// something WAIL did to itself.
type GridJump struct {
	Beats      float64 // signed jump, in beats
	Ms         float64 // the same jump in milliseconds at the room tempo
	Intervals  int64   // ≈ how many interval boundaries that is
	Peers      uint64  // LAN Link peer count when it happened
	Cause      string  // short attribution, for the headline
	Detail     string  // the evidence behind it
	SelfCaused bool    // WAIL moved its own grid (an entry snap) — not a fault
}

// gridSnapAttributionWindow is how recently our own snap must have run for a
// detected jump to be credited to it. Comfortably longer than the poll
// interval, far shorter than the gap between real merges.
const gridSnapAttributionWindow = 2 * time.Second

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
