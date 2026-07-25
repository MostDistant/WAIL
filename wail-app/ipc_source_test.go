//go:build !linkstub

package main

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
)

func TestIPCCaptureSourcePushPopDrop(t *testing.T) {
	src := &ipcCaptureSource{nowMicros: func() int64 { return 0 }}
	// Fill 10 past capacity to force the oldest 10 blocks to drop.
	total := ipcCaptureRingBlocks + 10
	for i := 0; i < total; i++ {
		src.Push([]int16{int16(i)}, uint64(i)*48, 1, 48000)
	}
	if src.Dropped() != 10 {
		t.Fatalf("expected 10 drops, got %d", src.Dropped())
	}

	lb := NewLinkBridge(120, 4)
	ss := abllink.NewSessionState()
	defer ss.Close()
	lb.Link().CaptureAppSessionState(ss)

	got := 0
	var firstSeq uint64
	for {
		buf, _, _, ok := src.PopMapped(ss, 4.0)
		if !ok {
			break
		}
		if got == 0 {
			firstSeq = buf.Count
		}
		got++
	}
	if got != ipcCaptureRingBlocks {
		t.Fatalf("retained %d blocks, want %d", got, ipcCaptureRingBlocks)
	}
	// pushSeq is 1-based; after dropping the first 10, the oldest survivor is seq 11.
	if firstSeq != 11 {
		t.Fatalf("first retained seq = %d, want 11", firstSeq)
	}
}

func TestIPCCaptureSourceBeatAnchoring(t *testing.T) {
	lb := NewLinkBridge(120, 4)
	ss := abllink.NewSessionState()
	defer ss.Close()
	lb.Link().CaptureAppSessionState(ss)

	src := &ipcCaptureSource{nowMicros: func() int64 { return 5_000_000 }}
	// Two blocks exactly one second apart (48000 frames @ 48 kHz).
	src.Push([]int16{0, 0}, 1000, 2, 48000)
	src.Push([]int16{0, 0}, 1000+48000, 2, 48000)

	_, beat1, ok1, _ := src.PopMapped(ss, 4.0)
	_, beat2, ok2, _ := src.PopMapped(ss, 4.0)
	if !ok1 || !ok2 {
		t.Fatal("expected both blocks to map")
	}
	// One second at 120 BPM advances 2 beats, regardless of the anchor's offset.
	if d := beat2 - beat1; math.Abs(d-2.0) > 1e-6 {
		t.Fatalf("beat advance = %f, want 2.0", d)
	}
}

func TestIPCCaptureSourceStampIncludesOutputLatencyLead(t *testing.T) {
	// The block being captured now reaches the DAW's DAC one output pipeline
	// later — that DAC time is the audio's true grid time, so stamps run ahead
	// of the capture clock by the lead (same fix as the Link Bridge stamp-ahead).
	lb := NewLinkBridge(120, 4)
	ss := abllink.NewSessionState()
	defer ss.Close()
	lb.Link().CaptureAppSessionState(ss)

	src := &ipcCaptureSource{nowMicros: func() int64 { return 5_000_000 }, leadUs: 10_000}
	src.Push([]int16{0, 0}, 1000, 2, 48000)
	_, beat, ok, _ := src.PopMapped(ss, 4.0)
	if !ok {
		t.Fatal("expected block to map")
	}
	want := ss.BeatAtTime(5_000_000+10_000, 4.0)
	if math.Abs(beat-want) > 1e-9 {
		t.Fatalf("stamp beat = %f, want %f (10ms lead)", beat, want)
	}
}

func TestResolveIPCSendLeadUs(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want int64
	}{
		{"unset uses default", "", false, 10_000},
		{"valid override", "5", true, 5_000},
		{"zero allowed", "0", true, 0},
		{"above ceiling clamps", "500", true, 50_000},
		{"unparseable falls back", "abc", true, 10_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("WAIL_IPC_SEND_LEAD_MS", tc.env)
			}
			if got := resolveIPCSendLeadUs(); got != tc.want {
				t.Fatalf("resolveIPCSendLeadUs() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRawPCMToInt16(t *testing.T) {
	// int16 payload passes through unchanged.
	in16 := []int16{0, 100, -100, 32767, -32768}
	buf := make([]byte, 2*len(in16))
	for i, s := range in16 {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(s))
	}
	if got := rawPCMToInt16(IPCRawFlagInt16, buf); !slices.Equal(got, in16) {
		t.Fatalf("int16 passthrough = %v", got)
	}

	// float32 payload is scaled to int16 and clamped.
	fs := []float32{0, 1.0, -1.0, 2.0, -2.0, 0.5}
	fbuf := make([]byte, 4*len(fs))
	for i, f := range fs {
		binary.LittleEndian.PutUint32(fbuf[4*i:], math.Float32bits(f))
	}
	// ×32767 scaling: -1.0 → -32767; only out-of-range (-2.0) clamps to -32768.
	want := []int16{0, 32767, -32767, 32767, -32768, 16383}
	if got := rawPCMToInt16(0, fbuf); !slices.Equal(got, want) {
		t.Fatalf("float32 conversion = %v, want %v", got, want)
	}
}

func TestIPCCaptureSourceStaleRingReanchors(t *testing.T) {
	lb := NewLinkBridge(120, 4)
	ss := abllink.NewSessionState()
	defer ss.Close()
	lb.Link().CaptureAppSessionState(ss)

	// The plugin's send ring sat blocked through an 11-minute WAIL outage
	// (wail_send.c re-sends the stale slots on reconnect). The stale slots
	// (begin_frame ≈ 0) flush first and arm the anchor...
	var nowUs int64 = 100_000_000
	src := &ipcCaptureSource{nowMicros: func() int64 { return nowUs }}
	src.Push([]int16{0, 0}, 0, 2, 48000)     // stale slot 1 (11 min old)
	src.Push([]int16{0, 0}, 48000, 2, 48000) // stale slot 2
	if _, _, ok, _ := src.PopMapped(ss, 4.0); !ok {
		t.Fatal("stale slot 1 must map")
	}
	_, beat2, ok2, _ := src.PopMapped(ss, 4.0)
	if !ok2 {
		t.Fatal("stale slot 2 must map")
	}

	// ...then the CURRENT block arrives, 31.7M frames (11 min) later. The
	// anchor must re-arm on the implausible forward jump; extrapolating from
	// the stale anchor would stamp this 660s in the future (+82 intervals —
	// the field bug), frozen for the session.
	nowUs += 10_000_000 // 10s of playback since the outage
	src.Push([]int16{0, 0}, 31_700_000, 2, 48000)
	_, beat, ok, _ := src.PopMapped(ss, 4.0)
	if !ok {
		t.Fatal("current block must map")
	}
	// Re-armed: the stamp advanced from the stale slot's 101s to 110s of
	// real time (18 beats). Poisoned: it would jump ~1319 beats (660s).
	if d := beat - beat2; math.Abs(d-18.0) > 1.0 {
		t.Fatalf("stamp jump from stale ring = %.1f beats, want ~18 (re-armed) — poisoned anchor would give ~1319", d)
	}

	// And a small forward skew (≤ the threshold) must NOT re-arm: normal
	// backlog/jitter keeps the established anchor.
	src.Push([]int16{0, 0}, 31_700_000+48000, 2, 48000)
	_, beat3, ok3, _ := src.PopMapped(ss, 4.0)
	if !ok3 {
		t.Fatal("follow-up block must map")
	}
	if d := beat3 - beat; math.Abs(d-2.0) > 1e-6 {
		t.Fatalf("beat advance after re-arm = %f, want 2.0 (anchor kept)", d)
	}
}
