package capture

import (
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// Continuation-pad tests: at tempos where the interval length isn't a multiple
// of the window, the short final window must carry the next interval's real
// head samples instead of zeros — feeding the Opus encoder a zero gap bakes an
// audible transient into every boundary (measured -13 dBFS over the first 10ms
// of each interval). 16 beats at 130 BPM / 100 Hz = 738.46 → 738 frames per
// interval: 8 windows of 100, final window = 38 real + 62 continuation.
const (
	csr     = 100
	cframes = 100
	ctempo  = 130.0
)

func ccfg() interval.Config { return interval.Config{Bars: 4, Quantum: 4} }

// cbeat converts a frame offset from the stream start to its beat at ctempo.
func cbeat(frameOff int) float64 { return float64(frameOff) / csr * ctempo / 60.0 }

// cramp returns mono PCM where frame i (absolute) holds value i+1 (never zero).
func cramp(fromFrame, n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(fromFrame + i + 1)
	}
	return s
}

func cfeed(a *Assembler, fromFrame, n, bufFrames int) []Window {
	var out []Window
	for off := 0; off < n; off += bufFrames {
		c := bufFrames
		if off+c > n {
			c = n - off
		}
		out = append(out, a.AddWindows(cbeat(fromFrame+off), ctempo, cramp(fromFrame+off, c), c)...)
	}
	return out
}

func TestFinalWindowCarriesContinuation(t *testing.T) {
	a := NewWindowed(ccfg(), 1, csr, cframes)
	is := ccfg().IntervalSamples(csr, ctempo)
	if is%cframes == 0 {
		t.Fatal("test tempo unexpectedly window-aligned")
	}
	tail := is % cframes  // 38
	pad := cframes - tail // 62

	// Feed well past the boundary so the continuation is available.
	windows := cfeed(a, 0, is+2*cframes, 50)

	finalAt := -1
	for i, w := range windows {
		if w.IntervalIndex == 0 && w.IsFinal {
			finalAt = i
			break
		}
	}
	if finalAt < 0 {
		t.Fatal("interval 0's final window was never emitted")
	}
	w := windows[finalAt]
	if w.Number != w.Total-1 || len(w.Samples) != cframes {
		t.Fatalf("final window mislabeled/missized: %+v", w)
	}
	for i := 0; i < tail; i++ {
		if want := int16(is - tail + i + 1); w.Samples[i] != want {
			t.Fatalf("final window tail frame %d = %d, want %d", i, w.Samples[i], want)
		}
	}
	for i := 0; i < pad; i++ {
		if want := int16(is + i + 1); w.Samples[tail+i] != want {
			t.Fatalf("final window pad frame %d = %d, want %d (next interval's head, not zeros)",
				i, w.Samples[tail+i], want)
		}
	}
	// The final window must still precede the next interval's first window.
	for i := 0; i < finalAt; i++ {
		if windows[i].IntervalIndex > 0 {
			t.Fatalf("interval 1 window emitted before interval 0's final")
		}
	}
}

func TestFinalWindowDeferredUntilContinuationArrives(t *testing.T) {
	a := NewWindowed(ccfg(), 1, csr, cframes)
	is := ccfg().IntervalSamples(csr, ctempo)
	pad := cframes - is%cframes

	// Exactly one interval: every full window emits, the short final is held.
	w := cfeed(a, 0, is, 50)
	for _, x := range w {
		if x.IsFinal {
			t.Fatal("short final window emitted before its continuation arrived")
		}
	}
	if len(w) != is/cframes {
		t.Fatalf("expected %d full windows, got %d", is/cframes, len(w))
	}

	// The continuation arrives: the final window emits, padded with it.
	w = cfeed(a, is, pad, pad)
	if len(w) != 1 || !w[0].IsFinal || w[0].IntervalIndex != 0 {
		t.Fatalf("expected exactly interval 0's final window, got %+v", w)
	}
	if got, want := w[0].Samples[cframes-1], int16(is+pad); got != want {
		t.Fatalf("pad end = %d, want %d", got, want)
	}
}

func TestFlushZeroPadsHeldFinalWindow(t *testing.T) {
	a := NewWindowed(ccfg(), 1, csr, cframes)
	is := ccfg().IntervalSamples(csr, ctempo)
	tail := is % cframes

	cfeed(a, 0, is, 50) // final held, waiting for continuation
	w := a.FlushWindows()
	if len(w) != 1 || !w[0].IsFinal || w[0].IntervalIndex != 0 {
		t.Fatalf("flush must emit the held final window, got %+v", w)
	}
	if w[0].Samples[tail-1] == 0 {
		t.Fatal("final window tail lost on flush")
	}
	for i := tail; i < cframes; i++ {
		if w[0].Samples[i] != 0 {
			t.Fatalf("flush pad frame %d = %d, want 0 (no continuation exists)", i, w[0].Samples[i])
		}
	}
}

func TestReanchorFlushesHeldFinalWindowOnce(t *testing.T) {
	a := NewWindowed(ccfg(), 1, csr, cframes)
	is := ccfg().IntervalSamples(csr, ctempo)

	cfeed(a, 0, is, 50) // final held
	// Genuine discontinuity: jump two intervals ahead.
	jump := 2*is + 10
	w := a.AddWindows(cbeat(jump), ctempo, cramp(jump, 50), 50)
	finals := 0
	for _, x := range w {
		if x.IntervalIndex == 0 && x.IsFinal {
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("held final window flushed %d times on re-anchor, want 1", finals)
	}
}

func TestAlignedIntervalFinalWindowUnchanged(t *testing.T) {
	// 120 BPM at 100 Hz: 800 frames = exactly 8 windows; the final window is
	// full-length and must emit on coverage, not be held.
	a := NewWindowed(ccfg(), 1, csr, cframes)
	is := ccfg().IntervalSamples(csr, 120)
	if is%cframes != 0 {
		t.Fatal("expected window-aligned interval at 120 BPM")
	}
	var out []Window
	for off := 0; off < is; off += 50 {
		out = append(out, a.AddWindows(float64(off)/csr*2, 120, cramp(off, 50), 50)...)
	}
	if len(out) != is/cframes || !out[len(out)-1].IsFinal {
		t.Fatalf("aligned interval: got %d windows, final=%v; want %d with final emitted",
			len(out), len(out) > 0 && out[len(out)-1].IsFinal, is/cframes)
	}
}
