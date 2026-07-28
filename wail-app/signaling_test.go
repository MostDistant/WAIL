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

// SendTo used to return a nil error for a send it knew had been dropped. The
// Hello reply is sent behind MarkHelloSent, which latches on the first call —
// so a drop there means the peer never learns our identity and evicts us at
// 15s, while our own GUI still shows them. Callers need the truth.
func TestPeerMeshSendToReportsDropWhenQueueFull(t *testing.T) {
	sc := &SignalingClient{syncOutCh: make(chan outgoingSync, 1)}
	mesh := &PeerMesh{signaling: sc}

	if !mesh.SendTo("peer1", NewStreamNames(nil)) {
		t.Fatal("first send should enqueue")
	}
	if mesh.SendTo("peer1", NewStreamNames(nil)) {
		t.Fatal("full queue must report a drop")
	}
}
