package emit

// Label offset confirmation (ADR-0006 follow-up): a healthy remote stream's
// WAIF frames carry room interval index == the receiver's current room index —
// the peer's interval N streams in during our interval N and plays at our
// boundary N+D. Because the playout scheduler releases whatever label it gets,
// a peer whose labels are off by k intervals is NOT glitchy — their audio just
// plays k intervals late, silently. This tracker makes that visible: it buckets
// incoming frames by (frame label − local room index) and, at each interval
// roll, finalizes the modal delta. Mode 0 (with rare −1 boundary stragglers) is
// healthy; any other persistent mode means the peer is mislabeled by exactly
// that many intervals.

const (
	// labelOffsetRange clamps tracked deltas to ±labelOffsetRange intervals.
	labelOffsetRange = 4
	// labelOffsetMinFrames is the minimum frames in an interval before a
	// verdict is finalized (an interval at any normal tempo has hundreds).
	labelOffsetMinFrames = 10
	// labelOffsetPersistIntervals is how many consecutive finalized intervals a
	// NONZERO delta must hold before it is reported. One odd interval is what a
	// join transition looks like while the labeler settles, and reporting it
	// claims a peer plays intervals late when nothing is wrong — indistinguishable
	// from the real fault it exists to catch. A genuine offset is stuck, so it
	// survives the wait and is reported one interval later.
	labelOffsetPersistIntervals = 2
)

// LabelOffsetTracker accumulates per-interval label deltas for one remote
// stream. Not safe for concurrent use — the caller serializes (engine lock).
type LabelOffsetTracker struct {
	countIdx int64
	started  bool
	counts   [2*labelOffsetRange + 1]uint64
	total    uint64
	verdict  int64
	valid    bool
	// pending is the modal delta awaiting confirmation and pendingCount how
	// many consecutive finalized intervals have agreed on it — the hold-down
	// that keeps a join transition from reading as a mislabeled peer.
	pending      int64
	pendingCount int
}

// Add records one frame's label against the current local room index. When the
// room index has rolled since the last frame, the previous interval's verdict
// is finalized first; Add then reports (verdict, true) if the verdict changed.
// verdict is the modal (frame label − room index) of the completed interval:
// 0 = healthy, k = peer's labels are k intervals off. Sign convention
// (source of truth): positive = the peer's frames are labeled AHEAD of our
// room index, so playout (release = label − D) holds them extra boundaries —
// their audio plays k intervals LATE; negative = early.
func (t *LabelOffsetTracker) Add(roomIdx, frameIdx int64) (int64, bool) {
	changed := false
	if !t.started || t.countIdx != roomIdx {
		if t.started && t.total >= labelOffsetMinFrames {
			v := t.modalDelta()
			if v == t.pending {
				t.pendingCount++
			} else {
				t.pending, t.pendingCount = v, 1
			}
			// Healthy (0) publishes at once — recovery is always safe to report,
			// and holding it down would leave a stale fault on the books. A
			// nonzero delta waits for confirmation. Intervals too sparse to
			// finalize neither confirm nor break a run: they carry no evidence
			// either way.
			if v == 0 || t.pendingCount >= labelOffsetPersistIntervals {
				if !t.valid || v != t.verdict {
					t.verdict, t.valid = v, true
					changed = true
				}
			}
		}
		t.counts = [2*labelOffsetRange + 1]uint64{}
		t.total = 0
		t.countIdx = roomIdx
		t.started = true
	}
	d := frameIdx - roomIdx
	if d < -labelOffsetRange {
		d = -labelOffsetRange
	}
	if d > labelOffsetRange {
		d = labelOffsetRange
	}
	t.counts[d+labelOffsetRange]++
	t.total++
	if !changed {
		return 0, false
	}
	return t.verdict, true
}

// Verdict returns the modal label delta of the last completed interval, or
// false if no interval has finalized yet (or it had too few frames).
func (t *LabelOffsetTracker) Verdict() (int64, bool) {
	return t.verdict, t.valid
}

func (t *LabelOffsetTracker) modalDelta() int64 {
	best, bestCount := int64(0), uint64(0)
	for d := int64(-labelOffsetRange); d <= labelOffsetRange; d++ {
		if c := t.counts[d+labelOffsetRange]; c > bestCount {
			best, bestCount = d, c
		}
	}
	return best
}
