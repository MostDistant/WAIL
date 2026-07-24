package main

import (
	"encoding/binary"
	"testing"
	"time"
)

func waifFrame(label int64) []byte {
	b := make([]byte, 25)
	copy(b, "WAIF")
	binary.LittleEndian.PutUint64(b[7:15], uint64(label))
	return b
}

func TestWaifIntervalIndex(t *testing.T) {
	if got, ok := waifIntervalIndex(waifFrame(42)); !ok || got != 42 {
		t.Fatalf("waifIntervalIndex = %d, %v; want 42, true", got, ok)
	}
	if _, ok := waifIntervalIndex([]byte("not a waif frame")); ok {
		t.Fatal("non-WAIF data must not parse")
	}
	if _, ok := waifIntervalIndex(waifFrame(1)[:10]); ok {
		t.Fatal("truncated frame must not parse")
	}
}

func TestWatchdogTriggersOnSustainedMismatch(t *testing.T) {
	w := newLabelWatchdog()
	sent := 0
	now := time.Now()
	send := func() { sent++ }
	// Just over the threshold (k=+4): no action before the sustain count.
	for i := 0; i < watchSustainFrames-1; i++ {
		w.observe("r", "p1", 19, 15, now, send)
	}
	if sent != 0 {
		t.Fatal("anchor sent before sustain threshold")
	}
	w.observe("r", "p1", 19, 15, now, send)
	if sent != 1 {
		t.Fatalf("anchor not sent at sustain threshold, sent=%d", sent)
	}
	// Still misaligned, but cooldown suppresses an immediate re-send.
	for i := 0; i < watchSustainFrames; i++ {
		w.observe("r", "p1", 19, 15, now.Add(500*time.Millisecond), send)
	}
	if sent != 1 {
		t.Fatalf("cooldown violated: sent=%d", sent)
	}
	// After the cooldown, a still-offending peer gets another anchor.
	for i := 0; i < watchSustainFrames; i++ {
		w.observe("r", "p1", 19, 15, now.Add(watchCooldown+time.Second), send)
	}
	if sent != 2 {
		t.Fatalf("no re-send after cooldown: sent=%d", sent)
	}
}

func TestWatchdogIgnoresStraddleAndHeals(t *testing.T) {
	w := newLabelWatchdog()
	sent := 0
	now := time.Now()
	send := func() { sent++ }
	// ±2 is normal boundary straddle — never triggers, even CONSECUTIVELY
	// (pins the threshold: off-by-one here = flapping peers get anchor-spammed).
	for i := 0; i < 2*watchSustainFrames; i++ {
		w.observe("r", "p1", 17, 15, now, send) // k = +2
		w.observe("r", "p1", 13, 15, now, send) // k = −2
	}
	if sent != 0 {
		t.Fatal("boundary straddle triggered the watchdog")
	}
	// A flagged peer that returns to alignment resets (and can re-trigger later).
	for i := 0; i < watchSustainFrames; i++ {
		w.observe("r", "p1", 20, 15, now, send)
	}
	if sent != 1 {
		t.Fatal("expected trigger")
	}
	for i := 0; i < 10; i++ {
		w.observe("r", "p1", 15, 15, now, send)
	}
	for i := 0; i < watchSustainFrames; i++ {
		w.observe("r", "p1", 20, 15, now.Add(watchCooldown+time.Second), send)
	}
	if sent != 2 {
		t.Fatalf("healed peer did not re-trigger cleanly: sent=%d", sent)
	}
}

func TestWatchdogForgetsDepartedPeers(t *testing.T) {
	w := newLabelWatchdog()
	sent := 0
	now := time.Now()
	send := func() { sent++ }
	for i := 0; i < watchSustainFrames/2; i++ {
		w.observe("r", "p1", 20, 15, now, send)
	}
	w.forget("p1")
	// A rejoining peer starts fresh — no stale sustain count.
	for i := 0; i < watchSustainFrames/2; i++ {
		w.observe("r", "p1", 20, 15, now, send)
	}
	if sent != 0 {
		t.Fatal("forgotten peer's sustain count persisted")
	}
}
