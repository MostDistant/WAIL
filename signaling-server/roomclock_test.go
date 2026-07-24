package main

import (
	"encoding/json"
	"testing"
)

func TestRoomClockIndexAndBoundary(t *testing.T) {
	rc := newRoomClock(roomAnchor{Index: 100, AtMicros: 0, TempoBPM: 120, Config: intervalConfig{Bars: 4, Quantum: 4}})
	// 16 beats at 120 BPM = 8s per interval.
	if got := rc.indexAt(0); got != 100 {
		t.Fatalf("indexAt(0) = %d, want 100", got)
	}
	if got := rc.indexAt(7_999_999); got != 100 {
		t.Fatalf("indexAt(just before 8s) = %d, want 100", got)
	}
	if got := rc.indexAt(8_000_000); got != 101 {
		t.Fatalf("indexAt(8s) = %d, want 101", got)
	}
	// Boundary/index are inverse.
	for idx := int64(98); idx <= 104; idx++ {
		if got := rc.indexAt(rc.boundaryMicros(idx)); got != idx {
			t.Errorf("boundary(%d) maps back to %d", idx, got)
		}
	}
}

func TestRoomClockReanchorQuantizes(t *testing.T) {
	rc := newRoomClock(roomAnchor{Index: 0, AtMicros: 0, TempoBPM: 120, Config: intervalConfig{Bars: 4, Quantum: 4}})
	// Mid interval 2 (16s..24s); change tempo. Applies at interval 3 (24s).
	rc.reanchor(20_000_000, 240, intervalConfig{Bars: 4, Quantum: 4})
	if got := rc.anchor().AtMicros; got != 24_000_000 {
		t.Fatalf("reanchor AtMicros = %d, want 24s", got)
	}
	if got := rc.indexAt(23_999_999); got != 2 {
		t.Fatalf("index just before 24s = %d, want 2", got)
	}
	// 240 BPM → 4s intervals; interval 4 at 28s.
	if got := rc.indexAt(28_000_000); got != 4 {
		t.Fatalf("index at 28s = %d, want 4", got)
	}
}

func TestRoomClockClampsBadInput(t *testing.T) {
	rc := newRoomClock(roomAnchor{Index: 0, AtMicros: 0, TempoBPM: 0, Config: intervalConfig{Bars: 0, Quantum: 0}})
	// Must not panic / produce NaN with zero tempo and zero config.
	if got := rc.indexAt(1_000_000); got < 0 {
		t.Fatalf("indexAt with bad config = %d", got)
	}
}

func TestRoomClockReanchorTempoUpDoesNotRetreat(t *testing.T) {
	// 120 BPM, 16-beat intervals (8s). Mid interval 2, jump to 480 BPM (2s
	// intervals): the 4s remaining in interval 2 exceeds the new interval
	// length, so new-tempo math alone would report the index *behind* the
	// current one during the transition — the room index ticks backward and
	// every client labeler aligned then is off by one for the session.
	rc := newRoomClock(roomAnchor{Index: 0, AtMicros: 0, TempoBPM: 120, Config: intervalConfig{Bars: 4, Quantum: 4}})
	before := rc.indexAt(20_000_000)
	if before != 2 {
		t.Fatalf("pre-reanchor index = %d, want 2", before)
	}
	rc.reanchor(20_000_000, 480, intervalConfig{Bars: 4, Quantum: 4})
	for us := int64(20_000_000); us < 24_000_000; us += 250_000 {
		if got := rc.indexAt(us); got != before {
			t.Fatalf("indexAt(%dus) = %d during transition, want pinned %d (room index must not retreat)", us, got, before)
		}
	}
	if got := rc.indexAt(24_000_000); got != 3 {
		t.Fatalf("indexAt(24s) = %d, want 3", got)
	}

	// A second re-anchor inside the same transition must be idempotent, not
	// walk the clock further backward.
	rc.reanchor(21_000_000, 480, intervalConfig{Bars: 4, Quantum: 4})
	if got := rc.anchor().AtMicros; got != 24_000_000 {
		t.Fatalf("idempotent re-anchor moved the boundary to %dus, want 24s", got)
	}
	if got := rc.indexAt(22_000_000); got != 2 {
		t.Fatalf("after idempotent re-anchor indexAt(22s) = %d, want 2", got)
	}
}

func TestObserveSyncSuppressesUnchangedAnchor(t *testing.T) {
	r := newRoom()

	if _, ok := r.observeSync("test", []byte(`{"type":"TempoChange","bpm":120,"quantum":4}`)); !ok {
		t.Fatal("first TempoChange should anchor the clock")
	}
	// Identical values again: no re-anchor, no broadcast. A redundant anchor
	// only re-rolls every client's labeler alignment — and each re-roll can
	// shift a peer's room labels by a whole interval (off-by-one hazard).
	if _, ok := r.observeSync("test", []byte(`{"type":"TempoChange","bpm":120,"quantum":4}`)); ok {
		t.Fatal("unchanged TempoChange must not re-anchor")
	}
	if _, ok := r.observeSync("test", []byte(`{"type":"IntervalConfig","bars":4,"quantum":4}`)); ok {
		t.Fatal("unchanged IntervalConfig must not re-anchor")
	}
	// Real changes still anchor...
	if _, ok := r.observeSync("test", []byte(`{"type":"TempoChange","bpm":128,"quantum":4}`)); !ok {
		t.Fatal("a tempo change must re-anchor")
	}
	if _, ok := r.observeSync("test", []byte(`{"type":"IntervalConfig","bars":8,"quantum":4}`)); !ok {
		t.Fatal("an interval change must re-anchor")
	}
	// ...and then go quiet on repeats.
	if _, ok := r.observeSync("test", []byte(`{"type":"IntervalConfig","bars":8,"quantum":4}`)); ok {
		t.Fatal("repeated IntervalConfig must not re-anchor")
	}
}

func TestObserveSyncMaintainsClock(t *testing.T) {
	r := newRoom()

	// A non-clock sync leaves the clock uninitialised.
	if _, ok := r.observeSync("test", []byte(`{"type":"ChatMessage","text":"hi"}`)); ok {
		t.Fatal("ChatMessage should not affect the clock")
	}
	if _, ok := r.currentAnchor(); ok {
		t.Fatal("no clock should exist before any tempo/config")
	}

	// A TempoChange bootstraps the clock and yields an anchor.
	am, ok := r.observeSync("test", []byte(`{"type":"TempoChange","bpm":120,"quantum":4}`))
	if !ok {
		t.Fatal("TempoChange should produce an anchor")
	}
	if am.Type != "interval_anchor" || am.BPM != 120 || am.Quantum != 4 {
		t.Fatalf("anchor = %+v", am)
	}
	if am.Bars != 4 {
		t.Fatalf("bars defaulted to %d, want 4", am.Bars)
	}

	// IntervalConfig updates bars and re-anchors.
	am2, ok := r.observeSync("test", []byte(`{"type":"IntervalConfig","bars":8,"quantum":4}`))
	if !ok || am2.Bars != 8 {
		t.Fatalf("IntervalConfig anchor = %+v, ok=%v", am2, ok)
	}

	// Late joiner can read the current anchor.
	if _, ok := r.currentAnchor(); !ok {
		t.Fatal("currentAnchor should exist after a tempo change")
	}
}

func TestAnchorCarriesNextBoundary(t *testing.T) {
	r := newRoom()
	r.observeSync("test", json.RawMessage(`{"type":"TempoChange","bpm":120,"quantum":4}`))
	am, ok := r.currentAnchor()
	if !ok {
		t.Fatal("no anchor after TempoChange")
	}
	// 16 beats at 120 BPM = 8s period: next boundary must be exactly one
	// period past the current interval's start, and in the future.
	idx := r.clk.indexAt(am.ServerNowMicros)
	if am.CurrentIndex != idx {
		t.Fatalf("CurrentIndex = %d, want %d", am.CurrentIndex, idx)
	}
	want := r.clk.boundaryMicros(idx + 1)
	if am.NextBoundaryMicros != want {
		t.Fatalf("NextBoundaryMicros = %d, want %d", am.NextBoundaryMicros, want)
	}
	if am.NextBoundaryMicros <= am.ServerNowMicros {
		t.Fatalf("next boundary %d not in the future of server_now %d", am.NextBoundaryMicros, am.ServerNowMicros)
	}
}

func TestServerPongPayload(t *testing.T) {
	pong, ok := serverPongPayload(json.RawMessage(`{"type":"Ping","id":7,"sent_at_us":123456}`))
	if !ok {
		t.Fatal("Ping payload should get a Pong")
	}
	var m map[string]any
	if err := json.Unmarshal(pong, &m); err != nil {
		t.Fatalf("pong unmarshal: %v", err)
	}
	if m["type"] != "Pong" || m["id"].(float64) != 7 || m["ping_sent_at_us"].(float64) != 123456 {
		t.Fatalf("bad pong fields: %v", m)
	}
	if m["server_now_micros"].(float64) <= 0 {
		t.Fatalf("missing server_now_micros: %v", m)
	}
	// Non-Ping payloads get no direct reply.
	if _, ok := serverPongPayload(json.RawMessage(`{"type":"StateSnapshot","bpm":120}`)); ok {
		t.Fatal("StateSnapshot should not get a Pong")
	}
	if _, ok := serverPongPayload(json.RawMessage(`{"type":"Ping"}`)); !ok {
		t.Fatal("Ping with zero id/sent_at is still a Ping")
	}
}
