// Package capture holds the pure interval-assembly logic for the Link Audio
// capture side: it buckets received audio buffers (each carrying a local begin
// beat) into fixed-length NINJAM intervals. The realtime source callback and
// Opus/WAIF/relay plumbing live in package main and wrap this proven logic.
package capture

import (
	"github.com/nicholasgasior/wail/wail-app/internal/dsp"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// CompletedInterval is a fully-bucketed interval ready to encode. Samples is the
// whole interval, zero-padded (NINJAM intervals are fixed length; capture gaps
// read as silence and are surfaced separately via LAN-loss metrics). Index is
// the *local* interval index; the caller labels it with the shared room index.
type CompletedInterval struct {
	Index         int64
	Samples       []int16 // interleaved, Frames × Channels
	Frames        int     // full interval length in frames
	Channels      int
	WrittenFrames int // frames actually covered by received buffers (<= Frames)
}

// Complete reports whether the whole interval was covered by received audio.
func (c *CompletedInterval) Complete() bool { return c.WrittenFrames >= c.Frames }

// Assembler buckets one capture channel's buffers into intervals. It assumes
// samples arrive at the internal rate (48 kHz); the caller resamples at the edge
// if the channel differs. Not safe for concurrent use — drive from the drain
// goroutine.
//
// Placement is sample-contiguous: consecutive buffers are laid down back to
// back, and beat stamps only anchor the stream — at the first buffer, after
// Reanchor(), or when a stamp diverges from the contiguous cursor by more than
// reanchorThreshold (a genuine discontinuity: source stop/start, transport
// jump, capture stall). Trusting per-buffer stamps for position turns
// sender-side clock drift into zero-gaps punched into continuous audio
// (measured live 2026-07: ~5-frame gaps every 64000 samples plus a growing
// ~100-frame pop at every interval boundary — audible clicks).
type Assembler struct {
	cfg         interval.Config
	channels    int
	sampleRate  uint32
	cur         *intervalBuf
	droppedLate uint64

	// Contiguous placement cursor (frames within cur) + anchoring state.
	nextOff  int
	anchored bool
	resnaps  uint64

	// Micro-slew drift correction: when the stamp-vs-cursor divergence leaves
	// the deadband (but stays under the re-anchor threshold), the next buffer's
	// tail is stretched/compressed by a few frames so the cursor tracks the
	// beat grid — bounding drift-as-latency without a single splice.
	pendingSlew  int
	slewedFrames uint64

	// Windowed (streaming) mode: 0 in batch mode.
	windowFrames    int
	droppedBackfill uint64

	// A short final window is held here until the next interval's head arrives
	// to pad it: feeding the encoder zeros instead splices a hard transient
	// into every boundary of the decoded stream. Zero-padded only when no
	// continuation can exist (flush, re-anchor discontinuity).
	pending *intervalBuf
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

// Config returns the interval grid the assembler is bucketing on.
func (a *Assembler) Config() interval.Config { return a.cfg }

// SetConfig switches the assembler to a new interval grid after a room config
// change, dropping any partial old-grid interval (a bounded one-time hole) and
// re-anchoring from the next buffer's beat. Without it the assembler keeps
// bucketing on the old grid while the room labeler maps on the new one, so its
// labels tick at the old rate against the room clock and the stream drifts
// further out of sync every interval — and a BPI increase would freeze it
// entirely (every new index reads as "late"). Dropping beats flushing here:
// the labeler is already re-aligned to the new grid, so old-grid windows would
// only be mislabeled.
func (a *Assembler) SetConfig(cfg interval.Config) {
	if cfg == a.cfg {
		return
	}
	a.cfg = cfg
	a.cur = nil
	a.pending = nil
	a.anchored = false
}

// reanchorThreshold is the stamp-vs-cursor divergence (frames) beyond which the
// beat stamp wins over sample contiguity. 250ms: far above clock-drift noise,
// far below any musically meaningful discontinuity.
func (a *Assembler) reanchorThreshold() int64 { return int64(a.sampleRate) / 4 }

// slewDeadband is the divergence (frames) below which stamps are treated as
// noise (10ms). Past it, the slew corrector gently pulls the cursor back to
// the beat grid.
func (a *Assembler) slewDeadband() int64 {
	if db := int64(a.sampleRate) / 100; db > 1 {
		return db
	}
	return 1
}

// slewAdjust applies a ±k-frame drift correction by linearly resampling the
// buffer's last slewWindow frames to slewWindow+k: the tail is stretched or
// compressed by ~1.3ms worth of audio — no splice, imperceptible pitch bend.
// Buffers too short to smear the correction pass through (the divergence
// persists, so the next buffer re-arms it).
func (a *Assembler) slewAdjust(samples []int16, numFrames, k int) ([]int16, int) {
	if k == 0 || numFrames < slewWindow+slewMaxPerBuffer {
		return samples, numFrames
	}
	headFrames := numFrames - slewWindow
	out := make([]int16, 0, (numFrames+k)*a.channels)
	out = append(out, samples[:headFrames*a.channels]...)
	tail := samples[headFrames*a.channels : numFrames*a.channels]
	out = append(out, dsp.ResampleLinearInterleaved(tail, a.channels, slewWindow, slewWindow+k)...)
	if k < 0 {
		a.slewedFrames += uint64(-k)
	} else {
		a.slewedFrames += uint64(k)
	}
	return out, len(out) / a.channels
}

const (
	// slewMaxPerBuffer caps the correction per incoming buffer (frames).
	slewMaxPerBuffer = 4
	// slewWindow is the tail span (frames) a correction is smeared over via
	// linear resampling — ~1.3ms at 48k: no splice, imperceptible pitch bend.
	slewWindow = 64
)

// SlewedFrames is the cumulative number of frames inserted or dropped by the
// drift corrector (each an inaudible micro-stretch, unlike Resnaps).
func (a *Assembler) SlewedFrames() uint64 { return a.slewedFrames }

// Reanchor tells the assembler the sample stream has a genuine discontinuity
// (e.g. the capture hop lost buffers): the next buffer is placed by its beat
// stamp instead of contiguously, so the lost span honestly reads as silence.
func (a *Assembler) Reanchor() { a.anchored = false }

// Resnaps counts automatic re-anchors (stamp divergence over the threshold).
func (a *Assembler) Resnaps() uint64 { return a.resnaps }

// place positions the stream cursor for a buffer stamped at `beat`: contiguous
// by default, anchored from the stamp on the first buffer, after Reanchor(),
// or past-threshold divergence. ok=false drops a buffer stamped into an
// already-emitted interval. flushedC/flushedW carry the previous interval's
// remainder when anchoring advances past it (batch/windowed respectively).
func (a *Assembler) place(beat, tempoBPM float64, windowed bool) (flushedC *CompletedInterval, flushedW []Window, ok bool) {
	beatIdx := a.cfg.IndexAtBeat(beat)
	if a.cur == nil {
		a.cur = a.newBuf(beatIdx, tempoBPM)
		a.nextOff = a.cfg.FrameOffset(beat, beatIdx, a.sampleRate, tempoBPM)
		a.anchored = true
		return nil, nil, true
	}

	beatOff := a.cfg.FrameOffset(beat, beatIdx, a.sampleRate, tempoBPM)
	div := (beatIdx-a.cur.index)*int64(a.cur.frames) + int64(beatOff-a.nextOff)
	if a.anchored && div <= a.reanchorThreshold() && div >= -a.reanchorThreshold() {
		// Stay contiguous. Past the deadband, arm a micro-slew on this buffer
		// so accumulated clock drift is corrected instead of growing without
		// bound into an eventual re-anchor splice.
		switch db := a.slewDeadband(); {
		case div > db:
			a.pendingSlew = int(min(div, slewMaxPerBuffer))
		case div < -db:
			a.pendingSlew = -int(min(-div, slewMaxPerBuffer))
		}
		return nil, nil, true
	}

	// Genuine discontinuity: re-anchor from the stamp.
	if beatIdx < a.cur.index {
		a.droppedLate++
		return nil, nil, false
	}
	if a.anchored {
		a.resnaps++
	}
	a.anchored = true
	if beatIdx > a.cur.index {
		if windowed {
			flushedW = append(a.flushPending(), a.remainingWindows()...)
		} else {
			flushedC = a.finish()
		}
		a.cur = a.newBuf(beatIdx, tempoBPM)
	}
	a.nextOff = beatOff
	return flushedC, flushedW, true
}

// Add places a buffer beginning at local beat `beat` (from Source.BeginBeats)
// into its interval, contiguously with the previous buffer. It returns a
// completed interval when one closes (anchoring advanced past it, or the
// buffer's samples spanned its end), otherwise nil.
func (a *Assembler) Add(beat, tempoBPM float64, samples []int16, numFrames int) *CompletedInterval {
	completed, _, ok := a.place(beat, tempoBPM, false)
	if !ok {
		return nil
	}
	if k := a.pendingSlew; k != 0 {
		a.pendingSlew = 0
		samples, numFrames = a.slewAdjust(samples, numFrames, k)
	}
	for numFrames > 0 {
		if a.nextOff >= a.cur.frames {
			c := a.finish()
			if completed == nil {
				completed = c
			}
			a.cur = a.newBuf(a.cur.index+1, tempoBPM)
			a.nextOff = 0
		}
		n := a.cur.frames - a.nextOff
		if n > numFrames {
			n = numFrames
		}
		a.write(a.nextOff, samples[:n*a.channels], n)
		a.nextOff += n
		samples = samples[n*a.channels:]
		numFrames -= n
	}
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
// a window is ready once coverage passes its end. A buffer whose samples span
// an interval's end fills its tail and continues into the next interval — no
// boundary gap. When anchoring advances past the current interval (a genuine
// discontinuity), its remaining windows flush first (zero-padded — capture
// gaps read as silence), ending with its final window.
//
// Emitted windows are immutable: after a backward re-anchor, a buffer that
// lands behind the emitted boundary is trimmed to it (fully-behind buffers are
// dropped) and counted in DroppedBackfill.
func (a *Assembler) AddWindows(beat, tempoBPM float64, samples []int16, numFrames int) []Window {
	_, out, ok := a.place(beat, tempoBPM, true)
	if !ok {
		return nil
	}
	if k := a.pendingSlew; k != 0 {
		a.pendingSlew = 0
		samples, numFrames = a.slewAdjust(samples, numFrames, k)
	}
	for numFrames > 0 {
		if a.nextOff >= a.cur.frames {
			if a.shortFinal(a.cur) && a.cur.emitted == a.cur.totalWindows-1 {
				a.pending = a.cur // hold the short final for its continuation
			} else {
				out = append(out, a.remainingWindows()...) // no-op after exact coverage
			}
			a.cur = a.newBuf(a.cur.index+1, tempoBPM)
			a.nextOff = 0
		}
		if lim := a.cur.emitted * a.windowFrames; a.nextOff < lim {
			a.droppedBackfill++
			skip := lim - a.nextOff
			if skip >= numFrames {
				return out
			}
			samples = samples[skip*a.channels:]
			numFrames -= skip
			a.nextOff = lim
		}
		n := a.cur.frames - a.nextOff
		if n > numFrames {
			n = numFrames
		}
		a.write(a.nextOff, samples[:n*a.channels], n)
		a.nextOff += n
		samples = samples[n*a.channels:]
		numFrames -= n
		out = append(out, a.releasePending()...)
		out = append(out, a.readyWindows()...)
	}
	return out
}

// shortFinal reports whether b's final window is shorter than a full window
// (the interval length isn't window-aligned) and so needs continuation padding.
func (a *Assembler) shortFinal(b *intervalBuf) bool {
	return a.windowFrames > 0 && b.frames%a.windowFrames != 0
}

// releasePending emits the held final window once its successor interval has
// accumulated enough head samples to pad it with real audio.
func (a *Assembler) releasePending() []Window {
	p := a.pending
	if p == nil {
		return nil
	}
	pad := p.totalWindows*a.windowFrames - p.frames
	if a.cur == nil || a.cur.index != p.index+1 || a.cur.writtenFrames < pad {
		return nil
	}
	a.pending = nil
	return []Window{a.finalWindow(p, a.cur.pcm[:pad*a.channels])}
}

// flushPending emits the held final window zero-padded — for paths where no
// continuation can exist (flush, re-anchor discontinuity).
func (a *Assembler) flushPending() []Window {
	p := a.pending
	if p == nil {
		return nil
	}
	a.pending = nil
	return []Window{a.finalWindow(p, nil)}
}

// finalWindow builds b's final window: the interval tail plus continuation
// samples (or zeros when cont is nil/short).
func (a *Assembler) finalWindow(b *intervalBuf, cont []int16) Window {
	k := b.totalWindows - 1
	s := make([]int16, a.windowFrames*a.channels)
	start := k * a.windowFrames * a.channels
	tail := copy(s, b.pcm[start:])
	copy(s[tail:], cont)
	return Window{
		IntervalIndex: b.index,
		Number:        k,
		Total:         b.totalWindows,
		IsFinal:       true,
		Samples:       s,
	}
}

// FlushWindows emits any held final window plus the current interval's
// remaining windows (zero-padded, ending with its final window) and clears the
// assembler — the streaming counterpart of Flush, for shutdown / channel
// removal.
func (a *Assembler) FlushWindows() []Window {
	w := a.flushPending()
	if a.cur != nil {
		w = append(w, a.remainingWindows()...)
		a.cur = nil
	}
	if len(w) == 0 {
		return nil
	}
	return w
}

// DroppedBackfill is the number of buffers trimmed or dropped for landing
// behind the emitted-window boundary (windowed mode only).
func (a *Assembler) DroppedBackfill() uint64 { return a.droppedBackfill }

// readyWindows emits every window whose end is covered. Exact full coverage
// also readies a full-length final window; a short final is withheld for the
// continuation-pad path (releasePending / flushPending).
func (a *Assembler) readyWindows() []Window {
	b := a.cur
	full := b.totalWindows
	if a.shortFinal(b) {
		full = b.totalWindows - 1
	}
	ready := b.writtenFrames / a.windowFrames
	if b.writtenFrames >= b.frames || ready > full {
		ready = full
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
