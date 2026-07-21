package emit

// FramePos identifies one 20ms frame slot: WAIF frame number Frame within room
// interval Interval.
type FramePos struct {
	Interval int64
	Frame    int
}

// MissingSlots enumerates the frame slots lost between prev and next in stream
// order: `gap` consecutive sequence numbers were sent after prev and never
// arrived before next. The walk starts at the slot after prev and rolls into
// following intervals via totalFor (the interval's WAIF frame count — the
// reassembler's known total, or derived from tempo/config). At most max slots
// (the head of the gap) are returned — Opus PLC fades to silence past ~100ms,
// so concealing a deep gap's tail is pointless.
//
// Returns nil when the walk is inconsistent (walking gap+1 slots from prev
// does not land on next — interval lengths changed mid-gap) or when totalFor
// cannot size an interval: the caller skips concealment rather than risk
// placing synthesized audio at the wrong position.
func MissingSlots(prev, next FramePos, gap int, totalFor func(int64) int, max int) []FramePos {
	if gap <= 0 {
		return nil
	}
	advance := func(p FramePos) (FramePos, bool) {
		total := totalFor(p.Interval)
		if total <= 0 {
			return p, false
		}
		p.Frame++
		if p.Frame >= total {
			p.Interval++
			p.Frame = 0
		}
		return p, true
	}

	var out []FramePos
	pos := prev
	for i := 0; i < gap; i++ {
		var ok bool
		pos, ok = advance(pos)
		if !ok {
			return nil
		}
		if len(out) < max {
			out = append(out, pos)
		}
	}
	if pos, ok := advance(pos); !ok || pos != next {
		return nil
	}
	return out
}
