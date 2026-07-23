package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const streamNamesFilename = "stream_names.json"

// LoadStreamNames loads stream names from disk. Returns empty map on any failure.
func LoadStreamNames(dataDir string) map[uint16]string {
	path := filepath.Join(dataDir, streamNamesFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return make(map[uint16]string)
	}

	var stringMap map[string]string
	if err := json.Unmarshal(data, &stringMap); err != nil {
		log.Printf("[stream_names] Failed to parse %s: %v", streamNamesFilename, err)
		return make(map[uint16]string)
	}

	names := make(map[uint16]string, len(stringMap))
	for k, v := range stringMap {
		var idx uint16
		if _, err := fmt.Sscanf(k, "%d", &idx); err == nil {
			names[idx] = v
		}
	}
	if len(names) > 0 {
		log.Printf("[stream_names] Loaded %d stream names", len(names))
	}
	return names
}

// effectiveStreamNames is the map receivers should label our streams with:
// each enabled capture channel defaults to its discovered Link Audio channel
// name (falling back to "stream N" when the channel is nameless, so receivers
// never render an unnamed stream), overridden by any user-set names (which
// also carry the test tone / WAV sender labels). Broadcast via the StreamNames
// sync whenever it changes.
func effectiveStreamNames(captureChannels []CaptureChannelInfo, overrides map[uint16]string) map[uint16]string {
	eff := make(map[uint16]string, len(captureChannels)+len(overrides))
	for _, cc := range captureChannels {
		if cc.Enabled {
			name := cc.Name
			if name == "" {
				name = fmt.Sprintf("stream %d", cc.StreamID)
			}
			eff[cc.StreamID] = name
		}
	}
	for k, v := range overrides {
		eff[k] = v
	}
	return eff
}

// SaveStreamNames persists stream names to disk. Logs on failure, never panics.
func SaveStreamNames(dataDir string, names map[uint16]string) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Printf("[stream_names] Failed to create data dir: %v", err)
		return
	}
	stringMap := make(map[string]string, len(names))
	for k, v := range names {
		stringMap[fmt.Sprintf("%d", k)] = v
	}
	data, err := json.MarshalIndent(stringMap, "", "  ")
	if err != nil {
		log.Printf("[stream_names] Failed to serialize: %v", err)
		return
	}
	path := filepath.Join(dataDir, streamNamesFilename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[stream_names] Failed to write %s: %v", streamNamesFilename, err)
	}
}
