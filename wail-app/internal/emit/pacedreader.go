package emit

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
	startBeat  float64
	totalFrames int
	cursor     int // frame position
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
		startBeat:   startBeat,
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

// beatAt maps a frame offset to its begin beat.
func (p *PacedReader) beatAt(frame int) float64 {
	seconds := float64(frame) / float64(p.sampleRate)
	tempo := p.tempoBPM
	if tempo <= 0 {
		tempo = 120
	}
	return p.startBeat + seconds*tempo/60.0
}
