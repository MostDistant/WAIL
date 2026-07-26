// Package playout holds the receiver-side hold-until-boundary scheduler that
// enforces WAIL's interval offset D (ADR-0003).
//
// Remote audio captured during room interval N is released for playback at the
// local boundary labeled N+D. The offset is enforced here by the receiver, not
// by network timing, so it is a fixed musical constant: as long as delivery
// beats the boundary the delay is exactly D, jitter-proof.
//
// The scheduler is a pure timing state machine — it decides *which* interval to
// release at a boundary and *how* to treat a freshly-arrived frame (buffer it,
// live-append it to the interval currently playing, or drop it as too late). It
// does not own PCM; the caller keeps the decoded buffers keyed by index and asks
// the scheduler what to do. One scheduler per remote (identity, stream).
package playout

// Disposition tells the caller what to do with a decoded frame for some index.
type Disposition int

const (
	// Buffer: the frame belongs to a future interval; hold it until its boundary.
	Buffer Disposition = iota
	// LiveAppend: the frame belongs to the interval currently playing; it arrived
	// after the boundary (play-partial) and must be spliced into playout now.
	LiveAppend
	// TooLate: the interval has already finished playing; drop the frame.
	TooLate
)

func (d Disposition) String() string {
	switch d {
	case Buffer:
		return "buffer"
	case LiveAppend:
		return "live-append"
	case TooLate:
		return "too-late"
	default:
		return "unknown"
	}
}

// Scheduler tracks the playout position for one remote stream.
type Scheduler struct {
	offsetD    int64
	playing    int64
	hasPlaying bool
}

// New creates a scheduler with interval offset D (clamped to >= 0). D is the
// receiver-enforced NINJAM delay; it is per-client, not advertised in the room
// (resolves migration-plan open question §71 — D is a local reliability/latency
// knob each receiver sets for itself).
func New(offsetD int) *Scheduler {
	if offsetD < 0 {
		offsetD = 0
	}
	return &Scheduler{offsetD: int64(offsetD)}
}

// Offset returns the configured interval offset D.
func (s *Scheduler) Offset() int64 { return s.offsetD }

// SetOffset live-adjusts D (clamped ≥ 0); future releases use the new value
// (the interval in flight keeps playing — D is a release-time decision).
func (s *Scheduler) SetOffset(d int64) {
	if d < 0 {
		d = 0
	}
	s.offsetD = d
}

// Playing returns the index currently being played and whether playout has begun.
func (s *Scheduler) Playing() (int64, bool) { return s.playing, s.hasPlaying }

// OnFrame decides what the caller should do with a decoded frame for room
// interval idx, given the current playout position.
func (s *Scheduler) OnFrame(idx int64) Disposition {
	if !s.hasPlaying || idx > s.playing {
		return Buffer
	}
	if idx == s.playing {
		return LiveAppend
	}
	return TooLate
}

// OnBoundary advances playout to the local boundary labeled `label` and returns
// the interval index to release now (label - D). advanced is false for a
// duplicate or non-monotonic boundary (nothing new to release), in which case
// the returned index is the interval already playing and the caller should not
// re-release it.
func (s *Scheduler) OnBoundary(label int64) (releaseIdx int64, advanced bool) {
	idx := label - s.offsetD
	if s.hasPlaying && idx <= s.playing {
		return s.playing, false
	}
	s.playing = idx
	s.hasPlaying = true
	return idx, true
}
