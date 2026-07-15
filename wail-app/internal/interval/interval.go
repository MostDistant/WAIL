// Package interval holds WAIL's pure interval-timing logic: mapping Link beats to
// NINJAM interval indices and back, placing captured audio buffers at the right
// sample offset inside an interval, and the relay-authoritative room clock that
// gives every peer the same interval index (ADR-0003).
//
// It has no cgo, no networking, and no dependency on package main, so it is fully
// unit-testable — the untestable capture/emit cgo layers wrap this proven logic.
package interval

import "math"

// minTempo / minBeatsPerInterval are defensive floors. Interval math divides by
// tempo and by beats-per-interval; internal callers should never pass zero, but
// per the repo trade-off prefs we clamp bad numeric inputs to safe minimums
// rather than panic or propagate errors.
const (
	minTempo   = 1.0 // BPM
	minBars    = 1
	minQuantum = 1.0
)

// Config describes an interval as a beat window: Bars × Quantum beats.
type Config struct {
	Bars    uint32
	Quantum float64
}

// beatsPerInterval returns Bars × Quantum, clamped to a safe minimum so index
// math never divides by zero.
func (c Config) BeatsPerInterval() float64 {
	bars := c.Bars
	if bars < minBars {
		bars = minBars
	}
	q := c.Quantum
	if q < minQuantum {
		q = minQuantum
	}
	return float64(bars) * q
}

// IndexAtBeat returns the interval index containing the given session beat.
// Equivalent to floor(beat / beatsPerInterval); handles negative beats.
func (c Config) IndexAtBeat(beat float64) int64 {
	return int64(math.Floor(beat / c.BeatsPerInterval()))
}

// BeatWindow returns the [start, end) session-beat range of the given interval.
func (c Config) BeatWindow(index int64) (start, end float64) {
	bpi := c.BeatsPerInterval()
	start = float64(index) * bpi
	return start, start + bpi
}

// clampTempo returns a strictly-positive tempo for duration math.
func clampTempo(tempoBPM float64) float64 {
	if !(tempoBPM > minTempo) { // also catches NaN
		return minTempo
	}
	return tempoBPM
}

// IntervalSamples returns the number of sample frames one interval spans at the
// given tempo and sample rate. Rounded to the nearest frame.
func (c Config) IntervalSamples(sampleRate uint32, tempoBPM float64) int {
	seconds := c.BeatsPerInterval() * 60.0 / clampTempo(tempoBPM)
	return int(math.Round(seconds * float64(sampleRate)))
}

// FrameOffset returns the sample-frame offset within interval `index` at which a
// buffer beginning at session beat `beatAtBufferBegin` should be written. The
// result is clamped to [0, IntervalSamples) so an out-of-window buffer can never
// index outside the interval's backing storage.
func (c Config) FrameOffset(beatAtBufferBegin float64, index int64, sampleRate uint32, tempoBPM float64) int {
	start, _ := c.BeatWindow(index)
	offsetBeats := beatAtBufferBegin - start
	if offsetBeats < 0 {
		offsetBeats = 0
	}
	seconds := offsetBeats * 60.0 / clampTempo(tempoBPM)
	off := int(math.Round(seconds * float64(sampleRate)))
	if max := c.IntervalSamples(sampleRate, tempoBPM); off > max {
		off = max
	}
	if off < 0 {
		off = 0
	}
	return off
}
