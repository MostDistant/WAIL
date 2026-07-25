package emit

import "testing"

// ShiftedPCM realigns a codec-delayed stream by a fixed lookahead: interval
// N's content from shiftFrames on, then the next interval's head (silence
// until it arrives). The Opus codec delays every stream by OPUS_GET_LOOKAHEAD;
// reading the reassembly back shifted puts transients on the room grid.
func TestShiftedPCMRealignsAcrossIntervals(t *testing.T) {
	const (
		ch      = 1
		spf     = 4 // tiny frames for a tiny test
		shift   = 2
		frames1 = 3 // interval 1: frames 0..2 → 12 samples
	)
	r := New(ch, spf)
	mk := func(start int16, n int) []int16 {
		p := make([]int16, n)
		for i := range p {
			p[i] = start + int16(i)
		}
		return p
	}
	// Interval 1: samples 100..111; interval 2: samples 200..211.
	for fn := 0; fn < frames1; fn++ {
		r.Add(1, fn, mk(int16(100+fn*spf), spf), fn == frames1-1, frames1)
		r.Add(2, fn, mk(int16(200+fn*spf), spf), fn == frames1-1, frames1)
	}

	dst := make([]int16, frames1*spf)
	got := r.ShiftedPCM(dst, 1, 2, shift)
	// Want: interval 1's content from sample 2 on, then interval 2's head.
	want := append(mk(102, frames1*spf-shift), mk(200, shift)...)
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestShiftedPCMMissingNextIntervalPadsSilence(t *testing.T) {
	r := New(1, 4)
	r.Add(1, 0, []int16{1, 2, 3, 4}, true, 1)
	dst := make([]int16, 4)
	got := r.ShiftedPCM(dst, 1, 2, 2)
	// Content from sample 2 on, then silence (interval 2 absent).
	if got[0] != 3 || got[1] != 4 || got[2] != 0 || got[3] != 0 {
		t.Fatalf("got %v, want [3 4 0 0]", []int16{got[0], got[1], got[2], got[3]})
	}
}
