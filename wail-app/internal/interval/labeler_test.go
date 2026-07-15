package interval

import "testing"

func TestRoomLabeler(t *testing.T) {
	var l RoomLabeler
	if _, ok := l.RoomIndex(0); ok {
		t.Fatal("labeler should not resolve before alignment")
	}

	// Anchor says the room is at index 1000 when our local index is 7.
	l.Align(1000, 7)
	if !l.Aligned() {
		t.Fatal("should be aligned")
	}
	if l.Offset() != 993 {
		t.Fatalf("offset = %d, want 993", l.Offset())
	}
	// Subsequent local boundaries carry the room label.
	for _, tc := range []struct{ local, room int64 }{{7, 1000}, {8, 1001}, {20, 1013}} {
		if got, ok := l.RoomIndex(tc.local); !ok || got != tc.room {
			t.Errorf("RoomIndex(%d) = (%d,%v), want %d", tc.local, got, ok, tc.room)
		}
	}

	// A fresh anchor after a tempo change re-aligns.
	l.Align(2000, 50)
	if got, _ := l.RoomIndex(51); got != 2001 {
		t.Fatalf("after re-align RoomIndex(51) = %d, want 2001", got)
	}
}
