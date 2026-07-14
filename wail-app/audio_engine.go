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
	HandleRemoteAudio(fromIdentity, displayName string, waif []byte)
	// SetRoomAnchor applies a fresh relay interval_anchor: aligns the local→room
	// interval labeler and adopts the room tempo/config.
	SetRoomAnchor(currentIndex int64, bpm float64, bars uint32, quantum float64)
	// CaptureChannels lists discovered local Link Audio channels for the
	// send-mixer UI, marking which are currently bridged.
	CaptureChannels() []CaptureChannelInfo
	// SetCaptureEnabled toggles whether a discovered channel is bridged.
	SetCaptureEnabled(channelID string, on bool)
}

// CaptureChannelInfo describes a discovered local Link Audio channel for the UI.
type CaptureChannelInfo struct {
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	PeerName  string `json:"peer_name"`
	Enabled   bool   `json:"enabled"`
}
