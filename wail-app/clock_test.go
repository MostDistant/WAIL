package main

import (
	"testing"
)

func TestRTTUsReturnsNilForUnknown(t *testing.T) {
	c := NewClockSync()
	if c.RTTUs("nonexistent") != nil {
		t.Fatal("should return nil for unknown peer")
	}
}

func TestRTTUsUpdatesOnPong(t *testing.T) {
	c := NewClockSync()
	sent := c.NowUs() - 5000
	c.HandlePong("peer-a", sent, 0)
	rtt := c.RTTUs("peer-a")
	if rtt == nil {
		t.Fatal("expected RTT value")
	}
	if *rtt < 4000 || *rtt > 7000 {
		t.Fatalf("expected ~5000, got %d", *rtt)
	}
}

func TestJitterNoneWithInsufficientSamples(t *testing.T) {
	c := NewClockSync()
	c.HandlePong("peer-a", c.NowUs()-10000, 0)
	if c.JitterUs("peer-a") != nil {
		t.Fatal("should return nil with < 2 samples")
	}
}

func TestJitterNoneForUnknownPeer(t *testing.T) {
	c := NewClockSync()
	if c.JitterUs("unknown") != nil {
		t.Fatal("should return nil")
	}
}

func TestJitterPositiveWithVariedSamples(t *testing.T) {
	c := NewClockSync()
	// Directly inject known RTT samples
	c.mu.Lock()
	clock := &PeerClock{samples: []int64{10000, 20000, 30000, 40000}}
	clock.RTTUs = medianOf(clock.samples)
	c.perPeer["peer-a"] = clock
	c.mu.Unlock()

	// median of [10000, 20000, 30000, 40000] = 30000 (index 2)
	// MAD = (|10000-30000| + |20000-30000| + |30000-30000| + |40000-30000|) / 4
	//     = (20000 + 10000 + 0 + 10000) / 4 = 10000
	jitter := c.JitterUs("peer-a")
	if jitter == nil {
		t.Fatal("expected jitter value")
	}
	if *jitter != 10000 {
		t.Fatalf("expected 10000, got %d", *jitter)
	}
}

func TestMakePingIncrements(t *testing.T) {
	c := NewClockSync()
	p1 := c.MakePing()
	p2 := c.MakePing()
	if p1.ID >= p2.ID {
		t.Fatal("ping IDs should increment")
	}
}

func TestServerPongRelayRTT(t *testing.T) {
	cs := NewClockSync()
	if _, ok := cs.RelayRTTUs(); ok {
		t.Fatal("relay RTT must be unknown before any server pong")
	}
	sent := cs.NowUs()
	rtt := cs.HandleServerPong(sent)
	if rtt < 0 {
		t.Fatal("negative RTT")
	}
	if _, ok := cs.RelayRTTUs(); !ok {
		t.Fatal("relay RTT must be known after a server pong")
	}
	// Clock anomaly: a pong for a ping "from the future" is ignored.
	before := cs.relayRTTUs
	cs.HandleServerPong(cs.NowUs() + 1_000_000)
	if cs.relayRTTUs != before {
		t.Fatal("anomalous pong must not update the estimate")
	}
}
