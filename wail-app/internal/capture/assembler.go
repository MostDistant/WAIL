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

	// Windowed (streaming) mode: 0 in batch mode.
	windowFrames    int
	droppedBackfill uint64
}

type intervalBuf struct {
	index         int64
	pcm           []int16
	frames        int
	writtenFrames int
	emitted       int // windows already emitted (windowed mode)
	totalWindows  int // fixed at interval open (windowed mode)
}

// New creates an assembler for a channel with the given interval config and
// channel count (1 or 2). sampleRate is the internal assembly rate.
func New(cfg interval.Config, channels int, sampleRate uint32) *Assembler {
	if channels < 1 {
		channels = 1
	}
	return &Assembler{cfg: cfg, channels: channels, sampleRate: sampleRate}
}

// NewWindowed creates an assembler in streaming mode: instead of handing back
// whole intervals, AddWindows emits fixed-size encode-ready windows (one WAIF
// frame each) as soon as capture coverage passes them, so transmission can run
// during the interval rather than bursting at its boundary.
func NewWindowed(cfg interval.Config, channels int, sampleRate uint32, windowFrames int) *Assembler {
	a := New(cfg, channels, sampleRate)
	if windowFrames < 1 {
		windowFrames = 1
	}
	a.windowFrames = windowFrames
	return a
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

// Window is one encode-ready chunk of an interval in streaming mode: exactly
// windowFrames × channels interleaved samples (the short final window of an
// odd-length interval is zero-padded). Number is the window index within the
// interval — the WAIF frame number — and IsFinal marks the interval's last
// window (its WAIF frame carries the interval trailer). Samples may alias the
// assembler's interval buffer and is valid until the next call on the
// assembler; encode it before adding more audio.
type Window struct {
	IntervalIndex int64
	Number        int
	Total         int
	IsFinal       bool
	Samples       []int16
}

// AddWindows is the streaming counterpart of Add (requires NewWindowed). It
// places the buffer exactly like Add and returns the windows that became ready:
// a window is ready once coverage passes its end, and when a buffer opens a new
// interval the previous interval's remaining windows flush first (zero-padded —
// capture gaps read as silence), ending with its final window.
//
// Emitted windows are immutable: a buffer that lands behind the emitted
// boundary is trimmed to it (fully-behind buffers are dropped) and counted in
// DroppedBackfill. Capture buffers arrive in temporal order, so this only
// fires on pathological reordering.
func (a *Assembler) AddWindows(beat, tempoBPM float64, samples []int16, numFrames int) []Window {
	idx := a.cfg.IndexAtBeat(beat)

	var out []Window
	if a.cur == nil {
		a.cur = a.newBuf(idx, tempoBPM)
	} else if idx > a.cur.index {
		out = a.remainingWindows()
		a.cur = a.newBuf(idx, tempoBPM)
	} else if idx < a.cur.index {
		a.droppedLate++
		return nil
	}

	off := a.cfg.FrameOffset(beat, a.cur.index, a.sampleRate, tempoBPM)
	if lim := a.cur.emitted * a.windowFrames; off < lim {
		a.droppedBackfill++
		skip := lim - off
		if skip >= numFrames {
			return out
		}
		samples = samples[skip*a.channels:]
		numFrames -= skip
		off = lim
	}
	a.write(off, samples, numFrames)
	return append(out, a.readyWindows()...)
}

// FlushWindows emits the current interval's remaining windows (zero-padded,
// ending with its final window) and clears it — the streaming counterpart of
// Flush, for shutdown / channel removal.
func (a *Assembler) FlushWindows() []Window {
	if a.cur == nil {
		return nil
	}
	w := a.remainingWindows()
	a.cur = nil
	if len(w) == 0 {
		return nil
	}
	return w
}

// DroppedBackfill is the number of buffers trimmed or dropped for landing
// behind the emitted-window boundary (windowed mode only).
func (a *Assembler) DroppedBackfill() uint64 { return a.droppedBackfill }

// readyWindows emits every window whose end is covered. Exact full coverage
// also readies the (possibly short) final window without waiting for close.
func (a *Assembler) readyWindows() []Window {
	b := a.cur
	ready := b.writtenFrames / a.windowFrames
	if b.writtenFrames >= b.frames || ready > b.totalWindows {
		ready = b.totalWindows
	}
	var out []Window
	for k := b.emitted; k < ready; k++ {
		out = append(out, a.window(k))
	}
	if ready > b.emitted {
		b.emitted = ready
	}
	return out
}

// remainingWindows flushes every not-yet-emitted window of the current interval.
func (a *Assembler) remainingWindows() []Window {
	b := a.cur
	var out []Window
	for k := b.emitted; k < b.totalWindows; k++ {
		out = append(out, a.window(k))
	}
	b.emitted = b.totalWindows
	return out
}

func (a *Assembler) window(k int) Window {
	b := a.cur
	start := k * a.windowFrames * a.channels
	end := start + a.windowFrames*a.channels
	var s []int16
	if end <= len(b.pcm) {
		s = b.pcm[start:end]
	} else {
		// Short final window of an odd-length interval: pad to a full frame.
		s = make([]int16, a.windowFrames*a.channels)
		if start < len(b.pcm) {
			copy(s, b.pcm[start:])
		}
	}
	return Window{
		IntervalIndex: b.index,
		Number:        k,
		Total:         b.totalWindows,
		IsFinal:       k == b.totalWindows-1,
		Samples:       s,
	}
}

func (a *Assembler) newBuf(idx int64, tempoBPM float64) *intervalBuf {
	frames := a.cfg.IntervalSamples(a.sampleRate, tempoBPM)
	b := &intervalBuf{index: idx, pcm: make([]int16, frames*a.channels), frames: frames}
	if a.windowFrames > 0 {
		b.totalWindows = (frames + a.windowFrames - 1) / a.windowFrames
	}
	return b
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
