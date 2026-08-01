package main

import (
	"testing"
	"time"
)

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

// A rejected update_streams declaration must not be a dead end: the session
// re-declares once the retry backoff elapses, and any drift in the desired
// count always declares immediately.
func TestShouldDeclareStreams(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	future := now.Add(30 * time.Second)
	cases := []struct {
		name                  string
		desired, lastDeclared int
		rejected              bool
		retryAt               time.Time
		want                  bool
	}{
		{"declared matches, no rejection", 3, 3, false, time.Time{}, false},
		{"drift declares immediately", 4, 3, false, time.Time{}, true},
		{"shrink declares immediately", 2, 3, false, time.Time{}, true},
		{"rejected but backoff pending", 3, 3, true, future, false},
		{"rejected and backoff elapsed", 3, 3, true, past, true},
		{"drift wins over pending backoff", 4, 3, true, future, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldDeclareStreams(tc.desired, tc.lastDeclared, tc.rejected, tc.retryAt, now)
			if got != tc.want {
				t.Fatalf("shouldDeclareStreams(%d, %d, %v, retryAt, now) = %v, want %v",
					tc.desired, tc.lastDeclared, tc.rejected, got, tc.want)
			}
		})
	}
}
