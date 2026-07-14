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
		eng.HandleRemoteAudio("identity-A", "Alice", f)
	}

	if len(le.emit) != 1 {
		t.Fatalf("expected 1 emit stream, got %d", len(le.emit))
	}
	if le.sinks.Len() != 1 {
		t.Fatalf("expected 1 published sink, got %d", le.sinks.Len())
	}

	// A malformed frame must be ignored without creating a stream or panicking.
	eng.HandleRemoteAudio("identity-B", "Bob", []byte{'x', 'y', 'z'})
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

	// Stop without Start must be safe and must tear down the sink.
	eng.Stop()
	if le.sinks.Len() != 0 {
		t.Fatalf("Stop should remove sinks, still have %d", le.sinks.Len())
	}
}
