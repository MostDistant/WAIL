// Package emit holds the pure playback-side logic for the Link Audio emit path:
// reassembling decoded WAIF frames into interval PCM, and pacing an interval out
// to a Link Audio sink in real time. The Opus decode and the cgo sink live in
// package main and wrap this proven logic; the hold-until-boundary timing lives
// in internal/playout.
package emit

// Reassembler collects decoded audio frames (one per 20ms WAIF frame) into a
// per-index interval PCM buffer, keyed by room interval index, for one remote
// stream. Frames may arrive out of order; the final frame carries the interval's
// total frame count. Not safe for concurrent use.
type Reassembler struct {
	channels        int
	samplesPerFrame int // decoded PCM frames per WAIF frame (e.g. 960 for 20ms @48k)
	partials        map[int64]*partial
}

type partial struct {
	pcm      []int16
	received int  // WAIF frames placed
	total    int  // total WAIF frames, -1 until the final frame is seen
	maxFrame int  // highest frame_number seen (+1), for sizing before total known
}

// New creates a reassembler. samplesPerFrame is the decoded PCM frame count of
// one WAIF frame (e.g. 960 for a 20ms frame at 48 kHz); channels is 1 or 2.
func New(channels, samplesPerFrame int) *Reassembler {
	if channels < 1 {
		channels = 1
	}
	return &Reassembler{
		channels:        channels,
		samplesPerFrame: samplesPerFrame,
		partials:        make(map[int64]*partial),
	}
}

// Add places one decoded frame's PCM at frameNumber within interval `index`.
// pcm is interleaved (samplesPerFrame × channels). isFinal marks the last frame,
// carrying totalFrames (the interval's WAIF frame count).
func (r *Reassembler) Add(index int64, frameNumber int, pcm []int16, isFinal bool, totalFrames int) {
	if frameNumber < 0 {
		return
	}
	p := r.partials[index]
	if p == nil {
		p = &partial{total: -1}
		r.partials[index] = p
	}
	if isFinal && totalFrames > 0 {
		p.total = totalFrames
	}
	if frameNumber+1 > p.maxFrame {
		p.maxFrame = frameNumber + 1
	}

	// Grow the interval buffer to hold this frame.
	need := (frameNumber + 1) * r.samplesPerFrame * r.channels
	if len(p.pcm) < need {
		grown := make([]int16, need)
		copy(grown, p.pcm)
		p.pcm = grown
	}
	off := frameNumber * r.samplesPerFrame * r.channels
	n := r.samplesPerFrame * r.channels
	if n > len(pcm) {
		n = len(pcm)
	}
	copy(p.pcm[off:off+n], pcm[:n])
	p.received++
}

// Complete reports whether every frame of interval `index` has arrived.
func (r *Reassembler) Complete(index int64) bool {
	p := r.partials[index]
	return p != nil && p.total > 0 && p.received >= p.total
}

// Has reports whether any frames for interval `index` have arrived.
func (r *Reassembler) Has(index int64) bool {
	_, ok := r.partials[index]
	return ok
}

// Interval returns the current PCM for interval `index` (whatever has arrived,
// zero-padded to the highest frame seen or to total when known) without removing
// it — used for play-partial + live-append while the interval is playing.
func (r *Reassembler) Interval(index int64) (samples []int16, received, total int, ok bool) {
	p := r.partials[index]
	if p == nil {
		return nil, 0, 0, false
	}
	return r.sized(p), p.received, p.total, true
}

// Take removes and returns interval `index`.
func (r *Reassembler) Take(index int64) (samples []int16, received, total int, ok bool) {
	p := r.partials[index]
	if p == nil {
		return nil, 0, 0, false
	}
	delete(r.partials, index)
	return r.sized(p), p.received, p.total, true
}

// Drop discards any partial for interval `index` and every earlier interval
// (they can never be played once we've moved past them).
func (r *Reassembler) Drop(upToInclusive int64) {
	for idx := range r.partials {
		if idx <= upToInclusive {
			delete(r.partials, idx)
		}
	}
}

// sized returns the interval PCM padded to its full frame count when the final
// frame is known, else to the highest received frame.
func (r *Reassembler) sized(p *partial) []int16 {
	frames := p.maxFrame
	if p.total > frames {
		frames = p.total
	}
	need := frames * r.samplesPerFrame * r.channels
	if len(p.pcm) >= need {
		return p.pcm[:need]
	}
	grown := make([]int16, need)
	copy(grown, p.pcm)
	p.pcm = grown
	return grown
}
