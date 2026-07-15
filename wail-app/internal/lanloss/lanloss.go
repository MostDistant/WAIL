// Package lanloss detects loss on the Link Audio LAN capture hop.
//
// Link Audio is fire-and-forget unicast UDP with no retransmission; each buffer
// carries a monotonically increasing per-stream `count`, so loss is *detectable*
// but never recoverable. WAIL surfaces it as a per-channel metric rather than
// hiding it (CONTEXT.md pillars 3 and 8). This is distinct from WAN relay loss
// (packet_loss.go, keyed on the WAIF frame sequence) and from interval-incomplete
// events (the emit side).
package lanloss

// Gap describes a detected discontinuity in a channel's buffer count sequence.
type Gap struct {
	ExpectedCount uint64 // the count we expected next
	GotCount      uint64 // the count that actually arrived
	LostBuffers   uint64 // number of buffers missing in between
}

// Tracker follows one capture channel's `count` sequence. Not safe for
// concurrent use; instantiate one per channel on the drain goroutine.
type Tracker struct {
	hasFirst     bool
	nextExpected uint64
	lostBuffers  uint64
	gapEvents    uint64
	reorders     uint64
}

// Observe records one buffer's count. It returns a non-nil *Gap when the count
// jumped ahead of the expected next value (one or more buffers were lost).
// Returns nil for the first buffer, an exactly-sequential buffer, or a
// reordered/duplicate arrival (count <= expected).
func (t *Tracker) Observe(count uint64) *Gap {
	if !t.hasFirst {
		t.hasFirst = true
		t.nextExpected = count + 1
		return nil
	}
	switch {
	case count == t.nextExpected:
		t.nextExpected++
		return nil
	case count < t.nextExpected:
		// Reorder or duplicate. The gap (if any) was already counted when we
		// advanced past this count; don't double-count.
		t.reorders++
		return nil
	default:
		lost := count - t.nextExpected
		gap := &Gap{ExpectedCount: t.nextExpected, GotCount: count, LostBuffers: lost}
		t.lostBuffers += lost
		t.gapEvents++
		t.nextExpected = count + 1
		return gap
	}
}

// LostBuffers is the cumulative number of missing buffers detected.
func (t *Tracker) LostBuffers() uint64 { return t.lostBuffers }

// GapEvents is the number of distinct gaps observed.
func (t *Tracker) GapEvents() uint64 { return t.gapEvents }

// Reorders is the number of out-of-order / duplicate buffers observed.
func (t *Tracker) Reorders() uint64 { return t.reorders }
