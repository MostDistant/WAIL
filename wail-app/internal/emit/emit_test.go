package emit

import "testing"

func frame(n, channels int, v int16) []int16 {
	s := make([]int16, n*channels)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestReassemblerInOrder(t *testing.T) {
	r := New(1, 960) // mono, 960 frames per WAIF frame
	r.Add(5, 0, frame(960, 1, 1), false, 0)
	r.Add(5, 1, frame(960, 1, 2), false, 0)
	r.Add(5, 2, frame(960, 1, 3), true, 3) // final, 3 frames total

	if !r.Complete(5) {
		t.Fatal("interval 5 should be complete after all 3 frames")
	}
	pcm, received, total, ok := r.Take(5)
	if !ok || received != 3 || total != 3 {
		t.Fatalf("Take = received=%d total=%d ok=%v", received, total, ok)
	}
	if len(pcm) != 3*960 {
		t.Fatalf("pcm len = %d, want %d", len(pcm), 3*960)
	}
	if pcm[0] != 1 || pcm[960] != 2 || pcm[1920] != 3 {
		t.Fatal("frames not placed at expected offsets")
	}
	if r.Has(5) {
		t.Fatal("Take should have removed interval 5")
	}
}

func TestReassemblerOutOfOrderAndPartial(t *testing.T) {
	r := New(1, 960)
	// Final frame arrives first (out of order), announcing 3 total.
	r.Add(2, 2, frame(960, 1, 9), true, 3)
	r.Add(2, 0, frame(960, 1, 7), false, 0)
	// Frame 1 never arrives → incomplete.
	if r.Complete(2) {
		t.Fatal("interval 2 missing frame 1 should be incomplete")
	}
	pcm, received, total, _ := r.Interval(2)
	if received != 2 || total != 3 {
		t.Fatalf("received=%d total=%d, want 2/3", received, total)
	}
	// Sized to total (3 frames) even though frame 1 is missing (silence gap).
	if len(pcm) != 3*960 {
		t.Fatalf("pcm len = %d, want %d (padded to total)", len(pcm), 3*960)
	}
	if pcm[0] != 7 || pcm[1920] != 9 || pcm[960] != 0 {
		t.Fatal("expected frame0=7, frame2=9, missing frame1=silence")
	}
}

func TestDropDiscardsPastIntervals(t *testing.T) {
	r := New(1, 960)
	r.Add(1, 0, frame(960, 1, 1), true, 1)
	r.Add(2, 0, frame(960, 1, 1), true, 1)
	r.Add(3, 0, frame(960, 1, 1), true, 1)
	r.Drop(2) // drop intervals <= 2
	if r.Has(1) || r.Has(2) {
		t.Fatal("intervals 1 and 2 should be dropped")
	}
	if !r.Has(3) {
		t.Fatal("interval 3 should survive")
	}
}

func TestPacedReaderChunksAndBeats(t *testing.T) {
	// 48000 frames (1s) at 120 BPM starting at beat 100; 120 BPM = 2 beats/sec.
	buf := frame(48000, 1, 5)
	pr := NewPacedReader(func() []int16 { return buf }, 1, 48000, 120, 100, 48000)

	// First chunk of 24000 frames (0.5s) begins at beat 100.
	s, beat, done := pr.Next(24000)
	if done || len(s) != 24000 || beat != 100 {
		t.Fatalf("chunk1: len=%d beat=%v done=%v", len(s), beat, done)
	}
	// Second chunk begins at beat 101 (0.5s = 1 beat later).
	s, beat, done = pr.Next(24000)
	if len(s) != 24000 || beat != 101 {
		t.Fatalf("chunk2: len=%d beat=%v", len(s), beat)
	}
	if !done {
		t.Fatal("interval should be done after reading all frames")
	}
	if pr.Remaining() != 0 {
		t.Fatalf("Remaining = %d, want 0", pr.Remaining())
	}
}

func TestPacedReaderPlayPartialSilence(t *testing.T) {
	// Interval claims 48000 frames but only 100 have arrived → the rest reads as
	// silence (play-partial), never out of bounds.
	buf := frame(100, 1, 8)
	pr := NewPacedReader(func() []int16 { return buf }, 1, 48000, 120, 0, 48000)
	total := 0
	for {
		s, _, done := pr.Next(10000)
		total += len(s)
		if done {
			break
		}
	}
	if total != 48000 {
		t.Fatalf("read %d frames total, want 48000", total)
	}
}

func TestPacedReaderLiveAppend(t *testing.T) {
	// The buffer grows after the reader starts (live-append); later chunks must
	// see the newly-arrived samples.
	buf := frame(24000, 1, 1) // first half present
	pr := NewPacedReader(func() []int16 { return buf }, 1, 48000, 120, 0, 48000)

	s, _, _ := pr.Next(24000) // reads first half
	if s[0] != 1 {
		t.Fatal("first half should be present")
	}
	// Second half arrives late.
	buf = append(buf, frame(24000, 1, 2)...)
	s, _, done := pr.Next(24000)
	if !done || s[0] != 2 {
		t.Fatalf("live-appended second half not visible: s[0]=%d done=%v", s[0], done)
	}
}

// --- Slot provenance (PLC support) ---

func TestAddPLCFillsOnlyEmptySlots(t *testing.T) {
	r := New(1, 960)
	r.Add(0, 0, frame(960, 1, 1), false, 0)
	r.AddPLC(0, 1, frame(960, 1, 8)) // conceal missing slot 1
	r.AddPLC(0, 0, frame(960, 1, 9)) // must NOT overwrite the real slot 0

	pcm, received, _, _ := r.Interval(0)
	if pcm[0] != 1 {
		t.Fatalf("PLC overwrote a real slot: pcm[0]=%d, want 1", pcm[0])
	}
	if pcm[960] != 8 {
		t.Fatalf("PLC slot content = %d, want 8", pcm[960])
	}
	if received != 1 {
		t.Fatalf("received = %d, want 1 — PLC must not count as received", received)
	}
	missing, concealed := r.Missing(0)
	if missing != 0 || concealed != 1 {
		t.Fatalf("Missing = (%d,%d), want (0,1)", missing, concealed)
	}
}

func TestRealFrameReplacesPLC(t *testing.T) {
	r := New(1, 960)
	r.AddPLC(3, 0, frame(960, 1, 8))
	r.Add(3, 0, frame(960, 1, 5), false, 0) // late real frame wins

	pcm, received, _, _ := r.Interval(3)
	if pcm[0] != 5 {
		t.Fatalf("real frame should replace PLC: pcm[0]=%d, want 5", pcm[0])
	}
	if received != 1 {
		t.Fatalf("received = %d, want 1", received)
	}
	if _, concealed := r.Missing(3); concealed != 0 {
		t.Fatalf("concealed = %d, want 0 after real replacement", concealed)
	}
}

func TestDuplicateAddDoesNotDoubleCountReceived(t *testing.T) {
	r := New(1, 960)
	r.Add(7, 0, frame(960, 1, 1), false, 0)
	r.Add(7, 0, frame(960, 1, 1), false, 0) // duplicate delivery
	r.Add(7, 2, frame(960, 1, 3), true, 3)  // final: 3 total, frame 1 missing

	if r.Complete(7) {
		t.Fatal("interval with a missing frame must not be Complete after duplicates")
	}
	_, received, _, _ := r.Interval(7)
	if received != 2 {
		t.Fatalf("received = %d, want 2 distinct frames", received)
	}
	missing, _ := r.Missing(7)
	if missing != 1 {
		t.Fatalf("missing = %d, want 1", missing)
	}
}

func TestMissingCountsAgainstTotalAndMaxFrame(t *testing.T) {
	r := New(1, 960)
	if m, c := r.Missing(9); m != 0 || c != 0 {
		t.Fatalf("unknown interval Missing = (%d,%d), want (0,0)", m, c)
	}
	// Total unknown: count against maxFrame.
	r.Add(9, 2, frame(960, 1, 3), false, 0) // slots 0,1 empty, maxFrame 3
	if m, _ := r.Missing(9); m != 2 {
		t.Fatalf("missing = %d, want 2 (against maxFrame)", m)
	}
	// Final announces 5 total: slots 0,1,3,4 empty.
	r.Add(9, 4, frame(960, 1, 5), true, 5)
	r.AddPLC(9, 0, frame(960, 1, 8))
	m, c := r.Missing(9)
	if m != 2 || c != 1 {
		t.Fatalf("Missing = (%d,%d), want (2,1)", m, c)
	}
}

func TestMinIndexTracksLowestBufferedInterval(t *testing.T) {
	r := New(2, 960)
	if _, ok := r.MinIndex(); ok {
		t.Fatal("empty reassembler must report no buffered interval")
	}
	pcm := make([]int16, 960*2)
	r.Add(70, 0, pcm, false, 0)
	r.Add(12, 0, pcm, false, 0)
	r.Add(4000, 0, pcm, false, 0)

	min, ok := r.MinIndex()
	if !ok || min != 12 {
		t.Fatalf("MinIndex = (%d,%v), want (12,true)", min, ok)
	}
	// The distinction retirement depends on: one far-ahead straggler must not
	// read as "audio is still playing" the way MaxIndex would.
	r.Drop(70)
	min, ok = r.MinIndex()
	if !ok || min != 4000 {
		t.Fatalf("after Drop(70) MinIndex = (%d,%v), want (4000,true)", min, ok)
	}
	if max, _ := r.MaxIndex(); max != min {
		t.Fatalf("single straggler should have MinIndex == MaxIndex, got %d vs %d", min, max)
	}
}
