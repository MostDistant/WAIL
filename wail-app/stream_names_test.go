package main

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEmptyDirReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	names := LoadStreamNames(dir)
	if len(names) != 0 {
		t.Fatalf("expected empty, got %d", len(names))
	}
}

func TestSaveAndLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	names := map[uint16]string{0: "Bass", 3: "Drums"}

	SaveStreamNames(dir, names)
	loaded := LoadStreamNames(dir)

	if len(loaded) != 2 {
		t.Fatalf("expected 2, got %d", len(loaded))
	}
	if loaded[0] != "Bass" || loaded[3] != "Drums" {
		t.Fatalf("mismatch: %v", loaded)
	}
}

func TestSaveEmptyOverwrites(t *testing.T) {
	dir := t.TempDir()
	names := map[uint16]string{0: "Bass"}
	SaveStreamNames(dir, names)

	SaveStreamNames(dir, map[uint16]string{})
	loaded := LoadStreamNames(dir)
	if len(loaded) != 0 {
		t.Fatalf("expected empty after overwrite, got %d", len(loaded))
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, streamNamesFilename), []byte("not json"), 0o644)
	names := LoadStreamNames(dir)
	if len(names) != 0 {
		t.Fatal("expected empty for invalid JSON")
	}
}

func TestEffectiveStreamNamesDefaultsAndOverrides(t *testing.T) {
	channels := []CaptureChannelInfo{
		{StreamID: 0, Name: "Synth Bus", Enabled: true},
		{StreamID: 1, Name: "Drums", Enabled: true},
		{StreamID: 2, Name: "Disabled Channel", Enabled: false}, // not sending: no name
		{StreamID: 3, Name: "", Enabled: true},                  // nameless: falls back to "stream N"
	}
	overrides := map[uint16]string{
		1: "My Custom Drums", // user rename wins over the channel name
		7: "Test Tone",       // in-app sender, no capture channel
	}
	got := effectiveStreamNames(channels, overrides)
	want := map[uint16]string{
		0: "Synth Bus",
		1: "My Custom Drums",
		3: "stream 3",
		7: "Test Tone",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("effectiveStreamNames = %v, want %v", got, want)
	}
}

func TestEffectiveStreamNamesEmptyInputs(t *testing.T) {
	if got := effectiveStreamNames(nil, nil); len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

// The broadcaster records the last map the relay actually accepted, not the
// last one we tried to send. Recording an attempt as delivered strands
// receivers on "stream N" for the life of the session, because the map itself
// never changes again.
func TestStreamNameBroadcasterResendsAfterADroppedSend(t *testing.T) {
	b := &streamNameBroadcaster{}
	names := map[uint16]string{0: "Bass DI"}

	sends := 0
	refuse := func(map[uint16]string) bool { sends++; return false }

	b.Sync(names, refuse)
	b.Sync(names, refuse)

	if sends != 2 {
		t.Fatalf("a refused send must be retried on the next Sync, got %d sends", sends)
	}
}

func TestStreamNameBroadcasterSkipsUnchangedAfterSuccess(t *testing.T) {
	b := &streamNameBroadcaster{}
	names := map[uint16]string{0: "Bass DI"}

	sends := 0
	accept := func(map[uint16]string) bool { sends++; return true }

	b.Sync(names, accept)
	b.Sync(map[uint16]string{0: "Bass DI"}, accept)

	if sends != 1 {
		t.Fatalf("an unchanged map must not resend, got %d sends", sends)
	}
}

func TestStreamNameBroadcasterResetForcesResend(t *testing.T) {
	b := &streamNameBroadcaster{}
	names := map[uint16]string{0: "Bass DI"}

	sends := 0
	accept := func(map[uint16]string) bool { sends++; return true }

	b.Sync(names, accept)
	b.Reset()
	b.Sync(names, accept)

	if sends != 2 {
		t.Fatalf("Reset must force a resend for a newly joined peer, got %d sends", sends)
	}
}

// Sync is handed the live map the session keeps mutating, so the record has to
// be a snapshot or a later in-place edit reads back as "unchanged".
func TestStreamNameBroadcasterSnapshotsTheSentMap(t *testing.T) {
	b := &streamNameBroadcaster{}
	names := map[uint16]string{0: "Bass DI"}

	sends := 0
	accept := func(map[uint16]string) bool { sends++; return true }

	b.Sync(names, accept)
	names[1] = "Synth"
	b.Sync(names, accept)

	if sends != 2 {
		t.Fatalf("a mutated map must count as changed, got %d sends", sends)
	}
}
