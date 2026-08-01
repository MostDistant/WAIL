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

// Slot provenance: a frame slot is empty until filled by a real decoded frame
// or a PLC-synthesized one. Real frames win over PLC; PLC never overwrites.
const (
	slotEmpty uint8 = iota
	slotPLC
	slotReal
)

type partial struct {
	pcm       []int16
	state     []uint8 // per frame-slot provenance (slotEmpty/slotPLC/slotReal)
	received  int     // distinct REAL WAIF frames placed (PLC does not count)
	concealed int     // slots currently holding PLC audio
	total     int     // total WAIF frames, -1 until the final frame is seen
	maxFrame  int     // highest frame_number seen (+1), for sizing before total known
}

// slotState returns the provenance of frame slot fn (empty if never grown).
func (p *partial) slotState(fn int) uint8 {
	if fn < len(p.state) {
		return p.state[fn]
	}
	return slotEmpty
}

// growState ensures the state slice covers frame slot fn.
func (p *partial) growState(fn int) {
	if fn < len(p.state) {
		return
	}
	grown := make([]uint8, fn+1)
	copy(grown, p.state)
	p.state = grown
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
// carrying totalFrames (the interval's WAIF frame count). A real frame replaces
// a PLC-concealed slot; a duplicate real frame overwrites in place without
// double-counting `received` (which would fake Complete() with holes).
func (r *Reassembler) Add(index int64, frameNumber int, pcm []int16, isFinal bool, totalFrames int) {
	if frameNumber < 0 {
		return
	}
	p := r.ensure(index)
	if isFinal && totalFrames > 0 {
		p.total = totalFrames
	}
	r.place(p, frameNumber, pcm)
	switch p.slotState(frameNumber) {
	case slotPLC:
		p.concealed--
		p.received++
	case slotEmpty:
		p.received++
	case slotReal:
		// duplicate: content already overwritten, count unchanged
	}
	p.growState(frameNumber)
	p.state[frameNumber] = slotReal
}

// AddPLC places codec-concealed PCM at frameNumber, filling only an empty slot
// (real audio always wins) and never counting toward `received`/Complete().
func (r *Reassembler) AddPLC(index int64, frameNumber int, pcm []int16) {
	if frameNumber < 0 {
		return
	}
	p := r.ensure(index)
	if p.slotState(frameNumber) != slotEmpty {
		return
	}
	r.place(p, frameNumber, pcm)
	p.growState(frameNumber)
	p.state[frameNumber] = slotPLC
	p.concealed++
}

// Missing reports the frame slots of interval `index` that hold no audio and
// those holding PLC concealment, measured against total when known (else the
// highest frame seen). (0,0) for unknown intervals.
func (r *Reassembler) Missing(index int64) (missing, concealed int) {
	p := r.partials[index]
	if p == nil {
		return 0, 0
	}
	frames := p.maxFrame
	if p.total > frames {
		frames = p.total
	}
	filled := 0
	for fn := 0; fn < frames && fn < len(p.state); fn++ {
		if p.state[fn] != slotEmpty {
			filled++
		}
	}
	return frames - filled, p.concealed
}

// ensure returns interval index's partial, creating it if absent.
func (r *Reassembler) ensure(index int64) *partial {
	p := r.partials[index]
	if p == nil {
		p = &partial{total: -1}
		r.partials[index] = p
	}
	return p
}

// place copies one frame's PCM into the interval buffer, growing it as needed
// and tracking maxFrame.
func (r *Reassembler) place(p *partial, frameNumber int, pcm []int16) {
	if frameNumber+1 > p.maxFrame {
		p.maxFrame = frameNumber + 1
	}
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

// MaxIndex returns the highest interval index currently buffered; ok is false
// when nothing is buffered. Used by the emit loop to measure how far a
// sender's room labels run ahead of local playout (anchor offset mismatch).
func (r *Reassembler) MaxIndex() (max int64, ok bool) {
	for idx := range r.partials {
		if !ok || idx > max {
			max, ok = idx, true
		}
	}
	return max, ok
}

// MinIndex returns the lowest interval index currently buffered; ok is false
// when nothing is buffered. Retirement uses it to ask whether anything
// *imminent* is held: MaxIndex cannot answer that, since one straggler far
// beyond the playout horizon (a sender whose room labels run ahead) would
// otherwise read as "still playing" for as many boundaries as it is ahead.
func (r *Reassembler) MinIndex() (min int64, ok bool) {
	for idx := range r.partials {
		if !ok || idx < min {
			min, ok = idx, true
		}
	}
	return min, ok
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
