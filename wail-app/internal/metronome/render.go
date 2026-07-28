// Package metronome renders a WAIL click track: one interval of PCM with a
// short percussive click on every whole beat, accented on bar downbeats. It is
// pure (no cgo, no networking), so it unit-tests without the Link SDK; the emit
// engine publishes the rendered buffers on a locally-owned room metronome
// Link Audio channel so a user can align it against their DAW's own metronome.
package metronome

import (
	"math"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

const (
	clickMs    = 15   // click burst length
	attackMs   = 1    // linear fade-in so the burst starts without a hard edge
	accentFreq = 1500 // Hz, bar downbeat
	beatFreq   = 1000 // Hz, other beats
	accentAmp  = 0.38 // 0..1 of full scale
	beatAmp    = 0.24
	decay      = 6.0 // exponential decay rate over the post-attack tail
)

// clickFrameCount is the click burst length in sample frames.
func clickFrameCount(sampleRate uint32) int {
	return int(sampleRate) * clickMs / 1000
}

// RenderInterval builds one interval (index idx) of interleaved PCM for the
// metronome: silence everywhere except a click burst at each whole beat inside
// the interval's beat window, louder/higher on bar downbeats (beats where
// beat mod Quantum == 0). The buffer length matches intervalPlayoutFrames
// (cfg.IntervalSamples), so it drops straight into a PacedReader.
func RenderInterval(cfg interval.Config, tempoBPM float64, sampleRate uint32, channels int, idx int64) []int16 {
	if channels < 1 {
		channels = 1
	}
	totalFrames := cfg.IntervalSamples(sampleRate, tempoBPM)
	buf := make([]int16, totalFrames*channels)

	start, end := cfg.BeatWindow(idx)
	q := cfg.Quantum
	if q < 1 {
		q = 1
	}

	// Whole (integer) beats are where a DAW metronome clicks; iterate them.
	for b := int64(math.Ceil(start - 1e-9)); float64(b) < end-1e-9; b++ {
		off := cfg.FrameOffset(float64(b), idx, sampleRate, tempoBPM)
		writeClick(buf, channels, totalFrames, off, isDownbeat(float64(b), q), sampleRate)
	}
	return buf
}

// isDownbeat reports whether an integer beat begins a bar (a multiple of the
// beats-per-bar quantum).
func isDownbeat(beat, quantum float64) bool {
	m := math.Mod(beat, quantum)
	return m < 1e-6 || quantum-m < 1e-6
}

// writeClick renders one enveloped sine burst into buf starting at frame off,
// clamped to the interval end, on every channel.
func writeClick(buf []int16, channels, totalFrames, off int, accent bool, sampleRate uint32) {
	freq, amp := float64(beatFreq), beatAmp
	if accent {
		freq, amp = float64(accentFreq), accentAmp
	}
	n := clickFrameCount(sampleRate)
	attack := int(sampleRate) * attackMs / 1000
	if attack < 1 {
		attack = 1
	}
	sr := float64(sampleRate)
	for i := 0; i < n; i++ {
		f := off + i
		if f < 0 || f >= totalFrames {
			break
		}
		env := 1.0
		if i < attack {
			env = float64(i) / float64(attack)
		} else {
			env = math.Exp(-decay * float64(i-attack) / float64(n-attack))
		}
		v := int16(math.Round(amp * env * math.Sin(2*math.Pi*freq*float64(i)/sr) * 32767))
		base := f * channels
		for c := 0; c < channels; c++ {
			buf[base+c] = v
		}
	}
}
