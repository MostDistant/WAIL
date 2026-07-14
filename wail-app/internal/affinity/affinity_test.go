package affinity

import "testing"

// fakeSink stands in for a LinkAudioSink; a monotonically increasing id lets the
// tests assert whether a new channel was minted or an existing one reused.
type fakeSink struct{ id int }

func TestReconnectReusesChannel(t *testing.T) {
	r := New[*fakeSink]()
	next := 0
	create := func() *fakeSink { next++; return &fakeSink{id: next} }

	key := Key{Identity: "uuid-A", Stream: 0}

	e1, created1 := r.Resolve(key, "Alice", "guitar", create)
	if !created1 {
		t.Fatal("first Resolve should create")
	}
	if e1.Name != "Alice · guitar" {
		t.Fatalf("name = %q", e1.Name)
	}

	// Simulate reconnect: same identity+stream, possibly a renamed peer.
	e2, created2 := r.Resolve(key, "Alice (reconnected)", "guitar", create)
	if created2 {
		t.Fatal("reconnect must reuse the existing channel, not create")
	}
	if e2.Handle != e1.Handle {
		t.Fatal("reconnect returned a different handle — channel affinity broken")
	}
	if e2.Handle.id != 1 {
		t.Fatalf("handle id = %d, want 1 (no new sink minted)", e2.Handle.id)
	}
	if e2.Name != "Alice (reconnected) · guitar" {
		t.Fatalf("name not refreshed on reconnect: %q", e2.Name)
	}
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1", r.Len())
	}
}

func TestDistinctStreamsAndIdentities(t *testing.T) {
	r := New[*fakeSink]()
	next := 0
	create := func() *fakeSink { next++; return &fakeSink{id: next} }

	r.Resolve(Key{"uuid-A", 0}, "Alice", "gtr", create)
	r.Resolve(Key{"uuid-A", 1}, "Alice", "vox", create)
	r.Resolve(Key{"uuid-B", 0}, "Bob", "bass", create)

	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3 distinct channels", r.Len())
	}
	if next != 3 {
		t.Fatalf("minted %d sinks, want 3", next)
	}
}

func TestRemoveReturnsHandle(t *testing.T) {
	r := New[*fakeSink]()
	create := func() *fakeSink { return &fakeSink{id: 42} }
	key := Key{"uuid-A", 0}
	r.Resolve(key, "Alice", "gtr", create)

	h, ok := r.Remove(key)
	if !ok || h.id != 42 {
		t.Fatalf("Remove = (%v, %v), want (handle#42, true)", h, ok)
	}
	if r.Len() != 0 {
		t.Fatalf("Len after remove = %d, want 0", r.Len())
	}
	if _, ok := r.Remove(key); ok {
		t.Fatal("second Remove should report not-found")
	}
}

func TestFormatName(t *testing.T) {
	cases := []struct{ peer, stream, want string }{
		{"Alice", "guitar", "Alice · guitar"},
		{"", "guitar", "guitar"},
		{"Alice", "", "Alice"},
		{"", "", "WAIL"},
		{"  Alice  ", " guitar ", "Alice · guitar"},
	}
	for _, tc := range cases {
		if got := FormatName(tc.peer, tc.stream); got != tc.want {
			t.Errorf("FormatName(%q,%q) = %q, want %q", tc.peer, tc.stream, got, tc.want)
		}
	}
}
