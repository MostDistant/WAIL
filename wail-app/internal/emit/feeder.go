package emit

// Feeder keeps a Link Audio sink fed a bounded cushion AHEAD of the wall-clock
// playhead. The previous design wrote exactly one chunk per engine tick (1×
// real time, zero cushion), so any scheduler/GC stall longer than the DAW-side
// buffer was an audible gap. The cushion buys stall tolerance; receivers
// tolerate near-future stamps (the reference renderer stalls on far-future
// buffers and plays ~4 beats behind — docs/link-audio-research.md §1.4), so a
// cushion of tens of ms stays "≈ local now".
//
// The feeder owns the playing interval's reader plus a lazily pre-rolled
// reader for the NEXT interval: when the cushion window crosses the interval
// end, it starts filling the next interval's head so boundaries don't reset
// the cushion to zero. Promote adopts the pre-rolled reader at the real
// boundary — committed beats are never re-emitted.
//
// Trade-off: cushion C costs C of live-append lateness margin (frames arriving
// later than "needed at playhead + C" play as silence). At D=1 the measured
// margin is ~a full interval, so tens of ms are immaterial.
//
// Not safe for concurrent use — drive from the emit loop under the engine lock.
type Feeder struct {
	cur, next       *PacedReader
	curIdx, nextIdx int64
	nextStart       int // frame the next reader starts at (continuation handoff)
	makeNext        func() (*PacedReader, int64, int)
	cushionFrames   int
	chunkFrames     int

	underrunEvents uint64
	underrunFrames uint64
}

// NewFeeder creates a feeder emitting chunkFrames-sized chunks, keeping the
// cursor cushionFrames ahead of the playhead.
func NewFeeder(cushionFrames, chunkFrames int) *Feeder {
	if chunkFrames < 1 {
		chunkFrames = 1
	}
	if cushionFrames < chunkFrames {
		cushionFrames = chunkFrames
	}
	return &Feeder{cushionFrames: cushionFrames, chunkFrames: chunkFrames}
}

// SetCushion changes the feed-ahead depth live. Re-floors to chunkFrames (as
// NewFeeder does). Safe mid-stream: the cursor only ever moves forward, so a
// larger cushion just fills further on the next Advance and a smaller one makes
// Advance a no-op until the playhead catches up — committed beats never re-emit.
// Drive from the emit loop under the engine lock (not concurrent-safe).
func (f *Feeder) SetCushion(cushionFrames int) {
	if cushionFrames < f.chunkFrames {
		cushionFrames = f.chunkFrames
	}
	f.cushionFrames = cushionFrames
}

// SetCurrent installs the playing interval's reader. makeNext lazily builds
// the following interval's reader for boundary pre-roll (nil disables
// pre-roll); it runs once, when the cushion first crosses the current
// interval's end, and returns the next reader, its index, and the frame it
// starts at. It may extend the current reader (SetTotalFrames) when the
// interval's continuation padding carries real audio.
func (f *Feeder) SetCurrent(idx int64, r *PacedReader, makeNext func() (*PacedReader, int64, int)) {
	f.cur, f.curIdx = r, idx
	f.next, f.nextIdx, f.nextStart = nil, 0, 0
	f.makeNext = makeNext
}

// Promote adopts the pre-rolled next reader as current (at the real boundary).
// Returns false when no pre-roll exists for idx — the caller falls back to
// SetCurrent with a fresh reader.
func (f *Feeder) Promote(idx int64, makeNext func() (*PacedReader, int64, int)) bool {
	if f.next == nil || f.nextIdx != idx {
		return false
	}
	f.cur, f.curIdx = f.next, f.nextIdx
	f.next, f.nextIdx, f.nextStart = nil, 0, 0
	f.makeNext = makeNext
	return true
}

// Current returns the playing interval's reader (nil before SetCurrent).
func (f *Feeder) Current() *PacedReader { return f.cur }

// SkipFrames re-anchors the readers after an entry-conformance grid snap
// (ADR-0006): the snap moved the playhead, not the audio — the jumped frames
// are dead work (stale stamps), so skip them WITHOUT counting underruns. The
// engine calls it via OnGridSnap with the snap's delta in frames.
func (f *Feeder) SkipFrames(frames int) {
	if frames <= 0 {
		return
	}
	if f.cur != nil {
		f.cur.Skip(f.cur.Cursor() + frames)
	}
	if f.next != nil {
		f.next.Skip(f.next.Cursor() + frames)
	}
}

// Underruns returns cumulative underrun events and skipped frames — the feed
// fell behind the playhead past the cushion: audio was late to the sink and
// the shortfall played as silence (the honest audible-dropout metric).
func (f *Feeder) Underruns() (events, frames uint64) {
	return f.underrunEvents, f.underrunFrames
}

// Advance tops the sink up to playhead+cushion at wall-clock beat nowBeat,
// emitting as many chunks as needed (catch-up burst after a stall), skipping
// ahead when the playhead overtook the cursor, and pre-rolling the next
// interval when the cushion window crosses the interval end. The handoff
// (makeNext) may extend the current reader, so it runs before the crossover
// math and the current reader is topped up again afterwards.
func (f *Feeder) Advance(nowBeat float64, emit func(samples []int16, beatAtBegin float64)) {
	if f.cur == nil {
		return
	}
	play := f.cur.FrameAtBeat(nowBeat)
	if play < 0 {
		play = 0
	}
	f.fill(f.cur, play, play+f.cushionFrames, emit)

	if play+f.cushionFrames <= f.cur.TotalFrames() {
		return
	}
	if f.next == nil {
		if f.makeNext == nil {
			return
		}
		f.next, f.nextIdx, f.nextStart = f.makeNext()
		if f.next == nil {
			return
		}
		if f.next.Cursor() < f.nextStart {
			f.next.Skip(f.nextStart)
		}
		f.fill(f.cur, play, play+f.cushionFrames, emit)
	}
	over := play + f.cushionFrames - f.cur.TotalFrames()
	if over <= 0 {
		return
	}
	overPlay := play - f.cur.TotalFrames()
	if overPlay < 0 {
		overPlay = 0
	}
	f.fill(f.next, f.nextStart+overPlay, f.nextStart+over, emit)
}

// fill brings one reader's cursor up to targetFrame, first skipping any
// shortfall behind playFrame — stale-stamped chunks would be dropped by
// receivers, so emitting them is dead work. Only MID-STREAM shortfall counts
// as an underrun: a fresh reader's catch-up (cursor 0) is always setup —
// join warmup (the first interval can't play before N+D) or the entry snap's
// grid jump — and the skipped frames were never playable audio (the ~500k
// join-time "underruns" field report). The skip itself always happens.
func (f *Feeder) fill(r *PacedReader, playFrame, targetFrame int, emit func([]int16, float64)) {
	if playFrame > r.Cursor() && r.Cursor() < r.TotalFrames() {
		skipTo := playFrame
		if skipTo > r.TotalFrames() {
			skipTo = r.TotalFrames()
		}
		if r.Cursor() > 0 {
			f.underrunEvents++
			f.underrunFrames += uint64(skipTo - r.Cursor())
		}
		r.Skip(skipTo)
	}
	if targetFrame > r.TotalFrames() {
		targetFrame = r.TotalFrames()
	}
	for r.Cursor() < targetFrame {
		n := f.chunkFrames
		if r.Cursor()+n > targetFrame {
			n = targetFrame - r.Cursor()
		}
		samples, beat, _ := r.Next(n)
		if len(samples) == 0 {
			return
		}
		emit(samples, beat)
	}
}
