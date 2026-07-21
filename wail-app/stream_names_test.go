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
		{StreamID: 3, Name: "", Enabled: true},                  // nameless: no default
	}
	overrides := map[uint16]string{
		1: "My Custom Drums", // user rename wins over the channel name
		7: "Test Tone",       // in-app sender, no capture channel
	}
	got := effectiveStreamNames(channels, overrides)
	want := map[uint16]string{
		0: "Synth Bus",
		1: "My Custom Drums",
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
