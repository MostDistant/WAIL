package main

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

func TestEncodeDecodeFrameRoundTrip(t *testing.T) {
	payload := []byte("hello wail ipc")
	frame := EncodeFrame(payload)
	got, consumed, ok := DecodeFrame(frame)
	if !ok {
		t.Fatal("DecodeFrame: ok=false on a complete frame")
	}
	if consumed != len(frame) {
		t.Fatalf("consumed=%d want %d", consumed, len(frame))
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload round-trip mismatch: %q", got)
	}
	// A frame missing its tail is "need more data", not an error.
	if _, _, ok := DecodeFrame(frame[:len(frame)-1]); ok {
		t.Fatal("DecodeFrame: ok=true on a truncated frame")
	}
	if _, _, ok := DecodeFrame([]byte{0x01, 0x02}); ok {
		t.Fatal("DecodeFrame: ok=true on a sub-header buffer")
	}
}

func TestRawPCMRoundTrip(t *testing.T) {
	pcm := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11}
	msg := EncodeRawPCM(3, IPCRawFlagInt16|IPCRawFlagPlaying, 2, 48000, 1234567, pcm)
	if IPCTag(msg) != int(IPCTagRawPCM) {
		t.Fatalf("tag = %d", IPCTag(msg))
	}
	got, ok := DecodeRawPCM(msg)
	if !ok {
		t.Fatal("DecodeRawPCM: ok=false")
	}
	if got.StreamIndex != 3 || got.Channels != 2 || got.SampleRate != 48000 || got.FrameCounter != 1234567 {
		t.Fatalf("header mismatch: %+v", got)
	}
	if got.Flags&IPCRawFlagInt16 == 0 || got.Flags&IPCRawFlagPlaying == 0 {
		t.Fatalf("flags mismatch: %#x", got.Flags)
	}
	if !bytes.Equal(got.Samples, pcm) {
		t.Fatalf("pcm mismatch: %v", got.Samples)
	}
	// Empty PCM (a final/keepalive block) must still decode.
	if _, ok := DecodeRawPCM(EncodeRawPCM(0, 0, 1, 44100, 0, nil)); !ok {
		t.Fatal("DecodeRawPCM: ok=false on empty pcm")
	}
	// A frame shorter than the fixed header is rejected.
	if _, ok := DecodeRawPCM([]byte{IPCTagRawPCM, 0x00}); ok {
		t.Fatal("DecodeRawPCM: ok=true on short header")
	}
}

func TestRemotePCMRoundTrip(t *testing.T) {
	samples := []int16{0, 1, -1, 32767, -32768, 12345, -12345}
	// The 8-byte field is the monotonic-µs time the first frame should play
	// (machine clock shared with the plugin), so large positive values.
	msg := EncodeRemotePCM("peer-XY", 7, 2, 48000, 1_759_000_123_456, samples)
	got, ok := DecodeRemotePCM(msg)
	if !ok {
		t.Fatal("DecodeRemotePCM: ok=false")
	}
	if got.PeerID != "peer-XY" || got.StreamID != 7 || got.Channels != 2 ||
		got.SampleRate != 48000 || got.PlayAtMicros != 1_759_000_123_456 {
		t.Fatalf("header mismatch: %+v", got)
	}
	if !slices.Equal(got.Samples, samples) {
		t.Fatalf("samples mismatch: %v", got.Samples)
	}
	// Odd trailing byte (not a whole number of int16) is malformed.
	bad := EncodeRemotePCM("p", 1, 1, 48000, 0, []int16{5})
	bad = append(bad, 0x99)
	if _, ok := DecodeRemotePCM(bad); ok {
		t.Fatal("DecodeRemotePCM: ok=true on odd-length pcm")
	}
	// Truncated header (claims a peer_id longer than the buffer) is rejected.
	if _, ok := DecodeRemotePCM([]byte{IPCTagRemotePCM, 0x05, 'a'}); ok {
		t.Fatal("DecodeRemotePCM: ok=true on truncated peer_id")
	}
}

func TestStreamNameAndGoneRoundTrip(t *testing.T) {
	msg := EncodeStreamName("peerA", 2, "Alice · guitar")
	pid, sid, name, ok := DecodeStreamName(msg)
	if !ok || pid != "peerA" || sid != 2 || name != "Alice · guitar" {
		t.Fatalf("StreamName round-trip: ok=%v pid=%q sid=%d name=%q", ok, pid, sid, name)
	}
	gmsg := EncodeStreamGone("peerA", 2)
	pid, sid, ok = DecodeStreamGone(gmsg)
	if !ok || pid != "peerA" || sid != 2 {
		t.Fatalf("StreamGone round-trip: ok=%v pid=%q sid=%d", ok, pid, sid)
	}
	// Cross-tag decode must fail (a StreamGone is not a StreamName).
	if _, _, _, ok := DecodeStreamName(gmsg); ok {
		t.Fatal("DecodeStreamName accepted a StreamGone frame")
	}
}

func TestTrackNameRoundTrip(t *testing.T) {
	msg := EncodeTrackName(3, "Bass DI")
	idx, name, ok := DecodeTrackName(msg)
	if !ok || idx != 3 || name != "Bass DI" {
		t.Fatalf("TrackName round-trip: ok=%v idx=%d name=%q", ok, idx, name)
	}
	// Empty name (host without track-info must not send these, but decode anyway).
	if idx, name, ok := DecodeTrackName(EncodeTrackName(0, "")); !ok || idx != 0 || name != "" {
		t.Fatalf("TrackName empty-name round-trip: ok=%v idx=%d name=%q", ok, idx, name)
	}
	// Cross-tag decode must fail (a RawPCM is not a TrackName).
	if _, _, ok := DecodeTrackName(EncodeRawPCM(0, 0, 2, 48000, 0, nil)); ok {
		t.Fatal("DecodeTrackName accepted a RawPCM frame")
	}
	// Truncated frames must fail.
	if _, _, ok := DecodeTrackName([]byte{IPCTagTrackName, 0x03}); ok {
		t.Fatal("DecodeTrackName: ok=true on short frame")
	}
	trunc := EncodeTrackName(3, "Bass DI")
	if _, _, ok := DecodeTrackName(trunc[:len(trunc)-2]); ok {
		t.Fatal("DecodeTrackName: ok=true on truncated name")
	}
}

func TestMetricsRoundTrip(t *testing.T) {
	got, ok := DecodeMetrics(EncodeMetrics(987654321))
	if !ok || got != 987654321 {
		t.Fatalf("Metrics round-trip: ok=%v got=%d", ok, got)
	}
	if _, ok := DecodeMetrics([]byte{IPCTagMetrics, 0x00}); ok {
		t.Fatal("DecodeMetrics: ok=true on short frame")
	}
}

func TestRecvBufferPartialAndMultiFrame(t *testing.T) {
	f1 := EncodeFrame(EncodeStreamGone("a", 1))
	f2 := EncodeFrame(EncodeRawPCM(9, 0, 2, 48000, 5, []byte{1, 2, 3, 4}))
	stream := slices.Concat(f1, f2)

	// Byte-at-a-time delivery must reassemble both frames in order.
	rb := NewIPCRecvBuffer()
	var frames [][]byte
	for _, b := range stream {
		rb.Push([]byte{b})
		for {
			f, err := rb.NextFrame()
			if err != nil {
				t.Fatalf("NextFrame error: %v", err)
			}
			if f == nil {
				break
			}
			frames = append(frames, f)
		}
	}
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(frames))
	}
	if IPCTag(frames[0]) != int(IPCTagStreamGone) || IPCTag(frames[1]) != int(IPCTagRawPCM) {
		t.Fatalf("frame order/tags wrong: %d, %d", IPCTag(frames[0]), IPCTag(frames[1]))
	}
	if rb.Buffered() != 0 {
		t.Fatalf("buffer not drained: %d bytes left", rb.Buffered())
	}

	// Both frames in a single Push must both come out.
	rb2 := NewIPCRecvBuffer()
	rb2.Push(stream)
	n := 0
	for {
		f, err := rb2.NextFrame()
		if err != nil {
			t.Fatalf("NextFrame error: %v", err)
		}
		if f == nil {
			break
		}
		n++
	}
	if n != 2 {
		t.Fatalf("single-push: expected 2 frames, got %d", n)
	}
}

func TestRecvBufferRejectsOversizeFrame(t *testing.T) {
	// A length prefix past the cap is unrecoverable: NextFrame must error before
	// trying to allocate/wait for the (bogus) payload.
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, uint32(maxIPCFrameSize+1))
	rb := NewIPCRecvBuffer()
	rb.Push(hdr)
	if _, err := rb.NextFrame(); err == nil {
		t.Fatal("NextFrame: expected an error on an oversize frame length")
	}
	// A frame exactly at the cap is allowed (prefix check is >, not >=).
	hdr2 := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr2, uint32(maxIPCFrameSize))
	rb2 := NewIPCRecvBuffer()
	rb2.Push(hdr2)
	if _, err := rb2.NextFrame(); err != nil {
		t.Fatalf("NextFrame: unexpected error at exactly the cap: %v", err)
	}
}
