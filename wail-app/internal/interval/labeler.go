package interval

// RoomLabeler maps a client's *local* interval index (derived from its own Link
// beat) to the shared *room* interval index owned by the relay (ADR-0003).
//
// When an interval_anchor arrives, the client samples its local index at that
// instant and aligns it to the anchor's room index, fixing a constant offset.
// Because the local Link session and the room clock advance at the same musical
// tempo, the offset holds until a tempo change delivers a fresh anchor. The
// interval's worth of slack absorbs RTT-scale skew, so sub-interval timing error
// in the alignment is immaterial.
type RoomLabeler struct {
	offset  int64
	aligned bool
}

// Align fixes the mapping so that localIndex corresponds to roomIndex.
func (l *RoomLabeler) Align(roomIndex, localIndex int64) {
	l.offset = roomIndex - localIndex
	l.aligned = true
}

// Aligned reports whether an anchor has been received yet.
func (l *RoomLabeler) Aligned() bool { return l.aligned }

// RoomIndex returns the room interval index for a local index. ok is false until
// the first anchor aligns the labeler.
func (l *RoomLabeler) RoomIndex(localIndex int64) (int64, bool) {
	if !l.aligned {
		return 0, false
	}
	return localIndex + l.offset, true
}

// Offset returns the current room-minus-local offset (valid once Aligned).
func (l *RoomLabeler) Offset() int64 { return l.offset }
