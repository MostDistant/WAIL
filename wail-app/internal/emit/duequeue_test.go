package emit

import (
	"slices"
	"testing"
)

// DueQueue exists for FIFO sinks (CLAP recv plugins over IPC, ADR-0005): unlike
// Link Audio subscribers they ignore beat stamps and play whatever has arrived,
// so a chunk must not be *delivered* until its stamped beat (minus a small lead
// for transport jitter). Feeding a FIFO sink the same cushion-ahead stream the
// Link Audio path gets makes everything play ~cushion late — the steady
// sub-interval offset measured in the field (~88ms at the default 100ms
// cushion).

func flushCollect(q *DueQueue, nowBeat, leadBeats float64) [][]int16 {
	got, _ := flushCollectBeats(q, nowBeat, leadBeats)
	return got
}

func flushCollectBeats(q *DueQueue, nowBeat, leadBeats float64) ([][]int16, []float64) {
	var got [][]int16
	var beats []float64
	q.FlushDue(nowBeat, leadBeats, func(s []int16, beat float64) {
		got = append(got, s)
		beats = append(beats, beat)
	})
	return got, beats
}

func TestDueQueueHoldsChunkUntilStampedBeat(t *testing.T) {
	q := &DueQueue{}
	q.Push(100.0, []int16{1, 2, 3})

	if got := flushCollect(q, 99.9, 0); len(got) != 0 {
		t.Fatalf("chunk released before its stamped beat: %v", got)
	}
	if q.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (chunk held)", q.Len())
	}

	got := flushCollect(q, 100.0, 0)
	if len(got) != 1 || !slices.Equal(got[0], []int16{1, 2, 3}) {
		t.Fatalf("at the stamped beat, got %v, want [[1 2 3]]", got)
	}
	if q.Len() != 0 {
		t.Fatalf("Len = %d after release, want 0", q.Len())
	}
}

func TestDueQueueReleasesInOrderHoldingFutureChunks(t *testing.T) {
	q := &DueQueue{}
	q.Push(100.0, []int16{1})
	q.Push(100.1, []int16{2})
	q.Push(100.2, []int16{3})

	got := flushCollect(q, 100.15, 0)
	if len(got) != 2 || !slices.Equal(got[0], []int16{1}) || !slices.Equal(got[1], []int16{2}) {
		t.Fatalf("got %v, want [[1] [2]] in order", got)
	}
	if q.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (future chunk held)", q.Len())
	}

	got = flushCollect(q, 100.2, 0)
	if len(got) != 1 || !slices.Equal(got[0], []int16{3}) {
		t.Fatalf("second flush got %v, want [[3]]", got)
	}
}

func TestDueQueueLeadWindowIsBounded(t *testing.T) {
	q := &DueQueue{}
	q.Push(100.0, []int16{1})

	// Just outside the lead: still held.
	if got := flushCollect(q, 99.74, 0.25); len(got) != 0 {
		t.Fatalf("chunk released outside the lead window: %v", got)
	}
	// Within the lead: released early by at most leadBeats.
	if got := flushCollect(q, 99.75, 0.25); len(got) != 1 {
		t.Fatalf("chunk not released inside the lead window")
	}
}

func TestDueQueueReleaseCarriesStampedBeat(t *testing.T) {
	// The sink converts each released chunk's beat into the plugin-facing
	// play-at timestamp, so the beat must survive the queue intact.
	q := &DueQueue{}
	q.Push(100.25, []int16{1})
	q.Push(100.5, []int16{2})
	_, beats := flushCollectBeats(q, 100.5, 0)
	if !slices.Equal(beats, []float64{100.25, 100.5}) {
		t.Fatalf("beats = %v, want [100.25 100.5]", beats)
	}
}

func TestDueQueueReleasesPastDueChunkImmediately(t *testing.T) {
	// Catch-up after a stall: a chunk pushed already-due must not wait for
	// another flush boundary.
	q := &DueQueue{}
	q.Push(50.0, []int16{1})
	if got := flushCollect(q, 100.0, 0); len(got) != 1 {
		t.Fatalf("past-due chunk not released on first flush")
	}
}
