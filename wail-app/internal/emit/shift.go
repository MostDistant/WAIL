package emit

// ShiftedPCM returns the interval's PCM shifted left by shiftFrames (the
// codec's OPUS_GET_LOOKAHEAD algorithmic delay): interval `index`'s content
// from shiftFrames on, followed by the head of interval `nextIndex` — or
// silence until that head arrives (play-partial covers late arrival; the
// stream's first shiftFrames, once, are the only audio ever lost).
//
// Rationale: the Opus codec delays every stream by a fixed lookahead, so a
// transient rendered on the sender's grid arrives lookahead-late. The shift is
// applied at read time (not at frame placement) because it is a per-stream
// constant and the reassembler's slot bookkeeping stays frame-aligned.
//
// dst must have room for the interval's full frame count × channels; the
// return value aliases dst. Returns nil when interval `index` has no buffer.
func (r *Reassembler) ShiftedPCM(dst []int16, index, nextIndex int64, shiftFrames int) []int16 {
	samples, _, _, ok := r.Interval(index)
	if !ok {
		return nil
	}
	if shiftFrames < 0 {
		shiftFrames = 0
	}
	ch := r.channels
	off := shiftFrames * ch
	if off > len(samples) {
		off = len(samples)
	}
	n := copy(dst, samples[off:])
	if nextIndex != index && n < len(dst) {
		if next, _, _, okNext := r.Interval(nextIndex); okNext {
			copy(dst[n:], next)
		}
	}
	return dst
}
