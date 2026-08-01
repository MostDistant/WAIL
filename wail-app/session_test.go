package main

import "testing"

// The relay scales its per-peer binary rate limit by the declared stream
// count, so the count the app declares must track the number of streams it
// can actually send on: enabled Link Audio capture channels plus any in-app
// senders (test tone, WAV, metronome broadcast).
func TestActiveSendStreamCount(t *testing.T) {
	cases := []struct {
		name                           string
		captureEnabled                 int
		testTone, wavSender, metronome bool
		want                           int
	}{
		{"idle peer still declares 1", 0, false, false, false, 1},
		{"one capture channel", 1, false, false, false, 1},
		{"three capture channels", 3, false, false, false, 3},
		{"capture plus metronome", 2, false, false, true, 3},
		{"all in-app senders", 0, true, true, true, 3},
		{"everything at once", 2, true, true, true, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := activeSendStreamCount(tc.captureEnabled, tc.testTone, tc.wavSender, tc.metronome)
			if got != tc.want {
				t.Fatalf("activeSendStreamCount(%d, %v, %v, %v) = %d, want %d",
					tc.captureEnabled, tc.testTone, tc.wavSender, tc.metronome, got, tc.want)
			}
		})
	}
}
