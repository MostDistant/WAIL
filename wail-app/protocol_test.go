package main

import (
	"encoding/json"
	"testing"
)

func TestSyncMessageHelloRoundtrip(t *testing.T) {
	dn := "Ringo"
	id := "stable-uuid-1234"
	msg := NewHello("abc123", &dn, &id)
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SyncMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "Hello" || decoded.PeerID != "abc123" {
		t.Fatal("field mismatch")
	}
	if decoded.DisplayName == nil || *decoded.DisplayName != "Ringo" {
		t.Fatal("display name mismatch")
	}
	if decoded.Identity == nil || *decoded.Identity != "stable-uuid-1234" {
		t.Fatal("identity mismatch")
	}
}

func TestSyncMessageIntervalBoundaryRoundtrip(t *testing.T) {
	msg := NewIntervalBoundary(42)
	data, _ := json.Marshal(msg)
	var decoded SyncMessage
	json.Unmarshal(data, &decoded)
	if decoded.Type != "IntervalBoundary" || decoded.Index != 42 {
		t.Fatal("mismatch")
	}
}

func TestStreamNamesToWireRoundtrip(t *testing.T) {
	names := map[uint16]string{0: "Bass", 1: "Drums"}
	wire := StreamNamesToWire(names)
	if wire["0"] != "Bass" || wire["1"] != "Drums" {
		t.Fatal("wire conversion failed")
	}
	back := StreamNamesFromWire(wire)
	if back[0] != "Bass" || back[1] != "Drums" {
		t.Fatal("round-trip failed")
	}
}

func TestChatMessageRoundtrip(t *testing.T) {
	msg := NewChatMessage("Ringo", "Let's change key")
	data, _ := json.Marshal(msg)
	var decoded SyncMessage
	json.Unmarshal(data, &decoded)
	if decoded.Type != "ChatMessage" || decoded.SenderName != "Ringo" || decoded.Text != "Let's change key" {
		t.Fatal("mismatch")
	}
}

func TestAudioStatusRoundtrip(t *testing.T) {
	msg := NewAudioStatus(true, 5, 3, true, 42)
	data, _ := json.Marshal(msg)
	var decoded SyncMessage
	json.Unmarshal(data, &decoded)
	if decoded.Type != "AudioStatus" || !decoded.AudioDCOpen || decoded.IntervalsSent != 5 {
		t.Fatal("mismatch")
	}
	if decoded.Seq != 42 {
		t.Fatalf("expected seq 42, got %d", decoded.Seq)
	}
}

// The declaration arbitration rule (ADR-0009): strictly-greater origin wins,
// lowest owner breaks exact ties, duplicates are structurally inert. The rule's
// behavior over a lossy 100-300ms relay is pinned in tempo_sim_wan_test.go;
// this is the boundary table.
func TestTempoDeclareAdopts(t *testing.T) {
	cases := []struct {
		name                 string
		msgOrigin, curOrigin int64
		msgOwner, curOwner   string
		want                 bool
	}{
		{"newer origin wins", 200, 100, "b", "a", true},
		{"older origin loses", 100, 200, "a", "b", false},
		{"exact duplicate is inert", 100, 100, "a", "a", false},
		{"tie: lower owner wins", 100, 100, "a", "b", true},
		{"tie: higher owner loses", 100, 100, "b", "a", false},
		{"anything beats the founding zero state", 1, 0, "a", "", true},
		{"empty owner never beats a real one on a tie", 100, 100, "", "a", false},
	}
	for _, c := range cases {
		if got := tempoDeclareAdopts(c.msgOrigin, c.msgOwner, c.curOrigin, c.curOwner); got != c.want {
			t.Errorf("%s: tempoDeclareAdopts(%d,%q vs %d,%q) = %v, want %v",
				c.name, c.msgOrigin, c.msgOwner, c.curOrigin, c.curOwner, got, c.want)
		}
	}
}
