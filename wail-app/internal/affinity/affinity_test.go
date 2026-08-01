package affinity

import (
	"strings"
	"testing"
)

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

func TestFormatRoomChannelName(t *testing.T) {
	// The prefix is the room-published marker (ADR-0007): bridge receivers
	// filter on it; raw LAN channels (bridge Send) never carry it.
	cases := []struct{ peer, stream, want string }{
		{"Alice", "guitar", "WAIL · Alice · guitar"},
		{"", "guitar", "WAIL · guitar"},
		{"Alice", "", "WAIL · Alice"},
	}
	for _, tc := range cases {
		if got := FormatRoomChannelName(tc.peer, tc.stream); got != tc.want {
			t.Errorf("FormatRoomChannelName(%q,%q) = %q, want %q", tc.peer, tc.stream, got, tc.want)
		}
	}
	if !strings.HasPrefix(FormatRoomChannelName("A", "b"), RoomChannelPrefix) {
		t.Error("room channel name must carry RoomChannelPrefix")
	}
}

// A user's own track or stream name may already carry the room prefix; the
// published name must still have exactly one, or the receive side (which
// strips one) shows a name that reads as a nested room channel.
func TestFormatRoomChannelNameStripsPrefixFromUserNames(t *testing.T) {
	cases := []struct{ peer, stream, want string }{
		{"alice", "Guitar", "WAIL · alice · Guitar"},
		{"alice", "WAIL · Guitar", "WAIL · alice · Guitar"},
		{"WAIL · alice", "Guitar", "WAIL · alice · Guitar"},
		{"alice", "WAIL · WAIL · Guitar", "WAIL · alice · Guitar"},
		{"alice", "WAIL · ", "WAIL · alice"},
	}
	for _, c := range cases {
		if got := FormatRoomChannelName(c.peer, c.stream); got != c.want {
			t.Errorf("FormatRoomChannelName(%q, %q) = %q, want %q", c.peer, c.stream, got, c.want)
		}
	}
}
