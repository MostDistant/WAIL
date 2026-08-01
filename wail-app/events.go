package main

// Events emitted to the frontend via Wails event system.

type SessionStarted struct {
	PeerID string  `json:"peer_id"`
	Room   string  `json:"room"`
	BPM    float64 `json:"bpm"`
}

type SessionEnded struct{}

type SessionError struct {
	Message string `json:"message"`
}

type PeerJoinedEvent struct {
	PeerID      string  `json:"peer_id"`
	DisplayName *string `json:"display_name,omitempty"`
}

type PeerLeftEvent struct {
	PeerID string `json:"peer_id"`
}

type TempoChangedEvent struct {
	BPM    float64 `json:"bpm"`
	Source string  `json:"source"` // "local" or "remote"
}

type PeerInfo struct {
	PeerID      string   `json:"peer_id"`
	DisplayName *string  `json:"display_name,omitempty"`
	RTTMs       *float64 `json:"rtt_ms,omitempty"`
	Slot        *uint32  `json:"slot,omitempty"`
	Status      string   `json:"status"`
	IsSending   bool     `json:"is_sending"`
	IsReceiving bool     `json:"is_receiving"`
}

type LocalSendInfo struct {
	StreamIndex uint16  `json:"stream_index"`
	IsSending   bool    `json:"is_sending"`
	StreamName  *string `json:"stream_name,omitempty"`
	IsTestTone  bool    `json:"is_test_tone"`
}

type SlotInfo struct {
	Slot         uint32   `json:"slot"`
	ShortID      string   `json:"short_id"`
	ClientID     string   `json:"client_id"`
	PeerID       string   `json:"peer_id"` // relay peer id; groups a slot's channel under its peer (empty if disconnected)
	ChannelIndex uint16   `json:"channel_index"`
	DisplayName  *string  `json:"display_name,omitempty"`
	Status       *string  `json:"status,omitempty"`
	RTTMs        *float64 `json:"rtt_ms,omitempty"`
	IsSending    bool     `json:"is_sending"`
	IsReceiving  bool     `json:"is_receiving"`
	StreamName   *string  `json:"stream_name,omitempty"`
}

// IntervalPromptEvent asks a joining user to match their DAW's launch
// quantization to the room's interval (ADR-0004: communicated, never enforced).
type IntervalPromptEvent struct {
	Bars    uint32  `json:"bars"`
	Quantum float64 `json:"quantum"`
	BPM     float64 `json:"bpm"`
}

type StatusUpdate struct {
	BPM                float64         `json:"bpm"`
	Beat               float64         `json:"beat"`
	Phase              float64         `json:"phase"`
	LinkPeers          uint64          `json:"link_peers"`
	Peers              []PeerInfo      `json:"peers"`
	Slots              []SlotInfo      `json:"slots"`
	LocalSends         []LocalSendInfo `json:"local_sends"`
	IntervalBars       uint32          `json:"interval_bars"`
	IntervalQuantum    float64         `json:"interval_quantum"`
	AudioSent          uint64          `json:"audio_sent"`
	AudioRecv          uint64          `json:"audio_recv"`
	AudioBytesSent     uint64          `json:"audio_bytes_sent"`
	AudioBytesRecv     uint64          `json:"audio_bytes_recv"`
	AudioDCOpen        bool            `json:"audio_dc_open"`
	PluginConnected    bool            `json:"plugin_connected"`
	Recording          bool            `json:"recording"`
	RecordingSizeBytes uint64          `json:"recording_size_bytes"`
	TestToneStream     *uint16         `json:"test_tone_stream,omitempty"`
	// Discovered local Link Audio channels for the capture send-mixer (Step 4).
	CaptureChannels []CaptureChannelInfo `json:"capture_channels,omitempty"`
	RelayRTTMs      *float64             `json:"relay_rtt_ms,omitempty"`
	// StreamOffsets is the debug-room readout: per-stream rhythmic phase
	// offset vs the room grid (internal/offset), empty until enough rhythmic
	// content has arrived to judge.
	StreamOffsets []StreamOffset `json:"stream_offsets,omitempty"`
}

// StreamOffset is one stream's measured rhythmic phase offset vs the room
// grid in milliseconds (debug-room analysis; positive = content late).
type StreamOffset struct {
	Name string  `json:"name"`
	Ms   float64 `json:"ms"`
}

type PeerNetworkInfo struct {
	PeerID          string   `json:"peer_id"`
	DisplayName     *string  `json:"display_name,omitempty"`
	Slot            *uint32  `json:"slot,omitempty"`
	ConnectionState string   `json:"connection_state"`
	RTTMs           *float64 `json:"rtt_ms,omitempty"`
	AudioRecv       uint64   `json:"audio_recv"`
	FramesReceived  uint64   `json:"frames_received"`
	PacketsLost     uint64   `json:"packets_lost"`
	LossEvents      uint64   `json:"loss_events"`
}

type PeersNetwork struct {
	Peers  []PeerNetworkInfo `json:"peers"`
	Health EngineHealth      `json:"health"`
}

type LogEntry struct {
	Level    string  `json:"level"`
	Message  string  `json:"message"`
	PeerID   *string `json:"peer_id,omitempty"`
	PeerName *string `json:"peer_name,omitempty"`
}

type ChatMessageEvent struct {
	SenderName string `json:"sender_name"`
	IsOwn      bool   `json:"is_own"`
	Text       string `json:"text"`
}

type SessionStale struct {
	Attempts uint32 `json:"attempts"`
}
