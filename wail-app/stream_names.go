package main

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
)

const streamNamesFilename = "stream_names.json"

// StreamNameEntry is one persisted rename, keyed the way a user thinks of the
// channel rather than by stream index.
type StreamNameEntry struct {
	CaptureChannelKey
	Name string `json:"name"`
}

// StreamNameStore is the persisted rename set. Stream indices are handed out in
// Link discovery order (they are not stable across restarts), so names key on
// (peer name, channel name) — the same reasoning as CaptureChannelKey. Legacy
// holds an index-keyed file written before that change; each entry is adopted
// and re-keyed the first time its index turns up as a live channel.
type StreamNameStore struct {
	ByChannel []StreamNameEntry
	Legacy    map[uint16]string
}

func (s *StreamNameStore) nameFor(k CaptureChannelKey) (string, bool) {
	for _, e := range s.ByChannel {
		if e.CaptureChannelKey == k {
			return e.Name, true
		}
	}
	return "", false
}

func (s *StreamNameStore) put(k CaptureChannelKey, name string) {
	for i := range s.ByChannel {
		if s.ByChannel[i].CaptureChannelKey == k {
			s.ByChannel[i].Name = name
			return
		}
	}
	s.ByChannel = append(s.ByChannel, StreamNameEntry{CaptureChannelKey: k, Name: name})
}

// LoadStreamNames loads persisted stream names. Returns an empty store on any
// failure, and falls back to reading the pre-re-key index-keyed format.
func LoadStreamNames(dataDir string) *StreamNameStore {
	store := &StreamNameStore{}
	data, err := os.ReadFile(filepath.Join(dataDir, streamNamesFilename))
	if err != nil {
		return store
	}

	if err := json.Unmarshal(data, &store.ByChannel); err == nil {
		if len(store.ByChannel) > 0 {
			log.Printf("[stream_names] Loaded %d stream names", len(store.ByChannel))
		}
		return store
	}

	// Pre-re-key format: a bare object of stream index → name.
	var byIndex map[string]string
	if err := json.Unmarshal(data, &byIndex); err != nil {
		log.Printf("[stream_names] Failed to parse %s: %v", streamNamesFilename, err)
		return store
	}
	store.Legacy = make(map[uint16]string, len(byIndex))
	for k, v := range byIndex {
		var idx uint16
		if _, err := fmt.Sscanf(k, "%d", &idx); err == nil {
			store.Legacy[idx] = v
		}
	}
	if len(store.Legacy) > 0 {
		log.Printf("[stream_names] Loaded %d stream names in the old index-keyed format; "+
			"each is re-keyed to its channel the first time that channel appears", len(store.Legacy))
	}
	return store
}

// resolveStreamNames folds persisted names into this session's override map,
// which is keyed by the stream index the engine happened to mint this run. It
// never clobbers an override already set (a rename made this session wins), and
// it adopts + re-keys any legacy index-keyed entry whose index is now live.
// Reports whether anything changed.
func resolveStreamNames(channels []CaptureChannelInfo, store *StreamNameStore, overrides map[uint16]string) bool {
	changed := false
	for _, cc := range channels {
		if _, ok := overrides[cc.StreamID]; ok {
			continue
		}
		key := CaptureChannelKey{PeerName: cc.PeerName, ChannelName: cc.Name}
		if name, ok := store.nameFor(key); ok {
			overrides[cc.StreamID] = name
			changed = true
			continue
		}
		if name, ok := store.Legacy[cc.StreamID]; ok {
			overrides[cc.StreamID] = name
			delete(store.Legacy, cc.StreamID)
			store.put(key, name)
			changed = true
		}
	}
	return changed
}

// streamNameEntries projects the session's index-keyed overrides back onto
// human-stable channel keys for persistence. Overrides with no capture channel
// behind them (test tone, WAV, metronome) are session-scoped and skipped.
func streamNameEntries(channels []CaptureChannelInfo, overrides map[uint16]string) []StreamNameEntry {
	entries := make([]StreamNameEntry, 0, len(overrides))
	for _, cc := range channels {
		if name, ok := overrides[cc.StreamID]; ok {
			entries = append(entries, StreamNameEntry{
				CaptureChannelKey: CaptureChannelKey{PeerName: cc.PeerName, ChannelName: cc.Name},
				Name:              name,
			})
		}
	}
	return entries
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
func SaveStreamNames(dataDir string, entries []StreamNameEntry) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Printf("[stream_names] Failed to create data dir: %v", err)
		return
	}
	if entries == nil {
		entries = []StreamNameEntry{}
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		log.Printf("[stream_names] Failed to serialize: %v", err)
		return
	}
	path := filepath.Join(dataDir, streamNamesFilename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[stream_names] Failed to write %s: %v", streamNamesFilename, err)
	}
}

// streamNameBroadcaster owns the "have the peers been told?" half of the
// StreamNames sync. It records the last map the relay *accepted*, not the last
// one we tried to send: a sync dropped on a full outgoing queue is silent, and
// because the map itself hasn't changed nothing ever retries it — receivers
// would sit on "stream N" for the rest of the session.
type streamNameBroadcaster struct{ sent map[uint16]string }

// Sync hands eff to send when it differs from the last accepted map. send
// reports whether the relay took the message; a refusal leaves the record
// untouched, so the next status tick retries.
func (b *streamNameBroadcaster) Sync(eff map[uint16]string, send func(map[uint16]string) bool) {
	if maps.Equal(eff, b.sent) {
		return
	}
	if !send(eff) {
		return
	}
	b.sent = maps.Clone(eff)
}

// Reset forgets the record so the next Sync resends unchanged names — a peer
// that just joined has never heard them.
func (b *streamNameBroadcaster) Reset() { b.sent = nil }
