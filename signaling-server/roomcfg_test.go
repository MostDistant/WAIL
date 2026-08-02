package main

import (
	"encoding/json"
	"testing"
)

// The room's tempo/config tracking after ADR-0009: plain values for joiner
// seeding, updated from TempoChange, TempoDeclare, and IntervalConfig — no
// clock, no index, no re-anchor mechanics.

func peek(t *testing.T, r *room, payload string) (anchorMsg, bool) {
	t.Helper()
	return r.observeSync("test-room", json.RawMessage(payload))
}

func TestObserveSyncTracksTempoAndConfig(t *testing.T) {
	r := newRoom()

	// Nothing observed yet: no config for a joiner.
	if _, ok := r.currentAnchor(); ok {
		t.Fatal("virgin room should have no config")
	}

	// Legacy TempoChange seeds it (defaults fill the unset config).
	am, ok := peek(t, r, `{"type":"TempoChange","bpm":124}`)
	if !ok || am.BPM != 124 || am.Bars != 4 || am.Quantum != 4 {
		t.Fatalf("TempoChange seed → (%+v, %v)", am, ok)
	}
	if am.CurrentIndex != 0 {
		t.Fatalf("current_index = %d, want 0 (vestigial, ADR-0009)", am.CurrentIndex)
	}

	// TempoDeclare updates it too — clients can eventually stop dual-sending.
	am, ok = peek(t, r, `{"type":"TempoDeclare","bpm":98,"origin_micros":123,"owner":"a"}`)
	if !ok || am.BPM != 98 {
		t.Fatalf("TempoDeclare → (%+v, %v)", am, ok)
	}

	// IntervalConfig changes the shape, keeps the tempo.
	am, ok = peek(t, r, `{"type":"IntervalConfig","bars":8,"quantum":4}`)
	if !ok || am.BPM != 98 || am.Bars != 8 {
		t.Fatalf("IntervalConfig → (%+v, %v)", am, ok)
	}

	// A joiner gets the latest values.
	am, ok = r.currentAnchor()
	if !ok || am.BPM != 98 || am.Bars != 8 || am.Quantum != 4 {
		t.Fatalf("joiner config → (%+v, %v)", am, ok)
	}
}

func TestObserveSyncSuppressesRedundantBroadcasts(t *testing.T) {
	r := newRoom()
	peek(t, r, `{"type":"TempoChange","bpm":120,"bars":4,"quantum":4}`)

	// The same values again — a join-time IntervalConfig re-broadcast, or the
	// dual-sent legacy copy of an already-observed declaration: no broadcast.
	if _, ok := peek(t, r, `{"type":"IntervalConfig","bars":4,"quantum":4}`); ok {
		t.Fatal("unchanged config must not re-broadcast")
	}
	if _, ok := peek(t, r, `{"type":"TempoChange","bpm":120}`); ok {
		t.Fatal("unchanged tempo must not re-broadcast")
	}
}

func TestObserveSyncIgnoresNonConfigTraffic(t *testing.T) {
	r := newRoom()
	for _, payload := range []string{
		`{"type":"Ping","id":1,"sent_at_us":5}`,
		`{"type":"StateSnapshot","bpm":97,"beat":1.5}`,
		`{"type":"ChatMessage","text":"TempoChange is a nice word"}`,
	} {
		if _, ok := peek(t, r, payload); ok {
			t.Fatalf("non-config payload updated the room: %s", payload)
		}
	}
	// The snapshot's tempo must NOT have leaked into room state.
	if _, ok := r.currentAnchor(); ok {
		t.Fatal("room config set by non-config traffic")
	}
}
