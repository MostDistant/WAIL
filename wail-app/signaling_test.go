package main

import "testing"

// A sync message dropped on a full queue is silent and permanent: the sender
// records it as delivered, so nothing resends it until the payload itself
// changes. Callers need to know a send was refused.
func TestBroadcastSyncReportsDropWhenQueueFull(t *testing.T) {
	sc := &SignalingClient{syncOutCh: make(chan outgoingSync, 1)}

	if !sc.BroadcastSync(NewStreamNames(nil)) {
		t.Fatal("first send should enqueue")
	}
	if sc.BroadcastSync(NewStreamNames(nil)) {
		t.Fatal("full queue must report a drop")
	}
	if got := sc.syncDrops.Load(); got != 1 {
		t.Fatalf("syncDrops = %d, want 1", got)
	}
}

func TestSendSyncToReportsDropWhenQueueFull(t *testing.T) {
	sc := &SignalingClient{syncOutCh: make(chan outgoingSync, 1)}

	if !sc.SendSyncTo("peer1", NewStreamNames(nil)) {
		t.Fatal("first send should enqueue")
	}
	if sc.SendSyncTo("peer1", NewStreamNames(nil)) {
		t.Fatal("full queue must report a drop")
	}
}
