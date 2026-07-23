package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

const captureEnabledFilename = "capture_enabled.json"

// CaptureChannelKey identifies a local Link Audio channel by its human-stable
// identity. Channel IDs are minted per publisher lifetime (a DAW restart
// changes them), so persistence keys on (peer name, channel name) — what a
// user thinks of as "the same channel".
type CaptureChannelKey struct {
	PeerName    string `json:"peer_name"`
	ChannelName string `json:"channel_name"`
}

// LoadCaptureEnabled loads the remembered set of enabled capture channels.
// Returns nil on any failure (missing file, corrupt JSON).
func LoadCaptureEnabled(dataDir string) []CaptureChannelKey {
	path := filepath.Join(dataDir, captureEnabledFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keys []CaptureChannelKey
	if err := json.Unmarshal(data, &keys); err != nil {
		log.Printf("[capture] Failed to parse %s: %v", captureEnabledFilename, err)
		return nil
	}
	if len(keys) > 0 {
		log.Printf("[capture] Loaded %d remembered capture channels", len(keys))
	}
	return keys
}

// SaveCaptureEnabled persists the set of enabled capture channels.
// Logs on failure, never panics.
func SaveCaptureEnabled(dataDir string, keys []CaptureChannelKey) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Printf("[capture] Failed to create data dir: %v", err)
		return
	}
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		log.Printf("[capture] Failed to serialize: %v", err)
		return
	}
	path := filepath.Join(dataDir, captureEnabledFilename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[capture] Failed to write %s: %v", captureEnabledFilename, err)
	}
}

// DeleteCaptureEnabled removes the persisted set (when "Remember settings" is
// turned off). A missing file is not an error.
func DeleteCaptureEnabled(dataDir string) {
	path := filepath.Join(dataDir, captureEnabledFilename)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[capture] Failed to delete %s: %v", captureEnabledFilename, err)
	}
}
