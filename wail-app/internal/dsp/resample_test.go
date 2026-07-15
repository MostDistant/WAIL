package dsp

import "testing"

func TestResampleIdentity(t *testing.T) {
	src := []int16{1, 2, 3, 4, 5, 6}
	got := ResampleLinearInterleaved(src, 2, 48000, 48000)
	if len(got) != len(src) {
		t.Fatalf("identity changed length: got %d want %d", len(got), len(src))
	}
	// Equal rates must return the same backing slice (no copy/work).
	if &got[0] != &src[0] {
		t.Fatal("identity should return src unchanged")
	}
}

func TestResampleDegenerate(t *testing.T) {
	if got := ResampleLinearInterleaved(nil, 2, 44100, 48000); got != nil {
		t.Fatalf("empty input should pass through, got %v", got)
	}
	src := []int16{7, 8}
	if got := ResampleLinearInterleaved(src, 1, 0, 48000); &got[0] != &src[0] {
		t.Fatal("non-positive srcRate should pass through")
	}
}

func TestResampleDownHalvesFrameCount(t *testing.T) {
	// 8 stereo frames at 48k → 24k should yield 4 stereo frames.
	src := make([]int16, 8*2)
	for i := range src {
		src[i] = int16(i)
	}
	got := ResampleLinearInterleaved(src, 2, 48000, 24000)
	if len(got) != 4*2 {
		t.Fatalf("downsample length: got %d want %d", len(got), 4*2)
	}
	// First frame is copied exactly (pos 0).
	if got[0] != src[0] || got[1] != src[1] {
		t.Fatalf("first frame not preserved: got (%d,%d)", got[0], got[1])
	}
}

func TestResampleUpMono(t *testing.T) {
	src := []int16{0, 100} // 2 mono frames
	got := ResampleLinearInterleaved(src, 1, 24000, 48000)
	if len(got) != 4 {
		t.Fatalf("upsample length: got %d want 4", len(got))
	}
	// Midpoint (pos 0.5 between 0 and 100) should interpolate to ~50.
	if got[1] < 40 || got[1] > 60 {
		t.Fatalf("interpolated sample off: got %d want ~50", got[1])
	}
}
