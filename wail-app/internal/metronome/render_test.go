package metronome

import (
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

const (
	testRate  = 48000
	testTempo = 120.0
)

// peakAround returns the largest absolute sample within [center-clickFrames,
// center+clickFrames) of channel 0.
func peakAround(buf []int16, channels, center, clickFrames int) int {
	peak := 0
	lo, hi := center-clickFrames, center+clickFrames
	if lo < 0 {
		lo = 0
	}
	for f := lo; f < hi; f++ {
		i := f * channels
		if i < 0 || i >= len(buf) {
			continue
		}
		v := int(buf[i])
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

func TestRenderIntervalLength(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 4}
	for _, ch := range []int{1, 2} {
		buf := RenderInterval(cfg, testTempo, testRate, ch, 0)
		wantFrames := cfg.IntervalSamples(testRate, testTempo)
		if got := len(buf); got != wantFrames*ch {
			t.Errorf("channels=%d: len=%d, want %d", ch, got, wantFrames*ch)
		}
	}
}

func TestRenderIntervalStereoMirrors(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 4}
	buf := RenderInterval(cfg, testTempo, testRate, 2, 0)
	for i := 0; i+1 < len(buf); i += 2 {
		if buf[i] != buf[i+1] {
			t.Fatalf("stereo not mirrored at frame %d: L=%d R=%d", i/2, buf[i], buf[i+1])
			return
		}
	}
}

// At 120 BPM / Quantum 4, each beat is 24000 frames; a click sits at every beat
// and the space between beats is silent.
func TestRenderIntervalClicksOnBeats(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 4} // beats 0,1,2,3 at frames 0,24000,48000,72000
	ch := 2
	buf := RenderInterval(cfg, testTempo, testRate, ch, 0)
	clickFrames := clickFrameCount(testRate)

	for _, beat := range []int{0, 1, 2, 3} {
		center := beat * 24000
		if peakAround(buf, ch, center, clickFrames) == 0 {
			t.Errorf("beat %d (frame %d): expected a click, got silence", beat, center)
		}
	}

	// Midway between beat 1 and beat 2 is well outside any 15ms click window.
	mid := 36000
	if p := peakAround(buf, ch, mid, clickFrames/2); p != 0 {
		t.Errorf("frame %d: expected silence between beats, got peak %d", mid, p)
	}
}

func TestRenderIntervalAccentsDownbeat(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 4} // downbeat only at beat 0
	ch := 2
	buf := RenderInterval(cfg, testTempo, testRate, ch, 0)
	clickFrames := clickFrameCount(testRate)

	accent := peakAround(buf, ch, 0, clickFrames)      // beat 0 (bar downbeat)
	regular := peakAround(buf, ch, 24000, clickFrames) // beat 1
	if accent <= regular {
		t.Errorf("downbeat peak %d should exceed regular beat peak %d", accent, regular)
	}
}

// The bar downbeat is an absolute property (beat mod Quantum == 0), so it lands
// at the head of interval 1 too, not just interval 0.
func TestRenderIntervalDownbeatAbsolute(t *testing.T) {
	cfg := interval.Config{Bars: 1, Quantum: 4}
	ch := 1
	i0 := RenderInterval(cfg, testTempo, testRate, ch, 0)
	i1 := RenderInterval(cfg, testTempo, testRate, ch, 1)
	clickFrames := clickFrameCount(testRate)

	accent0 := peakAround(i0, ch, 0, clickFrames)
	accent1 := peakAround(i1, ch, 0, clickFrames)
	if accent1 != accent0 {
		t.Errorf("interval 1 head peak %d should match interval 0 head peak %d (both bar downbeats)", accent1, accent0)
	}
}
