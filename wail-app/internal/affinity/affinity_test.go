package affinity

import "testing"

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
