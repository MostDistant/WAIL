package main

import (
	"math"
	"testing"
)

func TestFramesPerInterval(t *testing.T) {
	// 120 BPM, 4 bars, quantum 4 → 16 beats → 8 seconds → 400 frames
	fps := FramesPerInterval(120.0, 4, 4.0)
	if fps != 400 {
		t.Fatalf("expected 400, got %d", fps)
	}

	// 60 BPM, 4 bars, quantum 4 → 16 seconds → 800 frames
	fps = FramesPerInterval(60.0, 4, 4.0)
	if fps != 800 {
		t.Fatalf("expected 800, got %d", fps)
	}

	// 0 BPM should return 0 (guard against division by zero)
	fps = FramesPerInterval(0.0, 4, 4.0)
	if fps != 0 {
		t.Fatalf("expected 0 for zero BPM, got %d", fps)
	}
}

func TestGenerateSineFrame(t *testing.T) {
	var phase float64
	samples := GenerateSineFrame(440.0, &phase, 48000, 2)

	// 960 samples per channel * 2 channels = 1920
	if len(samples) != 1920 {
		t.Fatalf("expected 1920 samples, got %d", len(samples))
	}

	// Should have non-zero values (not silence)
	var maxAbs int16
	for _, s := range samples {
		if s > maxAbs {
			maxAbs = s
		}
		if -s > maxAbs {
			maxAbs = -s
		}
	}
	if maxAbs == 0 {
		t.Fatal("all samples are zero — expected non-silent sine wave")
	}

	// Phase should have advanced
	if phase == 0 {
		t.Fatal("phase should have advanced")
	}
}

func TestGenerateSineFramePhaseContinuity(t *testing.T) {
	var phase float64
	GenerateSineFrame(440.0, &phase, 48000, 2)
	phase1 := phase

	GenerateSineFrame(440.0, &phase, 48000, 2)
	phase2 := phase

	// Phase should keep advancing
	if math.Abs(phase2-phase1) < 0.1 {
		t.Fatal("phase should advance between frames")
	}
}

func TestToneFrequency(t *testing.T) {
	// Intervals 0-3 → 440Hz
	for i := int64(0); i < 4; i++ {
		if toneFrequency(i) != 440.0 {
			t.Fatalf("expected 440 for interval %d", i)
		}
	}
	// Intervals 4-7 → 880Hz
	for i := int64(4); i < 8; i++ {
		if toneFrequency(i) != 880.0 {
			t.Fatalf("expected 880 for interval %d", i)
		}
	}
	// Intervals 8-11 → 440Hz again
	for i := int64(8); i < 12; i++ {
		if toneFrequency(i) != 440.0 {
			t.Fatalf("expected 440 for interval %d", i)
		}
	}
}
