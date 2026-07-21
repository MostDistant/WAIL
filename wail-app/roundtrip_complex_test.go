//go:build !linkstub

package main

import (
	"math"
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/dsp"
)

// monoAt mixes one frame of interleaved stereo to mono.
func monoAt(s []int16, frame int) float64 {
	return (float64(s[frame*2]) + float64(s[frame*2+1])) / 2
}

// bestLag finds the delay (in frames) of out relative to src that maximizes
// cross-correlation over the first second — Opus has a small codec lookahead,
// so the decoded stream lags the input by a constant few ms.
func bestLag(src, out []int16, maxLag, window int) int {
	best, bestCorr := 0, math.Inf(-1)
	for lag := 0; lag <= maxLag; lag++ {
		corr := 0.0
		for i := 0; i < window; i++ {
			corr += monoAt(src, i) * monoAt(out, i+lag)
		}
		if corr > bestCorr {
			bestCorr, best = corr, lag
		}
	}
	return best
}

func rmsWindow(s []int16, startFrame, frames int) float64 {
	var e float64
	for i := startFrame; i < startFrame+frames; i++ {
		m := monoAt(s, i)
		e += m * m
	}
	return math.Sqrt(e / float64(frames))
}

// TestComplexProgramRoundTrip pushes dense, music-like program material (detuned
// pad + bass + percussive transients + noise) through the exact streaming path a
// capture channel uses — one EncodeWindow per 20ms with interval-boundary
// metadata, stateful wire decode on the other side — and asserts the result is
// sample-exact in length and free of dropouts/chops: every energetic source
// window must come back with comparable energy. Global SNR is reported.
func TestComplexProgramRoundTrip(t *testing.T) {
	const channels = 2
	const seconds = 10
	spf := samplesPerWaifFrame(dumpTestRate) // 960 frames / 20ms window
	frames := dumpTestRate * seconds
	src := dsp.GenerateComplexProgram(frames, dumpTestRate)
	if len(src) != frames*channels {
		t.Fatalf("generator length %d, want %d", len(src), frames*channels)
	}

	enc, err := NewIntervalEncoder(channels, dumpTestRate, engineBitrateKbps)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewIntervalDecoder(channels, dumpTestRate)
	if err != nil {
		t.Fatal(err)
	}

	// Stream in 20ms windows across multiple 2s intervals, as emitWindow does.
	const winPerInterval = 100
	nWin := frames / spf
	out := make([]int16, 0, len(src))
	var seq uint32
	for wi := 0; wi < nWin; wi++ {
		win := src[wi*spf*channels : (wi+1)*spf*channels]
		fn := uint32(wi % winPerInterval)
		wire, err := enc.EncodeWindow(win, WindowMeta{
			RoomIndex: int64(wi / winPerInterval), StreamID: 0, FrameNumber: fn, Seq: seq,
			IsFinal: fn == winPerInterval-1, TotalFrames: winPerInterval,
			BPM: 240, Quantum: 4, Bars: 4,
		})
		if err != nil {
			t.Fatalf("encode window %d: %v", wi, err)
		}
		seq++
		f, err := DecodeAudioFrameWire(wire)
		if err != nil {
			t.Fatalf("wire decode window %d: %v", wi, err)
		}
		pcm, err := dec.DecodeFrame(f.OpusData)
		if err != nil {
			t.Fatalf("opus decode window %d: %v", wi, err)
		}
		out = append(out, pcm...)
	}

	if len(out) != len(src) {
		t.Fatalf("round trip length %d != source %d (not sample-exact)", len(out), len(src))
	}

	lag := bestLag(src, out, 2*spf, dumpTestRate)
	t.Logf("codec delay: %d frames (%.2f ms)", lag, 1000*float64(lag)/dumpTestRate)

	// Per-20ms-window energy envelope on the aligned overlap. A dropped or
	// zeroed window shows as decoded energy collapsing while the source is hot.
	overlap := frames - lag
	nCheck := overlap/spf - 1 // skip the trailing partial window
	dropouts := 0
	worstRatio := math.Inf(1)
	var sigE, errE float64
	for w := 1; w < nCheck; w++ { // skip window 0 (codec warm-up)
		s := rmsWindow(src, w*spf, spf)
		o := rmsWindow(out, w*spf+lag, spf)
		if s > 800 {
			ratio := o / s
			if ratio < worstRatio {
				worstRatio = ratio
			}
			if ratio < 0.1 {
				dropouts++
				if dropouts <= 5 {
					t.Errorf("dropout at window %d (t=%.2fs): src RMS %.0f → decoded RMS %.0f", w, float64(w)*0.02, s, o)
				}
			}
		}
		for i := w * spf; i < (w+1)*spf; i++ {
			sm := monoAt(src, i)
			om := monoAt(out, i+lag)
			sigE += sm * sm
			errE += (sm - om) * (sm - om)
		}
	}
	if dropouts > 0 {
		t.Fatalf("%d dropout windows — round trip is choppy", dropouts)
	}
	snr := 10 * math.Log10(sigE/errE)
	t.Logf("checked %d windows: worst energy ratio %.2f, global SNR %.1f dB", nCheck-1, worstRatio, snr)
	if worstRatio < 0.35 {
		t.Fatalf("worst window energy ratio %.2f < 0.35 — energy collapse without full dropout", worstRatio)
	}
	if snr < 8 {
		t.Fatalf("global SNR %.1f dB < 8 dB — waveform fidelity too low", snr)
	}
}
