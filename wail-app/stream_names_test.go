package main

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func chanKey(peer, channel string) CaptureChannelKey {
	return CaptureChannelKey{PeerName: peer, ChannelName: channel}
}

func TestLoadEmptyDirReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	store := LoadStreamNames(dir)
	if len(store.ByChannel) != 0 || len(store.Legacy) != 0 {
		t.Fatalf("expected empty, got %+v", store)
	}
}

func TestSaveAndLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	entries := []StreamNameEntry{
		{CaptureChannelKey: chanKey("Live", "Bus 1"), Name: "Bass"},
		{CaptureChannelKey: chanKey("Live", "Bus 2"), Name: "Drums"},
	}

	SaveStreamNames(dir, entries)
	loaded := LoadStreamNames(dir)

	if len(loaded.ByChannel) != 2 {
		t.Fatalf("expected 2, got %d", len(loaded.ByChannel))
	}
	if n, _ := loaded.nameFor(chanKey("Live", "Bus 1")); n != "Bass" {
		t.Fatalf("mismatch: %+v", loaded.ByChannel)
	}
	if n, _ := loaded.nameFor(chanKey("Live", "Bus 2")); n != "Drums" {
		t.Fatalf("mismatch: %+v", loaded.ByChannel)
	}
}

func TestSaveEmptyOverwrites(t *testing.T) {
	dir := t.TempDir()
	SaveStreamNames(dir, []StreamNameEntry{{CaptureChannelKey: chanKey("Live", "Bus 1"), Name: "Bass"}})

	SaveStreamNames(dir, nil)
	loaded := LoadStreamNames(dir)
	if len(loaded.ByChannel) != 0 {
		t.Fatalf("expected empty after overwrite, got %+v", loaded.ByChannel)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, streamNamesFilename), []byte("not json"), 0o644)
	store := LoadStreamNames(dir)
	if len(store.ByChannel) != 0 || len(store.Legacy) != 0 {
		t.Fatalf("expected empty for invalid JSON, got %+v", store)
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

// Persisted renames key on (peer name, channel name), not the stream index:
// indices are handed out in Link discovery order, so an index-keyed rename
// lands on whatever channel happens to be discovered there after a restart.
func TestStreamNameStoreRoundtripsByChannel(t *testing.T) {
	dir := t.TempDir()
	entries := []StreamNameEntry{
		{CaptureChannelKey: CaptureChannelKey{PeerName: "Live", ChannelName: "Bus 1"}, Name: "Bass DI"},
	}

	SaveStreamNames(dir, entries)
	store := LoadStreamNames(dir)

	if got := store.ByChannel; len(got) != 1 || got[0] != entries[0] {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(store.Legacy) != 0 {
		t.Fatalf("v2 file must not populate Legacy: %v", store.Legacy)
	}
}

// A rename saved before the re-key is still keyed by index. It gets adopted the
// first time that index shows up as a live channel, then persists by channel.
func TestLoadStreamNamesReadsLegacyIndexKeyedFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, streamNamesFilename), []byte(`{"0":"Bass DI","3":"Drums"}`), 0o644)

	store := LoadStreamNames(dir)

	if len(store.ByChannel) != 0 {
		t.Fatalf("legacy file has no channel keys, got %+v", store.ByChannel)
	}
	if store.Legacy[0] != "Bass DI" || store.Legacy[3] != "Drums" {
		t.Fatalf("legacy names not read: %v", store.Legacy)
	}
}

func TestResolveStreamNamesAppliesPersistedNameToItsChannel(t *testing.T) {
	channels := []CaptureChannelInfo{
		{StreamID: 0, PeerName: "Live", Name: "Bus 2", Enabled: true},
		{StreamID: 1, PeerName: "Live", Name: "Bus 1", Enabled: true},
	}
	store := &StreamNameStore{ByChannel: []StreamNameEntry{
		{CaptureChannelKey: CaptureChannelKey{PeerName: "Live", ChannelName: "Bus 1"}, Name: "Bass DI"},
	}}
	overrides := map[uint16]string{}

	if !resolveStreamNames(channels, store, overrides) {
		t.Fatal("expected resolve to report a change")
	}
	// "Bus 1" is stream 1 this run even though it was stream 0 when saved.
	if overrides[1] != "Bass DI" {
		t.Fatalf("persisted name must follow its channel, got %v", overrides)
	}
	if _, ok := overrides[0]; ok {
		t.Fatalf("must not label the channel that merely reused the index: %v", overrides)
	}
}

func TestResolveStreamNamesAdoptsLegacyIndexThenKeysIt(t *testing.T) {
	channels := []CaptureChannelInfo{
		{StreamID: 0, PeerName: "Live", Name: "Bus 1", Enabled: true},
	}
	store := &StreamNameStore{Legacy: map[uint16]string{0: "Bass DI"}}
	overrides := map[uint16]string{}

	if !resolveStreamNames(channels, store, overrides) {
		t.Fatal("expected resolve to report a change")
	}
	if overrides[0] != "Bass DI" {
		t.Fatalf("legacy name not adopted: %v", overrides)
	}
	if len(store.Legacy) != 0 {
		t.Fatalf("adopted legacy entry must be consumed, got %v", store.Legacy)
	}
	want := StreamNameEntry{CaptureChannelKey: CaptureChannelKey{PeerName: "Live", ChannelName: "Bus 1"}, Name: "Bass DI"}
	if len(store.ByChannel) != 1 || store.ByChannel[0] != want {
		t.Fatalf("legacy entry must be re-keyed, got %+v", store.ByChannel)
	}
}

// A user rename made this session must win over whatever was persisted.
func TestResolveStreamNamesDoesNotClobberALiveOverride(t *testing.T) {
	channels := []CaptureChannelInfo{
		{StreamID: 0, PeerName: "Live", Name: "Bus 1", Enabled: true},
	}
	store := &StreamNameStore{ByChannel: []StreamNameEntry{
		{CaptureChannelKey: CaptureChannelKey{PeerName: "Live", ChannelName: "Bus 1"}, Name: "Bass DI"},
	}}
	overrides := map[uint16]string{0: "Renamed Just Now"}

	if resolveStreamNames(channels, store, overrides) {
		t.Fatal("nothing to resolve — must report no change")
	}
	if overrides[0] != "Renamed Just Now" {
		t.Fatalf("live override clobbered: %v", overrides)
	}
}

// Only capture channels get persisted: the test tone / WAV / metronome labels
// live at synthetic indices with no channel behind them.
func TestReconcileSkipsSendersWithNoChannel(t *testing.T) {
	channels := []CaptureChannelInfo{
		{StreamID: 0, PeerName: "Live", Name: "Bus 1", Enabled: true},
	}
	overrides := map[uint16]string{0: "Bass DI", 0xFF01: "Metronome"}

	store := &StreamNameStore{}
	store.Reconcile(channels, overrides)

	want := []StreamNameEntry{
		{CaptureChannelKey: chanKey("Live", "Bus 1"), Name: "Bass DI"},
	}
	if got := store.Entries(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("Entries = %+v, want %+v", got, want)
	}
}

// The save payload is built from the live capture channels, but the file is a
// full overwrite — so a name for a channel that simply isn't on the LAN today
// must still survive. Persist the union the store holds, not the live slice.
func TestSaveKeepsNamesForChannelsNotCurrentlyPresent(t *testing.T) {
	dir := t.TempDir()
	SaveStreamNames(dir, []StreamNameEntry{
		{CaptureChannelKey: chanKey("Live", "Bus 1"), Name: "Bass"},
		{CaptureChannelKey: chanKey("Bitwig", "Master"), Name: "Gtr"},
	})

	// Next session: only Bitwig is running.
	store := LoadStreamNames(dir)
	channels := []CaptureChannelInfo{{StreamID: 0, PeerName: "Bitwig", Name: "Master", Enabled: true}}
	overrides := map[uint16]string{}
	resolveStreamNames(channels, store, overrides)

	SaveStreamNames(dir, store.Entries())

	reloaded := LoadStreamNames(dir)
	if n, ok := reloaded.nameFor(chanKey("Live", "Bus 1")); !ok || n != "Bass" {
		t.Fatalf("absent channel's name was erased: %+v", reloaded.ByChannel)
	}
}

// Clearing a name has to stick. Without a delete on the store, the next resolve
// pass re-applies the old name and writes it straight back to disk.
func TestClearingANameRemovesItFromTheStore(t *testing.T) {
	channels := []CaptureChannelInfo{{StreamID: 0, PeerName: "Live", Name: "Bus 1", Enabled: true}}
	store := &StreamNameStore{ByChannel: []StreamNameEntry{
		{CaptureChannelKey: chanKey("Live", "Bus 1"), Name: "Bass"},
	}}

	// User clears the override; the session syncs the now-empty map.
	overrides := map[uint16]string{}
	store.Reconcile(channels, overrides)

	if resolveStreamNames(channels, store, overrides) {
		t.Fatal("a cleared name must not be resurrected by the next resolve")
	}
	if _, ok := overrides[0]; ok {
		t.Fatalf("cleared name came back: %v", overrides)
	}
	if _, ok := store.nameFor(chanKey("Live", "Bus 1")); ok {
		t.Fatal("cleared name still in the store — it would be rewritten to disk")
	}
}

// Reconcile must not treat "this channel isn't here right now" as "deleted".
func TestReconcileOnlyForgetsNamesForLiveChannels(t *testing.T) {
	channels := []CaptureChannelInfo{{StreamID: 0, PeerName: "Live", Name: "Bus 1", Enabled: true}}
	store := &StreamNameStore{ByChannel: []StreamNameEntry{
		{CaptureChannelKey: chanKey("Live", "Bus 1"), Name: "Bass"},
		{CaptureChannelKey: chanKey("Bitwig", "Master"), Name: "Gtr"},
	}}

	store.Reconcile(channels, map[uint16]string{}) // Bus 1 cleared, Bitwig absent

	if _, ok := store.nameFor(chanKey("Live", "Bus 1")); ok {
		t.Fatal("live channel's cleared name should be forgotten")
	}
	if n, _ := store.nameFor(chanKey("Bitwig", "Master")); n != "Gtr" {
		t.Fatal("absent channel's name must be left alone")
	}
}
