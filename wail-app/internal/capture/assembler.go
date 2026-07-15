// Package capture holds the pure interval-assembly logic for the Link Audio
// capture side: it buckets received audio buffers (each carrying a local begin
// beat) into fixed-length NINJAM intervals. The realtime source callback and
// Opus/WAIF/relay plumbing live in package main and wrap this proven logic.
package capture

import "github.com/nicholasgasior/wail/wail-app/internal/interval"

// CompletedInterval is a fully-bucketed interval ready to encode. Samples is the
// whole interval, zero-padded (NINJAM intervals are fixed length; capture gaps
// read as silence and are surfaced separately via LAN-loss metrics). Index is
// the *local* interval index; the caller labels it with the shared room index.
type CompletedInterval struct {
	Index        int64
	Samples      []int16 // interleaved, Frames × Channels
	Frames       int     // full interval length in frames
	Channels     int
	WrittenFrames int    // frames actually covered by received buffers (<= Frames)
}

// Complete reports whether the whole interval was covered by received audio.
func (c *CompletedInterval) Complete() bool { return c.WrittenFrames >= c.Frames }

// Assembler buckets one capture channel's buffers into intervals. It assumes
// samples arrive at the internal rate (48 kHz); the caller resamples at the edge
// if the channel differs. Not safe for concurrent use — drive from the drain
// goroutine.
type Assembler struct {
	cfg        interval.Config
	channels   int
	sampleRate uint32
	cur        *intervalBuf
	droppedLate uint64
}

type intervalBuf struct {
	index         int64
	pcm           []int16
	frames        int
	writtenFrames int
}

// New creates an assembler for a channel with the given interval config and
// channel count (1 or 2). sampleRate is the internal assembly rate.
func New(cfg interval.Config, channels int, sampleRate uint32) *Assembler {
	if channels < 1 {
		channels = 1
	}
	return &Assembler{cfg: cfg, channels: channels, sampleRate: sampleRate}
}

// Add places a buffer beginning at local beat `beat` (from Source.BeginBeats)
// into its interval. It returns a completed interval when the buffer opens a new
// interval (the previous one is done), otherwise nil.
func (a *Assembler) Add(beat, tempoBPM float64, samples []int16, numFrames int) *CompletedInterval {
	idx := a.cfg.IndexAtBeat(beat)

	var completed *CompletedInterval
	if a.cur == nil {
		a.cur = a.newBuf(idx, tempoBPM)
	} else if idx > a.cur.index {
		completed = a.finish()
		a.cur = a.newBuf(idx, tempoBPM)
	} else if idx < a.cur.index {
		// Buffer belongs to an already-emitted interval (reordered/late). Drop.
		a.droppedLate++
		return nil
	}

	off := a.cfg.FrameOffset(beat, a.cur.index, a.sampleRate, tempoBPM)
	a.write(off, samples, numFrames)
	return completed
}

// Flush returns the interval currently accumulating (e.g. at shutdown / channel
// removal) and clears it. Returns nil if nothing is buffered.
func (a *Assembler) Flush() *CompletedInterval {
	if a.cur == nil {
		return nil
	}
	c := a.finish()
	a.cur = nil
	return c
}

// DroppedLate is the number of buffers dropped for belonging to an already-
// emitted interval (out-of-order arrivals).
func (a *Assembler) DroppedLate() uint64 { return a.droppedLate }

func (a *Assembler) newBuf(idx int64, tempoBPM float64) *intervalBuf {
	frames := a.cfg.IntervalSamples(a.sampleRate, tempoBPM)
	return &intervalBuf{index: idx, pcm: make([]int16, frames*a.channels), frames: frames}
}

// write copies numFrames of interleaved samples into the current interval at
// frame offset off, clamping so a boundary-straddling buffer never overflows.
func (a *Assembler) write(off int, samples []int16, numFrames int) {
	if off >= a.cur.frames {
		return
	}
	avail := a.cur.frames - off
	if numFrames > avail {
		numFrames = avail
	}
	n := numFrames * a.channels
	if n > len(samples) {
		n = len(samples)
		numFrames = n / a.channels
	}
	copy(a.cur.pcm[off*a.channels:], samples[:n])

	if end := off + numFrames; end > a.cur.writtenFrames {
		a.cur.writtenFrames = end
	}
}

func (a *Assembler) finish() *CompletedInterval {
	b := a.cur
	return &CompletedInterval{
		Index:         b.index,
		Samples:       b.pcm,
		Frames:        b.frames,
		Channels:      a.channels,
		WrittenFrames: b.writtenFrames,
	}
}
