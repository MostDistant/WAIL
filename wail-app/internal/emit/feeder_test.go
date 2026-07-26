package emit

import "testing"

// Feeder tests use friendly numbers: sampleRate=1000, tempo=60 BPM → 1 beat/s
// → 1 beat = 1000 frames; chunk=10 frames, cushion=50 frames, interval=1000
// frames (exactly one beat). Source PCM is value-stamped with its frame index
// so emitted chunks reveal their position.
const (
	fRate    = 1000
	fTempo   = 60.0
	fChunk   = 10
	fCushion = 50
	fTotal   = 1000
)

// stampedSource returns mono PCM where sample i holds value base+i.
func stampedSource(frames int, base int16) []int16 {
	s := make([]int16, frames)
	for i := range s {
		s[i] = base + int16(i)
	}
	return s
}

type emitted struct {
	first int16
	n     int
	beat  float64
}

func collector(out *[]emitted) func([]int16, float64) {
	return func(s []int16, beat float64) {
		*out = append(*out, emitted{first: s[0], n: len(s), beat: beat})
	}
}

func newTestFeeder(startBeat float64, src []int16) (*Feeder, *PacedReader) {
	r := NewPacedReader(func() []int16 { return src }, 1, fRate, fTempo, startBeat, fTotal)
	f := NewFeeder(fCushion, fChunk)
	f.SetCurrent(0, r, nil)
	return f, r
}

func emittedFrames(out []emitted) int {
	n := 0
	for _, e := range out {
		n += e.n
	}
	return n
}

func TestFeederSetCushionGrows(t *testing.T) {
	var out []emitted
	f, _ := newTestFeeder(0, stampedSource(fTotal, 0))
	f.SetCushion(fCushion * 2)
	f.Advance(0, collector(&out))
	if got := emittedFrames(out); got != fCushion*2 {
		t.Fatalf("after SetCushion(%d): filled %d frames, want %d", fCushion*2, got, fCushion*2)
	}
}

// The default cushion is 0; a 0-frame cushion must still emit (floored to one
// chunk) rather than play silence.
func TestFeederZeroCushionStillEmits(t *testing.T) {
	var out []emitted
	r := NewPacedReader(func() []int16 { return stampedSource(fTotal, 0) }, 1, fRate, fTempo, 0, fTotal)
	f := NewFeeder(0, fChunk)
	f.SetCurrent(0, r, nil)
	f.Advance(0, collector(&out))
	if got := emittedFrames(out); got != fChunk {
		t.Fatalf("cushion 0: filled %d frames, want floored %d (silence otherwise)", got, fChunk)
	}
}

func TestFeederSetCushionFloorsToChunk(t *testing.T) {
	var out []emitted
	f, _ := newTestFeeder(0, stampedSource(fTotal, 0))
	f.SetCushion(1) // below chunk → floored to fChunk
	f.Advance(0, collector(&out))
	if got := emittedFrames(out); got != fChunk {
		t.Fatalf("after SetCushion(1): filled %d frames, want floored %d", got, fChunk)
	}
}

func TestFeederInitialFillToCushion(t *testing.T) {
	var out []emitted
	f, r := newTestFeeder(0, stampedSource(fTotal, 0))
	f.Advance(0, collector(&out))

	if got := emittedFrames(out); got != fCushion {
		t.Fatalf("initial fill = %d frames, want cushion %d", got, fCushion)
	}
	if r.Cursor() != fCushion {
		t.Fatalf("cursor = %d, want %d", r.Cursor(), fCushion)
	}
	if out[0].beat != 0 || out[0].first != 0 {
		t.Fatalf("first chunk beat=%v first=%d, want 0/0", out[0].beat, out[0].first)
	}
	// Stamps must be monotonic, 10-frame (0.01-beat) steps.
	for i := 1; i < len(out); i++ {
		if out[i].beat <= out[i-1].beat {
			t.Fatalf("non-monotonic stamps at %d", i)
		}
	}
	if ev, fr := f.Underruns(); ev != 0 || fr != 0 {
		t.Fatalf("unexpected underruns %d/%d", ev, fr)
	}
}

func TestFeederSteadyStateOneChunkPerTick(t *testing.T) {
	var out []emitted
	f, r := newTestFeeder(0, stampedSource(fTotal, 0))
	f.Advance(0, collector(&out))
	out = nil

	// Each 10-frame (0.01-beat) tick advances exactly one chunk.
	for tick := 1; tick <= 5; tick++ {
		f.Advance(float64(tick)*0.01, collector(&out))
	}
	if got := emittedFrames(out); got != 5*fChunk {
		t.Fatalf("steady state emitted %d frames over 5 ticks, want %d", got, 5*fChunk)
	}
	if r.Cursor() != fCushion+5*fChunk {
		t.Fatalf("cursor = %d, want %d", r.Cursor(), fCushion+5*fChunk)
	}
}

func TestFeederStallWithinCushionCatchesUpWithoutUnderrun(t *testing.T) {
	var out []emitted
	f, _ := newTestFeeder(0, stampedSource(fTotal, 0))
	f.Advance(0, collector(&out))
	out = nil

	// Stall: no ticks while the playhead moves 40 frames (< cushion 50).
	f.Advance(0.04, collector(&out))
	if got := emittedFrames(out); got != 40 {
		t.Fatalf("catch-up emitted %d frames, want 40", got)
	}
	if ev, _ := f.Underruns(); ev != 0 {
		t.Fatalf("stall within cushion must not count as underrun, got %d", ev)
	}
}

func TestFeederStallPastCushionCountsUnderrunAndSkips(t *testing.T) {
	var out []emitted
	f, r := newTestFeeder(0, stampedSource(fTotal, 0))
	f.Advance(0, collector(&out)) // cursor 50
	out = nil

	// Playhead jumps to frame 120: 70 frames past the cursor.
	f.Advance(0.12, collector(&out))
	ev, fr := f.Underruns()
	if ev != 1 || fr != 70 {
		t.Fatalf("underruns = %d events/%d frames, want 1/70", ev, fr)
	}
	// First emitted chunk must start AT the playhead (frame 120) — no stale stamps.
	if out[0].first != 120 {
		t.Fatalf("first post-skip chunk starts at frame %d, want 120", out[0].first)
	}
	if out[0].beat < 0.12-1e-9 {
		t.Fatalf("post-skip stamp %.9f is in the past (now 0.12)", out[0].beat)
	}
	if r.Cursor() != 120+fCushion {
		t.Fatalf("cursor = %d, want %d", r.Cursor(), 120+fCushion)
	}
}

// TestFeederSkipFramesReanchorsSilently: a grid snap moves the PLAYHEAD, not
// the audio — SkipFrames re-anchors the readers without counting, and the
// next Advance emits from the re-anchored position.
func TestFeederSkipFramesReanchorsSilently(t *testing.T) {
	var out []emitted
	f, r := newTestFeeder(0, stampedSource(fTotal, 0))
	f.Advance(0, collector(&out)) // cursor 50
	f.SkipFrames(100)             // the entry snap jumped the grid 100 frames
	if ev, fr := f.Underruns(); ev != 0 || fr != 0 {
		t.Fatalf("SkipFrames counted: %d events/%d frames, want 0/0", ev, fr)
	}
	if r.Cursor() != 150 {
		t.Fatalf("cursor = %d, want 150 (re-anchored)", r.Cursor())
	}
	out = nil
	f.Advance(0.15, collector(&out)) // playhead 150 == cursor → no underrun
	if ev, _ := f.Underruns(); ev != 0 {
		t.Fatalf("underrun after re-anchor: %d events", ev)
	}
	if out[0].first != 150 {
		t.Fatalf("post-re-anchor chunk starts at frame %d, want 150", out[0].first)
	}
}

func TestFeederPlayheadExactlyAtCursorNoUnderrun(t *testing.T) {
	var out []emitted
	f, _ := newTestFeeder(0, stampedSource(fTotal, 0))
	f.Advance(0, collector(&out)) // cursor 50
	f.Advance(0.05, collector(&out))
	if ev, _ := f.Underruns(); ev != 0 {
		t.Fatalf("playhead == cursor is not an underrun, got %d", ev)
	}
}

func TestFeederPreRollsNextIntervalAcrossBoundary(t *testing.T) {
	var out []emitted
	f, _ := newTestFeeder(0, stampedSource(fTotal, 0))
	made := 0
	next := NewPacedReader(func() []int16 { return stampedSource(fTotal, 5000) }, 1, fRate, fTempo, 1.0, fTotal)
	f.SetCurrent(0, NewPacedReader(func() []int16 { return stampedSource(fTotal, 0) }, 1, fRate, fTempo, 0, fTotal),
		func() (*PacedReader, int64, int) { made++; return next, 1, 0 })

	// Playhead at 970: cushion window reaches 1020 → 30 tail + 20 pre-roll.
	f.Advance(0.97, collector(&out))
	if made != 1 {
		t.Fatalf("makeNext called %d times, want 1", made)
	}
	if next.Cursor() != 20 {
		t.Fatalf("pre-rolled next cursor = %d, want 20", next.Cursor())
	}
	// Pre-rolled chunks must be stamped in interval 1's beat range (≥ 1.0).
	last := out[len(out)-1]
	if last.first != 5000+10 || last.beat < 1.0 {
		t.Fatalf("pre-roll chunk first=%d beat=%.3f, want 5010/≥1.0", last.first, last.beat)
	}
	// Further advance must not re-create next.
	f.Advance(0.98, collector(&out))
	if made != 1 {
		t.Fatalf("makeNext re-invoked (%d)", made)
	}
}

func TestFeederPromoteAdoptsPreRollWithoutReEmission(t *testing.T) {
	var out []emitted
	next := NewPacedReader(func() []int16 { return stampedSource(fTotal, 5000) }, 1, fRate, fTempo, 1.0, fTotal)
	f := NewFeeder(fCushion, fChunk)
	f.SetCurrent(0, NewPacedReader(func() []int16 { return stampedSource(fTotal, 0) }, 1, fRate, fTempo, 0, fTotal),
		func() (*PacedReader, int64, int) { return next, 1, 0 })
	f.Advance(0.97, collector(&out)) // pre-rolls next to 20

	if f.Promote(2, nil) {
		t.Fatal("Promote with mismatched idx must fail")
	}
	if !f.Promote(1, nil) {
		t.Fatal("Promote(1) should adopt the pre-rolled reader")
	}
	out = nil
	// Just past the boundary: playhead in interval 1 = 5 frames; cushion target 55;
	// frames 0..20 were already emitted — first new chunk must start at frame 20.
	f.Advance(1.005, collector(&out))
	if len(out) == 0 || out[0].first != 5000+20 {
		t.Fatalf("first post-promote chunk first=%d, want %d (no re-emission)", out[0].first, 5000+20)
	}
}

func TestFeederLiveAppendVisibleThroughCushion(t *testing.T) {
	var out []emitted
	src := make([]int16, 0, fTotal) // grows as frames "arrive"
	r := NewPacedReader(func() []int16 { return src }, 1, fRate, fTempo, 0, fTotal)
	f := NewFeeder(fCushion, fChunk)
	f.SetCurrent(0, r, nil)

	f.Advance(0, collector(&out)) // cushion reads 50 frames of nothing → silence
	for _, e := range out {
		if e.first != 0 {
			t.Fatal("not-yet-arrived frames must read as silence")
		}
	}
	// 100 frames arrive; the next tick's chunks (frames 50..60) must see them.
	src = stampedSource(100, 1)[:100]
	out = nil
	f.Advance(0.01, collector(&out))
	if len(out) == 0 || out[0].first != 1+50 {
		t.Fatalf("live-appended data not visible: first=%d, want %d", out[0].first, 1+50)
	}
}

func TestPacedReaderSkipAndRebase(t *testing.T) {
	r := NewPacedReader(func() []int16 { return stampedSource(fTotal, 0) }, 1, fRate, fTempo, 0, fTotal)
	r.Skip(100)
	if r.Cursor() != 100 {
		t.Fatalf("Skip: cursor = %d, want 100", r.Cursor())
	}
	r.Skip(50) // backward skip is a no-op
	if r.Cursor() != 100 {
		t.Fatal("Skip must never move backward")
	}
	beatBefore := r.FrameAtBeat(0.1) // frame 100 at old tempo
	if beatBefore != 100 {
		t.Fatalf("FrameAtBeat(0.1) = %d, want 100", beatBefore)
	}
	// Rebase to double tempo at cursor 100: beat 0.1 stays frame 100; future
	// beats advance twice as fast in frames-per-beat terms (120 BPM → 1 beat = 500 frames).
	r.Rebase(120, fTotal)
	if got := r.FrameAtBeat(0.1); got != 100 {
		t.Fatalf("rebase moved the anchor: FrameAtBeat(0.1) = %d, want 100", got)
	}
	if got := r.FrameAtBeat(0.6); got != 100+250 {
		t.Fatalf("post-rebase mapping = %d, want 350 (0.5 beat at 120bpm = 250 frames)", got)
	}
	s, beat, _ := r.Next(10)
	if len(s) != 10 || beat != 0.1 {
		t.Fatalf("Next after rebase: beat=%v, want 0.1", beat)
	}
}

func TestFeederFreshReaderFirstTickLagIsNotAnUnderrun(t *testing.T) {
	var out []emitted
	f, _ := newTestFeeder(0, stampedSource(fTotal, 0))
	// Boundaries are detected one tick late: the first Advance for a fresh
	// reader lands a chunk's worth past the start beat. Not a dropout.
	f.Advance(0.01, collector(&out)) // playhead 10 frames in, cursor 0
	if ev, _ := f.Underruns(); ev != 0 {
		t.Fatalf("first-tick lag counted as underrun (%d events)", ev)
	}
	// A big fresh-reader catch-up is setup, not loss — join warmup (the
	// first interval can't play before N+D) or the entry snap's grid jump.
	// In steady state SetCurrent only runs for a stream's first interval
	// (promote covers the rest), so big cursor-0 skips are setup by
	// construction (the ~500k join-time "underruns" field report). The skip
	// still happens; it just never counts.
	var out2 []emitted
	f2, _ := newTestFeeder(0, stampedSource(fTotal, 0))
	f2.Advance(0.5, collector(&out2)) // playhead 500 frames in, cursor 0
	if ev, _ := f2.Underruns(); ev != 0 {
		t.Fatalf("fresh-reader catch-up counted as underrun (%d events)", ev)
	}
	if out2[0].first != 500 {
		t.Fatalf("first chunk starts at frame %d, want 500 (skip still happens)", out2[0].first)
	}
}

func TestPacedReaderSetTotalFramesExtends(t *testing.T) {
	r := NewPacedReader(func() []int16 { return stampedSource(1050, 0) }, 1, fRate, fTempo, 0, fTotal)
	r.Skip(fTotal)
	if s, _, done := r.Next(10); len(s) != 0 || !done {
		t.Fatal("reader should be exhausted at totalFrames")
	}
	r.SetTotalFrames(1050)
	s, beat, done := r.Next(10)
	if len(s) != 10 || done || s[0] != 1000 {
		t.Fatalf("extension not readable: n=%d first=%d done=%v", len(s), s[0], done)
	}
	// Stamps stay on the same linear grid: frame 1000 begins at beat 1.0.
	if beat != 1.0 {
		t.Fatalf("extended frame stamped %.4f, want 1.0", beat)
	}
}

// The interval handoff for continuation-padded senders: makeNext extends the
// playing reader past its interval end (the pad carries real audio) and the
// next reader starts past its twice-encoded head. The emitted stream must stay
// contiguous in both content and stamps.
func TestFeederHandoffExtendsCurrentAndSkipsNext(t *testing.T) {
	const pad = 50
	var out []emitted
	cur := NewPacedReader(func() []int16 { return stampedSource(fTotal+pad, 0) }, 1, fRate, fTempo, 0, fTotal)
	next := NewPacedReader(func() []int16 { return stampedSource(fTotal, 5000) }, 1, fRate, fTempo, 1.0, fTotal)
	f := NewFeeder(fCushion, fChunk)
	f.SetCurrent(0, cur, func() (*PacedReader, int64, int) {
		cur.SetTotalFrames(fTotal + pad)
		return next, 1, pad
	})

	// Steady ticks to 970; the cushion crosses the unextended end (frame 1000)
	// on the way, so the handoff runs — but the cushion target stays inside
	// the extended current reader.
	for b := 0.0; b < 0.965; b += 0.01 {
		f.Advance(b, collector(&out))
	}
	f.Advance(0.97, collector(&out))
	if next.Cursor() != pad {
		t.Fatalf("next cursor = %d, want start offset %d", next.Cursor(), pad)
	}
	last := out[len(out)-1]
	if last.first+int16(last.n) != 1020 {
		t.Fatalf("cushion should fill current to frame 1020, last chunk %d+%d", last.first, last.n)
	}

	// Steady ticks to 1040: current finishes at 1050, next fills 50..90.
	out = nil
	f.Advance(0.99, collector(&out))
	f.Advance(1.01, collector(&out))
	f.Advance(1.04, collector(&out))
	if cur.Cursor() != fTotal+pad {
		t.Fatalf("current cursor = %d, want %d", cur.Cursor(), fTotal+pad)
	}
	if next.Cursor() != pad+40 {
		t.Fatalf("next cursor = %d, want %d", next.Cursor(), pad+40)
	}
	// Content and stamps must be contiguous through the handoff: current's
	// extension (frames 1020..1050 = values 1020..1049), then next from its
	// frame 50 (value 5050) stamped at beat 1.05.
	seenSwitch := false
	for i, e := range out {
		if e.first == 5000+pad {
			seenSwitch = true
			if prev := out[i-1]; int(prev.first)+prev.n != fTotal+pad {
				t.Fatalf("switch not contiguous: prev chunk ends at %d, want %d", int(prev.first)+prev.n, fTotal+pad)
			}
			if e.beat < 1.05-1e-9 || e.beat > 1.05+1e-9 {
				t.Fatalf("first next chunk stamped %.5f, want 1.05", e.beat)
			}
		}
	}
	if !seenSwitch {
		t.Fatal("never switched to the next reader")
	}
	if ev, fr := f.Underruns(); ev != 0 || fr != 0 {
		t.Fatalf("handoff produced underruns: %d/%d", ev, fr)
	}
}
