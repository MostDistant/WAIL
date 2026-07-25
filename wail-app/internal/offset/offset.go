// Package offset measures a stream's rhythmic phase offset against the room
// grid from its labeled frames (the "debug room" analysis): segments of
// energy in the frame-RMS series yield click onsets whose absolute room-frame
// positions fold into a modal beat phase. A stream whose content sits on the
// grid reports ~0; a peer performing late (e.g. monitoring through a latent
// path) shows it directly in milliseconds, no DAW or envelope-matching needed.
package offset

import "math"

// Frame is one labeled audio frame's energy at its absolute room-frame
// position (intervalIndex*framesPerInterval + frameNumber).
type Frame struct {
	Abs int64
	RMS float64
}

// Tracker accumulates a bounded history of frames for one stream.
type Tracker struct {
	frames []Frame
	max    int
}

func NewTracker(maxFrames int) *Tracker {
	if maxFrames <= 0 {
		maxFrames = 4000
	}
	return &Tracker{max: maxFrames}
}

func (t *Tracker) Add(abs int64, rms float64) {
	t.frames = append(t.frames, Frame{Abs: abs, RMS: rms})
	if len(t.frames) > t.max {
		t.frames = t.frames[len(t.frames)-t.max:]
	}
}

// Len reports the buffered frame count (analysis needs a few hundred).
func (t *Tracker) Len() int { return len(t.frames) }

type segment struct{ start, end int64 }

// segments finds energy bursts above max(20% of peak, floor) in abs space.
// Gaps up to 2 frames are bridged (late/lost frames read as silence).
func segments(frames []Frame, floor float64) []segment {
	if len(frames) == 0 {
		return nil
	}
	mx := 0.0
	for _, f := range frames {
		if f.RMS > mx {
			mx = f.RMS
		}
	}
	thr := mx * 0.2
	if thr < floor {
		thr = floor
	}
	var segs []segment
	i := 0
	for i < len(frames) {
		if frames[i].RMS > thr {
			start := frames[i].Abs
			end := frames[i].Abs
			j := i + 1
			for j < len(frames) && (frames[j].RMS > thr || (j+1 < len(frames) && frames[j+1].RMS > thr && frames[j+1].Abs-frames[j].Abs <= 2)) {
				end = frames[j].Abs
				j++
			}
			segs = append(segs, segment{start, end})
			i = j
		} else {
			i++
		}
	}
	return segs
}

// Offset returns the stream's modal content phase relative to the beat grid,
// in milliseconds: segment onsets are folded into beat phase and the circular
// mode returned, wrapped to ±beat/2. beatMs is 60000/bpm; frameMs is the
// duration of one frame (20ms in WAIL). ok is false with fewer than 4
// segments (not enough rhythmic content to judge).
func (t *Tracker) Offset(frameMs, beatMs float64) (float64, bool) {
	segs := segments(t.frames, 300)
	if len(segs) < 4 {
		return 0, false
	}
	// Circular mode of onset phases in 24 bins per beat.
	var bins [24]int
	for _, s := range segs {
		ph := math.Mod(s.phase(frameMs, beatMs), 1)
		if ph < 0 {
			ph++
		}
		b := int(ph * 24)
		if b >= 24 {
			b = 23
		}
		bins[b]++
	}
	best, bi := 0, 0
	for i, c := range bins {
		if c > best {
			best, bi = c, i
		}
	}
	ph := (float64(bi) + 0.5) / 24
	if ph > 0.5 {
		ph--
	}
	return ph * beatMs, true
}

func (s segment) phase(frameMs, beatMs float64) float64 {
	return float64(s.start) * frameMs / beatMs
}
