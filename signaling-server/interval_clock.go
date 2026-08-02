package main

import (
	"bytes"
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
)

// serverEpoch anchors the server's monotonic microsecond clock, which stamps
// pongs for the clients' relay-RTT estimate (the anchor rides it too, for wire
// shape stability with older clients).
var serverEpoch = time.Now()

func serverNowUs() int64 { return time.Since(serverEpoch).Microseconds() }

// intervalConfig is a room's interval shape: Bars × Quantum beats.
type intervalConfig struct {
	Bars    uint32
	Quantum float64
}

// syncPayloadPeek reads only the fields the relay needs to track room state.
// Everything else in the sync payload stays opaque and is relayed as-is.
type syncPayloadPeek struct {
	Type    string  `json:"type"`
	BPM     float64 `json:"bpm"`
	Bars    uint32  `json:"bars"`
	Quantum float64 `json:"quantum"`
}

// anchorMsg carries the room's tempo and interval shape (ADR-0009). This is
// NINJAM's ConfigChangeNotify, not a clock: the relay-authoritative interval
// index it used to carry retired with the shared round numbering — rounds are
// sender-relative, so there is nothing for a room clock to arbitrate. The
// message keeps its name and a zero current_index for wire-shape stability.
type anchorMsg struct {
	Type            string  `json:"type"`
	CurrentIndex    int64   `json:"current_index"`
	BPM             float64 `json:"bpm"`
	Bars            uint32  `json:"bars"`
	Quantum         float64 `json:"quantum"`
	ServerNowMicros int64   `json:"server_now_micros"`
}

// observeSync inspects a relayed sync payload; when it carries tempo or
// interval config, it updates the room state and returns the config message to
// broadcast. The returned bool is false for payloads that don't affect it.
func (r *room) observeSync(roomName string, payload json.RawMessage) (anchorMsg, bool) {
	// Cheap pre-filter: the vast majority of relayed sync traffic is Ping/Pong/
	// StateSnapshot, which never changes room config. Skip the unmarshal unless
	// the payload could be one of the config-bearing types. TempoDeclare is
	// watched alongside the legacy TempoChange so clients can eventually stop
	// dual-sending.
	if !bytes.Contains(payload, []byte(`"TempoChange"`)) &&
		!bytes.Contains(payload, []byte(`"TempoDeclare"`)) &&
		!bytes.Contains(payload, []byte(`"IntervalConfig"`)) {
		return anchorMsg{}, false
	}
	var p syncPayloadPeek
	if err := json.Unmarshal(payload, &p); err != nil {
		return anchorMsg{}, false
	}
	if p.Type != "TempoChange" && p.Type != "TempoDeclare" && p.Type != "IntervalConfig" {
		return anchorMsg{}, false
	}

	r.clockMu.Lock()
	defer r.clockMu.Unlock()

	bpm, bars, quantum := r.tempoBPM, r.cfg.Bars, r.cfg.Quantum
	if p.BPM > 0 {
		bpm = p.BPM
	}
	if p.Bars > 0 {
		bars = p.Bars
	}
	if p.Quantum > 0 {
		quantum = p.Quantum
	}
	// Sensible defaults until both tempo and config are known.
	if bpm == 0 {
		bpm = 120
	}
	if bars == 0 {
		bars = 4
	}
	if quantum == 0 {
		quantum = 4
	}

	// Values unchanged: no broadcast. Joins re-broadcast IntervalConfig per
	// ADR-0004, and echoing every one back at the room is pure noise.
	if r.haveConfig && bpm == r.tempoBPM && bars == r.cfg.Bars && quantum == r.cfg.Quantum {
		return anchorMsg{}, false
	}
	if !r.haveConfig {
		log.Printf("[roomcfg] room %s config set: tempo=%.1f cfg=%dx%.0f", roomName, bpm, bars, quantum)
	} else {
		log.Printf("[roomcfg] room %s config change: tempo %.1f→%.1f cfg=%dx%.0f", roomName, r.tempoBPM, bpm, bars, quantum)
	}
	r.tempoBPM, r.cfg.Bars, r.cfg.Quantum = bpm, bars, quantum
	r.haveConfig = true
	return r.anchorMsgLocked(), true
}

// currentAnchor returns the room's config message for a late joiner. false if
// no tempo/config has been observed yet.
func (r *room) currentAnchor() (anchorMsg, bool) {
	r.clockMu.Lock()
	defer r.clockMu.Unlock()
	if !r.haveConfig {
		return anchorMsg{}, false
	}
	return r.anchorMsgLocked(), true
}

// anchorMsgLocked builds the room-config message. Caller holds clockMu.
func (r *room) anchorMsgLocked() anchorMsg {
	return anchorMsg{
		Type:            "interval_anchor",
		BPM:             r.tempoBPM,
		Bars:            r.cfg.Bars,
		Quantum:         r.cfg.Quantum,
		ServerNowMicros: serverNowUs(),
	}
}

// serverPongPayload builds the relay's direct answer to a broadcast Ping:
// clients estimate relay RTT from the echoed ping timestamp (the server clock
// stamp remains for diagnostics). ok is false for non-Ping payloads.
func serverPongPayload(payload json.RawMessage) (json.RawMessage, bool) {
	if !bytes.Contains(payload, []byte(`"Ping"`)) {
		return nil, false
	}
	var p struct {
		Type     string `json:"type"`
		ID       uint64 `json:"id"`
		SentAtUs int64  `json:"sent_at_us"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.Type != "Ping" {
		return nil, false
	}
	now := serverNowUs()
	pong, err := json.Marshal(map[string]any{
		"type":              "Pong",
		"id":                p.ID,
		"ping_sent_at_us":   p.SentAtUs,
		"pong_sent_at_us":   now,
		"server_now_micros": now,
	})
	if err != nil {
		return nil, false
	}
	return pong, true
}

// broadcastAnchor sends the room-config message to every peer in the room,
// including the peer whose change triggered it.
func (r *room) broadcastAnchor(am anchorMsg) {
	raw, err := json.Marshal(am)
	if err != nil {
		return
	}
	wsMsg := wsMessage{websocket.TextMessage, raw}
	for _, e := range r.loadConns() {
		e.c.sendWS(wsMsg)
	}
}
