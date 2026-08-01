//go:build !linkstub

package main

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
	"github.com/nicholasgasior/wail/wail-app/internal/affinity"
	"github.com/nicholasgasior/wail/wail-app/internal/capture"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
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

func TestCaptureExcludesWAILPrefixedChannelsFromAnyPeer(t *testing.T) {
	le := newCaptureTestEngine(t)
	// A "WAIL · " room channel published by a DIFFERENT peer (not ours, and not a
	// name we minted) must still be kept out of the capture mixer: it carries
	// already-relayed room audio, so capturing it would re-relay the room. This
	// exercises the name-prefix filter, distinct from the own-ID/own-name filter.
	roomCh := testDiscoveredChannel(1, "SomeoneElse", "WAIL · Bob · guitar")
	plainCh := testDiscoveredChannel(2, "Live", "Main")
	le.reconcileChannels([]abllink.Channel{roomCh, plainCh})

	infos := le.CaptureChannels()
	if len(infos) != 1 || infos[0].Name != "Main" {
		t.Fatalf("capture mixer must exclude WAIL · room channels from any peer, got %+v", infos)
	}
}

// A room interval-config change must re-grid running capture assemblers: they
// are built once at channel start, and a stale grid makes a sender's room
// labels tick at the old rate — receivers hear that peer drift out of sync
// with everyone else, worse every interval, until the channel is restarted.
func TestSyncCaptureConfigReGridsAssemblerOnRoomConfigChange(t *testing.T) {
	le := newCaptureTestEngine(t)
	le.SetRoomAnchor(0, 120, 4, 4)
	ch := &captureChannel{
		name: "Main",
		asm:  capture.NewWindowed(interval.Config{Bars: 4, Quantum: 4}, 2, engineInternalRate, samplesPerWaifFrame(engineInternalRate)),
	}
	le.capture["deadbeef"] = ch

	// Room switches 4 bars → 2 bars. SetRoomAnchor adopts it engine-wide; the
	// drain goroutine follows on its next tick (syncCaptureConfig).
	le.SetRoomAnchor(1, 120, 2, 4)
	le.syncCaptureConfig(ch)

	if got := ch.asm.Config(); got != (interval.Config{Bars: 2, Quantum: 4}) {
		t.Fatalf("assembler still on old grid after room config change: %+v", got)
	}
}

// --- stream retirement ---

const retireTestInterval = int64(5)

// feedRemoteStream publishes one remote stream by pushing a real one-interval
// WAIF stream through the ingestion path, exactly as the relay would.
func feedRemoteStream(t *testing.T, le *linkAudioEngine, identity, display, streamName string, streamID uint16) {
	t.Helper()
	feedRemoteStreamAt(t, le, identity, display, streamName, streamID, retireTestInterval)
}

// feedRemoteStreamAt is feedRemoteStream with an explicit room interval index,
// for exercising senders whose labels sit far from local playout.
func feedRemoteStreamAt(t *testing.T, le *linkAudioEngine, identity, display, streamName string, streamID uint16, roomIdx int64) {
	t.Helper()
	enc, err := NewIntervalEncoder(2, engineInternalRate, 128)
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	pcm := make([]int16, 960*2*2) // 2 stereo 20ms frames
	frames, _, err := enc.EncodeInterval(pcm, roomIdx, streamID, 0, 120, 4, 4)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, f := range frames {
		le.HandleRemoteAudio(identity, display, streamName, f)
	}
}

// drainPlayout simulates the interval having finished playing out, which the
// boundary handler does via reasm.Drop once the release window passes.
func drainPlayout(le *linkAudioEngine) {
	le.mu.Lock()
	defer le.mu.Unlock()
	for _, st := range le.emit {
		st.reasm.Drop(retireTestInterval)
	}
}

func sweep(le *linkAudioEngine, at time.Time) {
	le.mu.Lock()
	defer le.mu.Unlock()
	le.sweepRetiredLocked(at)
}

func hasStream(le *linkAudioEngine, identity string, streamID uint16) bool {
	le.mu.Lock()
	defer le.mu.Unlock()
	_, ok := le.emit[affinity.Key{Identity: identity, Stream: streamID}]
	return ok
}

// A stream its sender no longer lists in StreamNames must stop being published:
// e.emit only ever grew within a session, so a dropped stream's channel kept
// publishing silence on the LAN and held a port on every WAIL Receive.
func TestEmitStreamRetiredWhenSenderDropsIt(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStream(t, le, "id-A", "Alice", "guitar", 0)
	feedRemoteStream(t, le, "id-A", "Alice", "bass", 1)
	if len(le.emit) != 2 {
		t.Fatalf("expected 2 published streams, got %d", len(le.emit))
	}

	// Alice now sends only stream 0.
	le.SetPeerStreams("id-A", map[uint16]bool{0: true})
	past := time.Now().Add(24 * time.Hour)

	// Still buffered: retiring here would cut the tail off the last interval,
	// which playout is still holding (release runs D boundaries behind).
	sweep(le, past)
	if !hasStream(le, "id-A", 1) {
		t.Fatal("retired a stream that still had audio buffered — that truncates the last interval")
	}

	drainPlayout(le)

	// Inside the grace, frames could still be in flight.
	sweep(le, time.Now())
	if !hasStream(le, "id-A", 1) {
		t.Fatal("retired inside the grace period")
	}

	sweep(le, time.Now().Add(retireGraceDropped+time.Second))
	if hasStream(le, "id-A", 1) {
		t.Fatal("dropped stream still published after its grace expired")
	}
	if !hasStream(le, "id-A", 0) {
		t.Fatal("retired a stream the sender still lists")
	}
}

// An empty declared set means "I send nothing" — the case that reaches us with
// the wire field absent (Names is omitempty), which used to be discarded.
func TestEmitStreamsRetiredWhenSenderDeclaresNone(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStream(t, le, "id-A", "Alice", "guitar", 0)
	le.SetPeerStreams("id-A", map[uint16]bool{})
	drainPlayout(le)
	sweep(le, time.Now().Add(retireGraceDropped+time.Second))
	if len(le.emit) != 0 {
		t.Fatalf("expected every stream retired, %d still published", len(le.emit))
	}
}

// A peer that left gets a much longer grace than one that dropped a stream:
// affinity exists so a reconnect blip keeps the same channel and the far
// side's routing survives.
func TestDepartedPeerKeepsChannelsThroughLongerGrace(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStream(t, le, "id-A", "Alice", "guitar", 0)
	le.DropPeer("id-A")
	drainPlayout(le)

	sweep(le, time.Now().Add(retireGraceDropped+time.Second))
	if !hasStream(le, "id-A", 0) {
		t.Fatal("a departed peer's channel went away on the short grace — a brief reconnect loses routing")
	}
	sweep(le, time.Now().Add(retireGracePeerGone+time.Second))
	if hasStream(le, "id-A", 0) {
		t.Fatal("departed peer's channel still published after the peer-gone grace")
	}
}

// Rejoining inside the grace revives the channel rather than minting a new one.
func TestRejoinInsideGraceKeepsTheSameChannel(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStream(t, le, "id-A", "Alice", "guitar", 0)
	before := le.emit[affinity.Key{Identity: "id-A", Stream: 0}].sinks[0]

	le.DropPeer("id-A")
	le.SetPeerStreams("id-A", map[uint16]bool{0: true}) // rejoined, still sending stream 0
	drainPlayout(le)
	sweep(le, time.Now().Add(24*time.Hour))

	st, ok := le.emit[affinity.Key{Identity: "id-A", Stream: 0}]
	if !ok {
		t.Fatal("channel retired despite the peer rejoining and re-declaring the stream")
	}
	if st.sinks[0] != before {
		t.Fatal("re-minted the sink instead of keeping it (affinity)")
	}
}

// Silence from a peer we have heard nothing about is not evidence: with no
// declared intent the stream stays, however long it has been idle.
func TestStreamWithoutDeclaredIntentIsNeverRetired(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStream(t, le, "id-A", "Alice", "guitar", 0)
	drainPlayout(le)
	sweep(le, time.Now().Add(24*time.Hour))
	if !hasStream(le, "id-A", 0) {
		t.Fatal("retired a stream with no declared intent")
	}
}

// Retirement must not lower the cumulative Health totals: the session logs a
// counter only when it exceeds the previous snapshot, so a dip would silently
// swallow every later event on the surviving streams until it recovered.
func TestRetirementKeepsHealthTotalsMonotonic(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStream(t, le, "id-A", "Alice", "guitar", 0)
	le.emit[affinity.Key{Identity: "id-A", Stream: 0}].framesMissedAtPlay.Add(7)
	if got := le.Health().EmitFramesMissingAtPlay; got != 7 {
		t.Fatalf("pre-retirement total = %d, want 7", got)
	}

	le.SetPeerStreams("id-A", map[uint16]bool{})
	drainPlayout(le)
	sweep(le, time.Now().Add(retireGraceDropped+time.Second))

	if got := le.Health().EmitFramesMissingAtPlay; got != 7 {
		t.Fatalf("total dropped to %d after retiring the stream, want 7 retained", got)
	}
}

// Frames queued behind a PeerLeft publish under the peer-id fallback (the
// registry entry is gone by the time they are handled), so retirement has to
// cover that key too — otherwise the departure leaves a channel keyed by
// something nothing will ever declare again, published for the session.
func TestPeerIdKeyedStreamsAreRetirable(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStream(t, le, "peer-id-P", "", "", 0) // no Hello yet / peer already removed
	le.DropPeer("peer-id-P")
	drainPlayout(le)
	sweep(le, time.Now().Add(retireGracePeerGone+time.Second))
	if hasStream(le, "peer-id-P", 0) {
		t.Fatal("a stream published under the peer-id fallback never retires")
	}
}

// DropPeer on a source that will never send StreamNames (the loopback echo)
// must be undoable, or re-enabling it leaves the drop in force and the monitor
// channels are retired the next time frames pause.
func TestClearPeerIntentMakesStreamsWantedAgain(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStream(t, le, "id-A:loopback", "Me (loopback)", "guitar", 0)
	le.DropPeer("id-A:loopback")
	le.ClearPeerIntent("id-A:loopback")
	drainPlayout(le)
	sweep(le, time.Now().Add(24*time.Hour))
	if !hasStream(le, "id-A:loopback", 0) {
		t.Fatal("cleared intent still retired the stream")
	}
}

// A sender whose room labels run far ahead of our playout leaves stragglers
// buffered beyond the playout horizon. Drop only clears at or below the release
// cursor, so requiring an empty reassembler kept a dead channel published for
// as many boundaries as that peer was mislabeled — unbounded in practice.
func TestFarAheadStragglerDoesNotBlockRetirement(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStreamAt(t, le, "id-A", "Alice", "guitar", 0, 5000)
	st := le.emit[affinity.Key{Identity: "id-A", Stream: 0}]
	if st == nil {
		t.Fatal("stream not published")
	}
	st.sched.OnBoundary(3) // local playout is way behind the sender's labels

	le.SetPeerStreams("id-A", map[uint16]bool{})
	sweep(le, time.Now().Add(retireGraceDropped+time.Second))

	if hasStream(le, "id-A", 0) {
		t.Fatal("a straggler beyond the playout horizon still blocks retirement")
	}
}

// The other side of that horizon: audio due imminently — including the interval
// that just played, which playout drops only a boundary later — must still hold
// the channel open, or retirement truncates it.
func TestImminentAudioStillBlocksRetirement(t *testing.T) {
	le := newCaptureTestEngine(t)
	feedRemoteStreamAt(t, le, "id-A", "Alice", "guitar", 0, 5)
	st := le.emit[affinity.Key{Identity: "id-A", Stream: 0}]
	if st == nil {
		t.Fatal("stream not published")
	}
	// playing = 3-D = 2, so interval 5 sits exactly on the horizon (D+2).
	st.sched.OnBoundary(3)

	le.SetPeerStreams("id-A", map[uint16]bool{})
	sweep(le, time.Now().Add(24*time.Hour))

	if !hasStream(le, "id-A", 0) {
		t.Fatal("retired a stream with audio still due to play")
	}
}
