//go:build !linkstub

package main

import (
	"context"
	"encoding/hex"
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
)

// TestAudioEngineEmitIngestion drives the emit ingestion path end-to-end without
// the network or the engine goroutines: real WAIF frames (built by the real Opus
// encoder) are fed to HandleRemoteAudio, which must decode them, reassemble per
// (identity, stream), and publish a Link Audio sink via the affinity registry.
// It also checks the control surface and that teardown is safe without Start.
func TestAudioEngineEmitIngestion(t *testing.T) {
	lb := NewLinkBridge(120, 4)

	var sent [][]byte
	eng := newAudioEngine(lb, "TestPeer", func(w []byte) { sent = append(sent, w) }, 1)
	le, ok := eng.(*linkAudioEngine)
	if !ok {
		t.Fatalf("expected *linkAudioEngine, got %T", eng)
	}

	// Build a real one-interval WAIF stream (2 frames) via the encoder.
	enc, err := NewIntervalEncoder(2, 48000, 128)
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	pcm := make([]int16, 960*2*2) // 2 stereo 20ms frames
	frames, _, err := enc.EncodeInterval(pcm, 5, 0, 0, 120, 4, 4)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("expected >= 2 WAIF frames, got %d", len(frames))
	}
	for _, f := range frames {
		eng.HandleRemoteAudio("identity-A", "Alice", "guitar", f)
	}

	if len(le.emit) != 1 {
		t.Fatalf("expected 1 emit stream, got %d", len(le.emit))
	}
	var st *emitStream
	for _, s := range le.emit {
		st = s
	}
	if len(st.sinks) != 1 {
		t.Fatalf("expected the stream to publish exactly one Link Audio sink, got %d", len(st.sinks))
	}

	// Reconnect: same identity+stream, renamed peer → reuse the same stream and
	// channel (affinity), just refresh the name. Must not mint a new stream/sink.
	firstSink := st.sinks[0]
	eng.HandleRemoteAudio("identity-A", "Alice (reconnected)", "guitar", frames[0])
	if len(le.emit) != 1 {
		t.Fatalf("reconnect minted a new stream: %d", len(le.emit))
	}
	if len(st.sinks) != 1 || st.sinks[0] != firstSink {
		t.Fatal("reconnect replaced the sink — channel affinity broken")
	}
	if st.lastDisplayName != "Alice (reconnected)" {
		t.Fatalf("name not refreshed on reconnect: %q", st.lastDisplayName)
	}

	// A malformed frame must be ignored without creating a stream or panicking.
	eng.HandleRemoteAudio("identity-B", "Bob", "bass", []byte{'x', 'y', 'z'})
	if len(le.emit) != 1 {
		t.Fatalf("malformed frame created a stream: %d", len(le.emit))
	}

	// Control surface is safe.
	eng.SetRoomAnchor(100, 120, 4, 4)
	if !le.labeler.Aligned() {
		t.Fatal("SetRoomAnchor should align the labeler")
	}
	if len(eng.CaptureChannels()) != 0 {
		t.Fatal("no capture channels expected before discovery runs")
	}

	// Stop without Start must be safe and must tear down the streams (+ sinks).
	eng.Stop()
	if len(le.emit) != 0 {
		t.Fatalf("Stop should clear emit streams, still have %d", len(le.emit))
	}
}

// TestEngineConcealsSeqGapWithPLC ships an interval with one frame withheld:
// the seq gap must be PLC-concealed at decode time (slot filled, not counted
// as received), and the late real frame must replace the concealment.
func TestEngineConcealsSeqGapWithPLC(t *testing.T) {
	lb := NewLinkBridge(120, 4)
	eng := newAudioEngine(lb, "TestPeer", func([]byte) {}, 1)
	le := eng.(*linkAudioEngine)
	defer eng.Stop()

	// Anchor so cfg/tempo-derived interval totals exist for the slot walk.
	eng.SetRoomAnchor(0, 120, 4, 4)

	enc, err := NewIntervalEncoder(2, 48000, 128)
	if err != nil {
		t.Fatal(err)
	}
	// Four loud stereo frames (sine, not silence — PLC needs signal history).
	phase := 0.0
	pcm := make([]int16, 0, 4*960*2)
	for i := 0; i < 4; i++ {
		pcm = append(pcm, sineWindow(960, 2, 220, &phase)...)
	}
	frames, _, err := enc.EncodeInterval(pcm, 5, 0, 0, 120, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4 {
		t.Fatalf("expected 4 frames, got %d", len(frames))
	}

	// Ship 0, 1, then 3 — frame 2 "lost" (seq gap of 1).
	eng.HandleRemoteAudio("id-plc", "Alice", "gtr", frames[0])
	eng.HandleRemoteAudio("id-plc", "Alice", "gtr", frames[1])
	eng.HandleRemoteAudio("id-plc", "Alice", "gtr", frames[3])

	var st *emitStream
	for _, s := range le.emit {
		st = s
	}
	if st == nil {
		t.Fatal("no emit stream")
	}
	if got := st.framesConcealed.Load(); got != 1 {
		t.Fatalf("framesConcealed = %d, want 1", got)
	}
	missing, concealed := st.reasm.Missing(5)
	if missing != 0 || concealed != 1 {
		t.Fatalf("Missing(5) = (%d,%d), want (0,1)", missing, concealed)
	}
	if st.reasm.Complete(5) {
		t.Fatal("PLC must not make the interval Complete")
	}
	// The concealed slot must hold non-silent audio.
	ipcm, _, _, _ := st.reasm.Interval(5)
	var energy int64
	for _, s := range ipcm[2*960*2 : 3*960*2] {
		energy += int64(s) * int64(s)
	}
	if energy == 0 {
		t.Fatal("concealed slot is silent — PLC audio not placed")
	}

	// The late real frame 2 arrives: real audio wins.
	eng.HandleRemoteAudio("id-plc", "Alice", "gtr", frames[2])
	if !st.reasm.Complete(5) {
		t.Fatal("interval should be Complete once the real frame lands")
	}
	if _, concealed := st.reasm.Missing(5); concealed != 0 {
		t.Fatalf("concealed = %d, want 0 after real replacement", concealed)
	}
}

// --- capture restore: remembering which channels the user enabled ---

// newCaptureTestEngine builds a real engine with a live (offline) Link but
// without Start()'s discovery/emit loops; drain goroutines get a context so
// auto-enabled channels are exercisable. Stop runs at cleanup.
func newCaptureTestEngine(t *testing.T) *linkAudioEngine {
	t.Helper()
	eng := newAudioEngine(NewLinkBridge(120, 4), "WAIL", func([]byte) {}, 1)
	le, ok := eng.(*linkAudioEngine)
	if !ok {
		t.Fatalf("expected *linkAudioEngine, got %T", eng)
	}
	le.ctx, le.cancel = context.WithCancel(context.Background())
	t.Cleanup(eng.Stop)
	return le
}

func testDiscoveredChannel(idByte byte, peer, name string) abllink.Channel {
	var id abllink.ChannelID
	id[0] = idByte
	return abllink.Channel{ID: id, Name: name, PeerName: peer}
}

func captureEntry(t *testing.T, le *linkAudioEngine, c abllink.Channel) *captureChannel {
	t.Helper()
	id := hex.EncodeToString(c.ID[:])
	ch, ok := le.capture[id]
	if !ok {
		t.Fatalf("channel %q not registered in capture map", c.Name)
	}
	return ch
}

func TestReconcileRegistersDiscoveredChannelDisabled(t *testing.T) {
	le := newCaptureTestEngine(t)
	c := testDiscoveredChannel(1, "Live", "Main")

	le.reconcileChannels([]abllink.Channel{c})

	if ch := captureEntry(t, le, c); ch.enabled {
		t.Fatal("discovered channel must start disabled (explicit opt-in)")
	}
}

func TestReconcileAutoEnablesRememberedChannel(t *testing.T) {
	le := newCaptureTestEngine(t)
	le.SetCaptureRestore([]CaptureChannelKey{{PeerName: "Live", ChannelName: "Main"}})

	le.reconcileChannels([]abllink.Channel{testDiscoveredChannel(1, "Live", "Main")})

	if ch := captureEntry(t, le, testDiscoveredChannel(1, "Live", "Main")); !ch.enabled {
		t.Fatal("remembered channel should auto-enable on discovery")
	}
}

func TestReconcileDoesNotRestoreNameCollisionFromOtherPeer(t *testing.T) {
	le := newCaptureTestEngine(t)
	le.SetCaptureRestore([]CaptureChannelKey{{PeerName: "Live", ChannelName: "Main"}})

	// Same channel name, different publishing app: not the remembered channel.
	le.reconcileChannels([]abllink.Channel{testDiscoveredChannel(1, "Reaper", "Main")})

	if ch := captureEntry(t, le, testDiscoveredChannel(1, "Reaper", "Main")); ch.enabled {
		t.Fatal("restore key is (peer, channel): a different peer's same-named channel must not auto-enable")
	}
}

func TestSetCaptureEnabledUpdatesRestoreSet(t *testing.T) {
	le := newCaptureTestEngine(t)
	c := testDiscoveredChannel(1, "Live", "Main")
	le.reconcileChannels([]abllink.Channel{c})
	id := hex.EncodeToString(c.ID[:])

	le.SetCaptureEnabled(id, true)
	if !le.capture[id].enabled {
		t.Fatal("SetCaptureEnabled(true) did not enable the channel")
	}
	if !le.restoreSet()[CaptureChannelKey{PeerName: "Live", ChannelName: "Main"}] {
		t.Fatal("enabling a channel must add it to the restore set")
	}

	le.SetCaptureEnabled(id, false)
	if le.capture[id].enabled {
		t.Fatal("SetCaptureEnabled(false) did not disable the channel")
	}
	if le.restoreSet()[CaptureChannelKey{PeerName: "Live", ChannelName: "Main"}] {
		t.Fatal("disabling a channel must remove it from the restore set")
	}
}

func TestCaptureChannelsNeverSerializesOwnChannels(t *testing.T) {
	le := newCaptureTestEngine(t)
	// Learn a sink name as ours, then place a matching channel in the capture
	// map directly (as if the reconcile-time Own() filter leaked it through).
	le.own.Published("WAIL · stream 0")
	ownCh := testDiscoveredChannel(1, "WAIL", "WAIL · stream 0")
	otherCh := testDiscoveredChannel(2, "Live", "Main")
	le.capture[hex.EncodeToString(ownCh.ID[:])] = &captureChannel{id: ownCh.ID, name: ownCh.Name, peerName: ownCh.PeerName}
	le.capture[hex.EncodeToString(otherCh.ID[:])] = &captureChannel{id: otherCh.ID, name: otherCh.Name, peerName: otherCh.PeerName}

	infos := le.CaptureChannels()
	if len(infos) != 1 || infos[0].Name != "Main" {
		t.Fatalf("CaptureChannels must exclude own republished channels, got %+v", infos)
	}
}
