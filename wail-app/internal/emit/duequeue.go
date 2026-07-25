package emit

// DueQueue is a timed-release buffer of beat-stamped chunks for FIFO sinks
// (CLAP recv plugins over IPC, ADR-0005). The emit loop's feeder produces
// chunks cushion-ahead of the playhead — correct for Link Audio sinks, whose
// subscribers render each buffer at its stamped beat — but FIFO sinks play
// whatever has arrived, so early delivery is audible as constant extra
// latency. The queue holds each chunk until its stamped beat arrives (minus a
// small lead for transport jitter), making delivery time match stamped time.
//
// Beats come from the Link session timeline, which is monotonic across tempo
// changes, so release timing is tempo-independent. Depth is bounded by the
// feeder's cushion (chunks are only ever pushed up to playhead+cushion).
//
// Not safe for concurrent use — drive from the emit loop like Feeder.
type DueQueue struct {
	chunks []dueChunk
	head   int // index of the oldest pending chunk; avoids reslicing per flush
}

type dueChunk struct {
	beat    float64
	samples []int16
}

// Push enqueues a chunk stamped with the beat at which it begins.
func (q *DueQueue) Push(beat float64, samples []int16) {
	q.chunks = append(q.chunks, dueChunk{beat: beat, samples: samples})
}

// FlushDue emits every pending chunk whose stamped beat is due at nowBeat
// (beat <= nowBeat+leadBeats), in arrival order, each with its stamped beat
// (the sink converts it into the plugin-facing play-at timestamp). leadBeats
// lets delivery run a small constant ahead of the beat so socket jitter
// doesn't starve the sink.
func (q *DueQueue) FlushDue(nowBeat, leadBeats float64, emit func(samples []int16, beat float64)) {
	due := nowBeat + leadBeats
	for q.head < len(q.chunks) && q.chunks[q.head].beat <= due {
		emit(q.chunks[q.head].samples, q.chunks[q.head].beat)
		q.head++
	}
	// Reclaim the backing array once fully drained so a long-running stream
	// doesn't pin the cushion's worth of chunks forever.
	if q.head == len(q.chunks) {
		q.chunks = q.chunks[:0]
		q.head = 0
	}
}

// Len returns the number of pending (not yet due) chunks.
func (q *DueQueue) Len() int { return len(q.chunks) - q.head }
