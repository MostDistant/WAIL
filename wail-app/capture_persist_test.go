package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCaptureEnabledEmptyDirReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	keys := LoadCaptureEnabled(dir)
	if len(keys) != 0 {
		t.Fatalf("expected empty, got %d", len(keys))
	}
}

func TestCaptureEnabledSaveAndLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	keys := []CaptureChannelKey{
		{PeerName: "Live", ChannelName: "Main"},
		{PeerName: "Live", ChannelName: "Synth Bus"},
		{PeerName: "Reaper", ChannelName: "Drums"},
	}

	SaveCaptureEnabled(dir, keys)
	loaded := LoadCaptureEnabled(dir)

	if len(loaded) != len(keys) {
		t.Fatalf("expected %d keys, got %d", len(keys), len(loaded))
	}
	got := make(map[CaptureChannelKey]bool, len(loaded))
	for _, k := range loaded {
		got[k] = true
	}
	for _, k := range keys {
		if !got[k] {
			t.Fatalf("missing key after roundtrip: %+v (got %v)", k, loaded)
		}
	}
}

func TestCaptureEnabledSaveEmptyOverwrites(t *testing.T) {
	dir := t.TempDir()
	SaveCaptureEnabled(dir, []CaptureChannelKey{{PeerName: "Live", ChannelName: "Main"}})

	SaveCaptureEnabled(dir, nil)
	loaded := LoadCaptureEnabled(dir)
	if len(loaded) != 0 {
		t.Fatalf("expected empty after overwrite, got %d", len(loaded))
	}
}

func TestLoadCaptureEnabledInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, captureEnabledFilename), []byte("not json"), 0o644)
	keys := LoadCaptureEnabled(dir)
	if len(keys) != 0 {
		t.Fatal("expected empty for invalid JSON")
	}
}

func TestDeleteCaptureEnabled(t *testing.T) {
	dir := t.TempDir()
	SaveCaptureEnabled(dir, []CaptureChannelKey{{PeerName: "Live", ChannelName: "Main"}})

	DeleteCaptureEnabled(dir)
	if _, err := os.Stat(filepath.Join(dir, captureEnabledFilename)); !os.IsNotExist(err) {
		t.Fatalf("expected file removed, stat err = %v", err)
	}
	// Deleting a missing file is not an error.
	DeleteCaptureEnabled(dir)
}
