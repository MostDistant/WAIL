package playout

import "sort"

// Adaptive is the ADR-0009 scheduler: rounds are the sender's own interval
// indices — an opaque, monotonic sequence needing no room-wide agreement — and
// each round plays at the receiver's next local boundary once it is ready.
// When a backlog forms, the freshest ready round plays and the stale queue is
// skipped at the speakers (NINJAM's rule, njclient.cpp:1305); skipped rounds
// are already in the archive, because recording taps frames at receipt.
//
// This replaces Scheduler's hold-until-label-N+D: there is no D and no label
// arithmetic, so the heard delay is adaptive — bounded by the interval,
// self-healing with network weather — which is NINJAM's actual behavior
// rather than the folkloric fixed interval. One Adaptive per sender identity,
// shared by all of that sender's streams, so a musician's mic and guitar can
// never split across rounds.
type Adaptive struct {
	playing    int64
	hasPlaying bool
	boundary   int64
	hasBound   bool
	skipped    uint64
}

// RoundState is the caller's view of one buffered round at a boundary.
type RoundState struct {
	Index    int64
	Complete bool
	// FirstSeen is the receiver's boundary count at the moment the round's
	// first frame arrived. A round that started arriving during the current
	// boundary window has had no streaming time and is not ready unless
	// complete; one boundary of age means its tail has an interval's worth of
	// time to keep pace, which is the live-append contract.
	FirstSeen int64
}

// restartGap is how far below the playing round a candidate must sit before it
// reads as a sender restart (indices reset near zero) rather than a straggler.
// Stragglers trail by a round or two; a restarted sender is hundreds below.
const restartGap = 8

// Playing returns the round currently being played and whether playout began.
func (a *Adaptive) Playing() (int64, bool) { return a.playing, a.hasPlaying }

// Skipped returns how many rounds freshest-wins has skipped at the speakers.
func (a *Adaptive) Skipped() uint64 { return a.skipped }

// OnFrame decides what to do with a decoded frame for round idx: future
// rounds buffer, the playing round live-appends, finished rounds are too late
// (for the speakers — the recorder already has them). A round far below the
// playing one is not a straggler but a sender restart (their indices reset
// near zero): it buffers, so OnBoundary's re-pin has something to re-pin to —
// without this the restarted sender's audio would be dropped at the door and
// the re-pin path could never fire.
func (a *Adaptive) OnFrame(idx int64) Disposition {
	if !a.hasPlaying || idx > a.playing {
		return Buffer
	}
	if idx == a.playing {
		return LiveAppend
	}
	if idx <= a.playing-restartGap {
		return Buffer
	}
	return TooLate
}

// OnBoundary picks the round to release at this local boundary, given every
// round the caller has buffered (any order; filtering is the scheduler's job).
// advanced is false when nothing is ready — an idle or behind sender simply
// does not advance, and the feeder pads. skipped lists unplayed rounds passed
// over by freshest-wins, oldest first; the caller drops their buffers.
func (a *Adaptive) OnBoundary(boundary int64, buffered []RoundState) (release int64, skipped []int64, advanced bool) {
	if a.hasBound && boundary <= a.boundary {
		return a.playing, nil, false
	}
	a.boundary, a.hasBound = boundary, true

	ready := func(r RoundState) bool { return r.Complete || r.FirstSeen < boundary }

	cands := make([]RoundState, 0, len(buffered))
	for _, r := range buffered {
		if !a.hasPlaying || r.Index > a.playing {
			cands = append(cands, r)
		}
	}
	// Sender restart: nothing above the playing round, but data far below it.
	// The indices reset (the sender's app restarted mid-session); re-pin to the
	// new sequence instead of reading it as too-late forever. Everything left
	// from the old era — including the previously-playing round's buffer — is
	// reported as skipped so the caller drops it: an old-era index is numerically
	// ABOVE the re-pinned cursor and would otherwise win freshest-wins next
	// boundary, yanking playback back to the dead sequence (and its buffer
	// would block retirement forever).
	repin := false
	if len(cands) == 0 && a.hasPlaying {
		for _, r := range buffered {
			if r.Index <= a.playing-restartGap {
				cands = append(cands, r)
			}
		}
		if len(cands) == 0 {
			return a.playing, nil, false
		}
		repin = true
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Index < cands[j].Index })

	chosen := -1
	for i, r := range cands {
		if ready(r) {
			chosen = i // newest ready wins; later entries overwrite
		}
	}
	if chosen == -1 {
		return a.playing, nil, false
	}
	for _, r := range cands[:chosen] {
		skipped = append(skipped, r.Index)
	}
	if repin {
		for _, r := range buffered {
			if r.Index > cands[chosen].Index {
				skipped = append(skipped, r.Index)
			}
		}
	}
	a.skipped += uint64(len(skipped))
	a.playing, a.hasPlaying = cands[chosen].Index, true
	return a.playing, skipped, true
}
