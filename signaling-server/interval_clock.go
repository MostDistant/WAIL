package main

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
)

// serverEpoch anchors the server's monotonic microsecond clock. The room clock
// and the anchors it broadcasts are all in this domain; clients map it to their
// local clock with interval-scale slack (ADR-0003), so millisecond one-way skew
// is immaterial.
var serverEpoch = time.Now()

func serverNowUs() int64 { return time.Since(serverEpoch).Microseconds() }

// syncPayloadPeek reads only the fields the relay needs to own the interval
// clock. Everything else in the sync payload stays opaque and is relayed as-is.
type syncPayloadPeek struct {
	Type    string  `json:"type"`
	BPM     float64 `json:"bpm"`
	Bars    uint32  `json:"bars"`
	Quantum float64 `json:"quantum"`
}

// anchorMsg is the interval_anchor the relay broadcasts so every client shares
// the room interval index. current_index is the server's authoritative index
// right now; bpm/bars/quantum let clients advance their own local boundaries.
type anchorMsg struct {
	Type            string  `json:"type"`
	CurrentIndex    int64   `json:"current_index"`
	BPM             float64 `json:"bpm"`
	Bars            uint32  `json:"bars"`
	Quantum         float64 `json:"quantum"`
	ServerNowMicros int64   `json:"server_now_micros"`
}

// observeSync inspects a relayed sync payload; when it carries tempo or interval
// config, it updates the room clock and returns the anchor to broadcast. The
// returned bool is false for payloads that don't affect the clock.
func (r *room) observeSync(payload json.RawMessage) (anchorMsg, bool) {
	var p syncPayloadPeek
	if err := json.Unmarshal(payload, &p); err != nil {
		return anchorMsg{}, false
	}
	if p.Type != "TempoChange" && p.Type != "IntervalConfig" {
		return anchorMsg{}, false
	}

	r.clockMu.Lock()
	defer r.clockMu.Unlock()

	if p.BPM > 0 {
		r.tempoBPM = p.BPM
	}
	if p.Bars > 0 {
		r.cfg.Bars = p.Bars
	}
	if p.Quantum > 0 {
		r.cfg.Quantum = p.Quantum
	}
	// Sensible defaults until both tempo and config are known.
	if r.tempoBPM == 0 {
		r.tempoBPM = 120
	}
	if r.cfg.Bars == 0 {
		r.cfg.Bars = 4
	}
	if r.cfg.Quantum == 0 {
		r.cfg.Quantum = 4
	}

	now := serverNowUs()
	if !r.haveClock {
		r.clk = newRoomClock(roomAnchor{Index: 0, AtMicros: now, TempoBPM: r.tempoBPM, Config: r.cfg})
		r.haveClock = true
	} else {
		r.clk.reanchor(now, r.tempoBPM, r.cfg)
	}
	return r.anchorMsgLocked(now), true
}

// currentAnchor returns the room's current anchor for a late joiner. false if no
// tempo/config has been observed yet.
func (r *room) currentAnchor() (anchorMsg, bool) {
	r.clockMu.Lock()
	defer r.clockMu.Unlock()
	if !r.haveClock {
		return anchorMsg{}, false
	}
	return r.anchorMsgLocked(serverNowUs()), true
}

// anchorMsgLocked builds an anchor message. Caller holds clockMu.
func (r *room) anchorMsgLocked(now int64) anchorMsg {
	return anchorMsg{
		Type:            "interval_anchor",
		CurrentIndex:    r.clk.indexAt(now),
		BPM:             r.tempoBPM,
		Bars:            r.cfg.Bars,
		Quantum:         r.cfg.Quantum,
		ServerNowMicros: now,
	}
}

// broadcastAnchor sends the interval_anchor to every peer in the room, including
// the tempo setter, so all peers adopt the one authoritative room index.
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
