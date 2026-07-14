package abllink

import (
	"math"
	"testing"
)

// TestSessionStateMath exercises the sync FFI round-trips offline (no Enable, so
// no network). It proves the cgo binding links against vendor/link and that
// doubles/int64s cross the boundary intact.
func TestSessionStateMath(t *testing.T) {
	l := New(120.0)
	defer l.Close()

	if l.IsEnabled() {
		t.Fatal("new Link should start disabled")
	}
	if got := l.NumPeers(); got != 0 {
		t.Fatalf("NumPeers on a disabled link = %d, want 0", got)
	}

	ss := NewSessionState()
	defer ss.Close()

	l.CaptureAppSessionState(ss)
	if got := ss.Tempo(); math.Abs(got-120.0) > 1e-6 {
		t.Fatalf("initial tempo = %v, want 120", got)
	}

	// Mutate tempo and commit; re-capture should reflect it.
	now := l.ClockMicros()
	ss.SetTempo(140.0, now)
	l.CommitAppSessionState(ss)

	l.CaptureAppSessionState(ss)
	if got := ss.Tempo(); math.Abs(got-140.0) > 1e-3 {
		t.Fatalf("tempo after SetTempo = %v, want ~140", got)
	}

	// Beats must advance monotonically with time at ~140 BPM.
	const quantum = 4.0
	t0 := l.ClockMicros()
	beat0 := ss.BeatAtTime(t0, quantum)
	beat1 := ss.BeatAtTime(t0+1_000_000, quantum) // +1 second
	if beat1 <= beat0 {
		t.Fatalf("beats did not advance: beat0=%v beat1=%v", beat0, beat1)
	}
	// 140 BPM ≈ 2.333 beats/sec; allow generous tolerance.
	if d := beat1 - beat0; math.Abs(d-140.0/60.0) > 0.2 {
		t.Fatalf("one second advanced %v beats, want ~2.33", d)
	}

	// Phase must stay within [0, quantum).
	if p := ss.PhaseAtTime(t0, quantum); p < 0 || p >= quantum {
		t.Fatalf("phase %v out of [0,%v)", p, quantum)
	}

	// TimeAtBeat should invert BeatAtTime approximately.
	tb := ss.TimeAtBeat(beat1, quantum)
	if back := ss.BeatAtTime(tb, quantum); math.Abs(back-beat1) > 0.01 {
		t.Fatalf("TimeAtBeat/BeatAtTime round-trip off: %v vs %v", back, beat1)
	}
}

// TestLinkAudioControlSurface exercises the non-realtime Link Audio surface that
// does not require the network. Calls must be safe (never panic / never leak the
// C string list) on a fresh, disabled instance.
//
// Note on peer name: Controller::setName posts an async lambda onto the Link IO
// context, so the name only becomes readable once Link is enabled and its IO
// thread has run. On a disabled instance PeerName() correctly returns "". We
// therefore assert no-panic + disabled-instance semantics here and leave the
// name round-trip to enabled-link integration testing.
func TestLinkAudioControlSurface(t *testing.T) {
	l := New(120.0)
	defer l.Close()

	l.SetPeerName("WAIL test") // must not panic; async-applied when enabled
	_ = l.PeerName()           // must not panic; empty while disabled

	if l.IsLinkAudioEnabled() {
		t.Fatal("Link Audio should start disabled")
	}

	// With no peers/network, the channel list must be empty (never panic).
	if chans := l.Channels(); len(chans) != 0 {
		t.Fatalf("expected no channels on a fresh disabled link, got %d", len(chans))
	}
}
