package emit

import "math"

// PacedReader walks an interval's PCM in sink-sized chunks, stamping each chunk
// with the beat at which it begins. WAIL must not dump a whole interval into a
// Link Audio sink at once — the sink queue drains straight onto the wire and
// receivers drop far-future buffers — so the emit loop tops the sink up a few ms
// at a time, impersonating a live performer one interval behind (research §6.3).
//
// The reader stamps beats from a fixed start beat and tempo, independent of the
// underlying buffer, so play-partial + live-append work: the buffer can be
// extended/filled after the reader is created and later chunks read the newer
// samples.
type PacedReader struct {
	samples    func() []int16 // late-bound so live-append is visible
	channels   int
	sampleRate uint32
	tempoBPM   float64
	// Beat anchor: frame baseFrame begins at beat baseBeat. Rebase moves the
	// anchor to the cursor (for tempo changes) without retro-shifting past stamps.
	baseBeat    float64
	baseFrame   int
	totalFrames int
	cursor      int // frame position
}

// NewPacedReader creates a reader over an interval. samples is a getter (called
// each Next) so a growing/live-appended buffer stays visible. totalFrames is the
// interval's full frame count; startBeat is the beat at which the interval plays.
func NewPacedReader(samples func() []int16, channels int, sampleRate uint32, tempoBPM, startBeat float64, totalFrames int) *PacedReader {
	if channels < 1 {
		channels = 1
	}
	return &PacedReader{
		samples:     samples,
		channels:    channels,
		sampleRate:  sampleRate,
		tempoBPM:    tempoBPM,
		baseBeat:    startBeat,
		totalFrames: totalFrames,
	}
}

// Next returns up to chunkFrames of interleaved samples starting at the current
// cursor, the beat at which that chunk begins, and done=true once the whole
// interval has been read. Frames not yet present in the underlying buffer read
// as silence (play-partial). The returned slice is a fresh copy safe to hand to
// the sink.
func (p *PacedReader) Next(chunkFrames int) (samples []int16, beatAtChunk float64, done bool) {
	if p.cursor >= p.totalFrames {
		return nil, p.beatAt(p.cursor), true
	}
	n := chunkFrames
	if p.cursor+n > p.totalFrames {
		n = p.totalFrames - p.cursor
	}
	beat := p.beatAt(p.cursor)

	out := make([]int16, n*p.channels)
	src := p.samples()
	start := p.cursor * p.channels
	if start < len(src) {
		end := start + n*p.channels
		if end > len(src) {
			end = len(src)
		}
		copy(out, src[start:end])
	}
	// Anything past len(src) stays zero (silence for not-yet-arrived frames).

	p.cursor += n
	return out, beat, p.cursor >= p.totalFrames
}

// Remaining returns the number of frames left to read.
func (p *PacedReader) Remaining() int {
	if p.cursor >= p.totalFrames {
		return 0
	}
	return p.totalFrames - p.cursor
}

// Cursor returns the current frame position.
func (p *PacedReader) Cursor() int { return p.cursor }

// TotalFrames returns the interval's full frame count.
func (p *PacedReader) TotalFrames() int { return p.totalFrames }

// TempoBPM returns the tempo the reader stamps beats at.
func (p *PacedReader) TempoBPM() float64 { return p.tempoBPM }

// Skip advances the cursor to toFrame without emitting (underrun skip-ahead —
// frames behind the playhead would be stamped in the past and dropped by
// receivers). Never moves backward; clamps to the interval end.
func (p *PacedReader) Skip(toFrame int) {
	if toFrame > p.totalFrames {
		toFrame = p.totalFrames
	}
	if toFrame > p.cursor {
		p.cursor = toFrame
	}
}

// FrameAtBeat maps a session beat to its frame position (inverse of beatAt).
func (p *PacedReader) FrameAtBeat(beat float64) int {
	seconds := (beat - p.baseBeat) * 60.0 / p.tempo()
	return p.baseFrame + int(math.Round(seconds*float64(p.sampleRate)))
}

// Rebase re-anchors beat stamping at the current cursor with a new tempo (and
// interval length, which changes with it): stamps already emitted keep their
// beats; future stamps advance at the new tempo from here.
func (p *PacedReader) Rebase(tempoBPM float64, totalFrames int) {
	p.baseBeat = p.beatAt(p.cursor)
	p.baseFrame = p.cursor
	if tempoBPM > 0 {
		p.tempoBPM = tempoBPM
	}
	if totalFrames > 0 {
		p.totalFrames = totalFrames
	}
}

// beatAt maps a frame offset to its begin beat.
func (p *PacedReader) beatAt(frame int) float64 {
	seconds := float64(frame-p.baseFrame) / float64(p.sampleRate)
	return p.baseBeat + seconds*p.tempo()/60.0
}

func (p *PacedReader) tempo() float64 {
	if p.tempoBPM <= 0 {
		return 120
	}
	return p.tempoBPM
}
