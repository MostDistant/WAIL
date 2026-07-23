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
