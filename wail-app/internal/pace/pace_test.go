package pace

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

// collector gathers sent frames thread-safely.
type collector struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *collector) send(f []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, f)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

func (c *collector) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.frames))
	copy(out, c.frames)
	return out
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func batch(n int, tag byte) [][]byte {
	frames := make([][]byte, n)
	for i := range frames {
		frames[i] = []byte(fmt.Sprintf("%c%04d", tag, i))
	}
	return frames
}

func TestDeliversAllFramesInOrder(t *testing.T) {
	c := &collector{}
	s := New(0, 4, c.send, nil)
	defer s.Close()

	in := batch(100, 'a')
	s.Enqueue(in)

	waitFor(t, 2*time.Second, func() bool { return c.count() == len(in) })
	for i, f := range c.snapshot() {
		if !bytes.Equal(f, in[i]) {
			t.Fatalf("frame %d out of order: got %q want %q", i, f, in[i])
		}
	}
}

func TestBatchesDeliverBackToBackInOrder(t *testing.T) {
	c := &collector{}
	s := New(0, 4, c.send, nil)
	defer s.Close()

	b1 := batch(50, 'a')
	b2 := batch(50, 'b')
	s.Enqueue(b1)
	s.Enqueue(b2)

	waitFor(t, 2*time.Second, func() bool { return c.count() == 100 })
	got := c.snapshot()
	want := append(append([][]byte{}, b1...), b2...)
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestSpacesFramesByGap(t *testing.T) {
	c := &collector{}
	const gap = 5 * time.Millisecond
	const n = 6
	s := New(gap, 4, c.send, nil)
	defer s.Close()

	start := time.Now()
	s.Enqueue(batch(n, 'a'))
	waitFor(t, 2*time.Second, func() bool { return c.count() == n })

	// n frames have n-1 gaps between them; only assert the lower bound —
	// scheduling jitter makes an upper bound flaky.
	if elapsed := time.Since(start); elapsed < (n-1)*gap {
		t.Fatalf("frames sent too fast: %v elapsed, want >= %v", elapsed, (n-1)*gap)
	}
}

func TestBacklogDropsWholeBatchAndReports(t *testing.T) {
	// Gate the send so the runner is deterministically blocked mid-batch.
	gate := make(chan struct{})
	sending := make(chan struct{}, 16)
	send := func([]byte) {
		sending <- struct{}{}
		<-gate
	}

	var mu sync.Mutex
	var dropped []int
	onDrop := func(n int) {
		mu.Lock()
		dropped = append(dropped, n)
		mu.Unlock()
	}

	s := New(0, 1, send, onDrop)
	defer s.Close()

	s.Enqueue(batch(1, 'a')) // picked up by the runner, blocks in send
	<-sending                // runner is now inside send; queue is empty
	s.Enqueue(batch(2, 'b')) // fills the 1-slot queue
	s.Enqueue(batch(3, 'c')) // queue full → dropped

	mu.Lock()
	got := append([]int{}, dropped...)
	mu.Unlock()
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("expected one dropped batch of 3 frames, got %v", got)
	}
	close(gate)
}

func TestCloseStopsPromptlyMidBatch(t *testing.T) {
	c := &collector{}
	s := New(time.Hour, 4, c.send, nil) // huge gap: only frame 0 goes out
	s.Enqueue(batch(10, 'a'))
	waitFor(t, 2*time.Second, func() bool { return c.count() == 1 })

	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return promptly while paced mid-batch")
	}
	if got := c.count(); got != 1 {
		t.Fatalf("expected no sends after Close, got %d frames", got)
	}
}

func TestEnqueueAfterCloseIsNoOp(t *testing.T) {
	c := &collector{}
	var drops int
	var mu sync.Mutex
	s := New(0, 4, c.send, func(int) { mu.Lock(); drops++; mu.Unlock() })
	s.Close()
	s.Enqueue(batch(5, 'a')) // must not panic, send, or count as a drop
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	d := drops
	mu.Unlock()
	if c.count() != 0 || d != 0 {
		t.Fatalf("enqueue after close: sent=%d drops=%d, want 0/0", c.count(), d)
	}
}
