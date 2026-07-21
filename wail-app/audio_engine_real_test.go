//go:build !linkstub

package main

import "testing"

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
	if st.sink == nil {
		t.Fatal("expected the stream to publish a Link Audio sink")
	}

	// Reconnect: same identity+stream, renamed peer → reuse the same stream and
	// channel (affinity), just refresh the name. Must not mint a new stream/sink.
	firstSink := st.sink
	eng.HandleRemoteAudio("identity-A", "Alice (reconnected)", "guitar", frames[0])
	if len(le.emit) != 1 {
		t.Fatalf("reconnect minted a new stream: %d", len(le.emit))
	}
	if st.sink != firstSink {
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
