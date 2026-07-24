package main

import (
	"bytes"
	"encoding/json"
	"log"
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
// next_boundary_micros is the server-clock time the current interval ends
// (ADR-0006): with a relay RTT estimate clients map it into their local clock
// domain and measure their grid alignment error δ against the room grid.
type anchorMsg struct {
	Type               string  `json:"type"`
	CurrentIndex       int64   `json:"current_index"`
	BPM                float64 `json:"bpm"`
	Bars               uint32  `json:"bars"`
	Quantum            float64 `json:"quantum"`
	ServerNowMicros    int64   `json:"server_now_micros"`
	NextBoundaryMicros int64   `json:"next_boundary_micros"`
}

// observeSync inspects a relayed sync payload; when it carries tempo or interval
// config, it updates the room clock and returns the anchor to broadcast. The
// returned bool is false for payloads that don't affect the clock. roomName is
// for the fly.io-visible clock logs.
func (r *room) observeSync(roomName string, payload json.RawMessage) (anchorMsg, bool) {
	// Cheap pre-filter: the vast majority of relayed sync traffic is Ping/Pong/
	// StateSnapshot, which never re-anchors. Skip the unmarshal unless the payload
	// could be one of the two anchor-bearing types.
	if !bytes.Contains(payload, []byte(`"TempoChange"`)) && !bytes.Contains(payload, []byte(`"IntervalConfig"`)) {
		return anchorMsg{}, false
	}
	var p syncPayloadPeek
	if err := json.Unmarshal(payload, &p); err != nil {
		return anchorMsg{}, false
	}
	if p.Type != "TempoChange" && p.Type != "IntervalConfig" {
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

	// Values unchanged: no re-anchor, no broadcast. A redundant anchor only
	// re-rolls every client's labeler alignment, and each re-roll can shift a
	// peer's room labels by a whole interval — peers then disagree with each
	// other until the next anchor. (Joins re-broadcast IntervalConfig per
	// ADR-0004; without this guard every join flooded the room with re-rolls.)
	if r.haveClock && bpm == r.tempoBPM && bars == r.cfg.Bars && quantum == r.cfg.Quantum {
		return anchorMsg{}, false
	}
	r.tempoBPM, r.cfg.Bars, r.cfg.Quantum = bpm, bars, quantum

	now := serverNowUs()
	if !r.haveClock {
		r.clk = newRoomClock(roomAnchor{Index: 0, AtMicros: now, TempoBPM: r.tempoBPM, Config: r.cfg})
		r.haveClock = true
		log.Printf("[roomclock] room %s clock created: tempo=%.1f cfg=%dx%.0f", roomName, r.tempoBPM, r.cfg.Bars, r.cfg.Quantum)
	} else {
		oldTempo := r.clk.anchor().TempoBPM
		r.clk.reanchor(now, r.tempoBPM, r.cfg)
		log.Printf("[roomclock] room %s re-anchor: tempo %.1f→%.1f cfg=%dx%.0f", roomName, oldTempo, r.tempoBPM, r.cfg.Bars, r.cfg.Quantum)
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
	idx := r.clk.indexAt(now)
	return anchorMsg{
		Type:               "interval_anchor",
		CurrentIndex:       idx,
		BPM:                r.tempoBPM,
		Bars:               r.cfg.Bars,
		Quantum:            r.cfg.Quantum,
		ServerNowMicros:    now,
		NextBoundaryMicros: r.clk.boundaryMicros(idx + 1),
	}
}

// serverPongPayload builds the relay's direct answer to a broadcast Ping
// (ADR-0006 relay time service): clients estimate relay RTT from the echoed
// ping timestamp and the server↔local clock offset from server_now_micros.
// ok is false for non-Ping payloads, which get no direct reply.
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
