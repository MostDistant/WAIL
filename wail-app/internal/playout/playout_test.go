package playout

import "testing"

func TestHoldUntilBoundaryD1(t *testing.T) {
	s := New(1)

	// Frames for interval 5 arrive before playout has begun → buffer them.
	if d := s.OnFrame(5); d != Buffer {
		t.Fatalf("pre-playout frame = %v, want buffer", d)
	}

	// Local boundary labeled 6 releases interval 6-1 = 5.
	if idx, adv := s.OnBoundary(6); !adv || idx != 5 {
		t.Fatalf("boundary 6 → (%d,%v), want (5,true)", idx, adv)
	}

	// Now interval 5 is playing; a frame for 6 is a future interval → buffer.
	if d := s.OnFrame(6); d != Buffer {
		t.Fatalf("frame for future interval = %v, want buffer", d)
	}
	// A late frame for the playing interval 5 → live-append.
	if d := s.OnFrame(5); d != LiveAppend {
		t.Fatalf("late frame for playing interval = %v, want live-append", d)
	}

	// Next boundary releases 6.
	if idx, adv := s.OnBoundary(7); !adv || idx != 6 {
		t.Fatalf("boundary 7 → (%d,%v), want (6,true)", idx, adv)
	}
	// Interval 5 has now finished → a straggler for 5 is too late.
	if d := s.OnFrame(5); d != TooLate {
		t.Fatalf("straggler for finished interval = %v, want too-late", d)
	}
}

func TestOffsetD2(t *testing.T) {
	s := New(2)
	// With D=2, the boundary labeled 10 releases interval 8.
	if idx, adv := s.OnBoundary(10); !adv || idx != 8 {
		t.Fatalf("boundary 10 with D=2 → (%d,%v), want (8,true)", idx, adv)
	}
}

func TestOffsetD0PlaysImmediately(t *testing.T) {
	s := New(0)
	// D=0: boundary N releases interval N (no delay).
	if idx, adv := s.OnBoundary(42); !adv || idx != 42 {
		t.Fatalf("boundary 42 with D=0 → (%d,%v), want (42,true)", idx, adv)
	}
}

func TestNegativeOffsetClamped(t *testing.T) {
	s := New(-5)
	if s.Offset() != 0 {
		t.Fatalf("Offset = %d, want 0 (negative clamped)", s.Offset())
	}
}

func TestDuplicateBoundaryDoesNotReRelease(t *testing.T) {
	s := New(1)
	if _, adv := s.OnBoundary(6); !adv {
		t.Fatal("first boundary should advance")
	}
	// Same boundary again (e.g. jitter): must not re-release interval 5.
	if idx, adv := s.OnBoundary(6); adv {
		t.Fatalf("duplicate boundary advanced (idx=%d); want no-op", idx)
	}
	// A stale, earlier boundary is also a no-op.
	if _, adv := s.OnBoundary(3); adv {
		t.Fatal("stale earlier boundary should not advance")
	}
	// Playout position is still interval 5.
	if p, ok := s.Playing(); !ok || p != 5 {
		t.Fatalf("playing = (%d,%v), want (5,true)", p, ok)
	}
}

func TestBoundaryGapReleasesLatest(t *testing.T) {
	s := New(1)
	s.OnBoundary(6) // playing 5
	// A stall: the next boundary the scheduler sees is 9 (labels 7,8 skipped).
	if idx, adv := s.OnBoundary(9); !adv || idx != 8 {
		t.Fatalf("boundary 9 after a gap → (%d,%v), want (8,true)", idx, adv)
	}
	// Frames for the skipped interval 7 are now too late.
	if d := s.OnFrame(7); d != TooLate {
		t.Fatalf("frame for skipped interval = %v, want too-late", d)
	}
}
