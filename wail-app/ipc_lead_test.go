//go:build !linkstub

package main

import "testing"

// The IPC delivery lead is the recv-plugin path's playback offset (FIFO sink:
// delivery time IS play time), so it wants to be as small as possible — but
// the plugin's process() pulls a whole DAW block per call and pads silence
// when short, so the floor is ~DAW-block + app-chunk + jitter (≈10ms at
// 128-sample buffers, ≈18ms at 512). WAIL_IPC_LEAD_MS lets a setup tune that
// trade-off; the clamp keeps experiments out of the guaranteed-crackle zone.

func TestResolveIPCLeadMs(t *testing.T) {
	cases := []struct {
		name string
		env  string
		set  bool
		want int
	}{
		{"unset uses default", "", false, 20},
		{"valid override", "10", true, 10},
		{"below floor clamps to one tick", "0", true, 5},
		{"above ceiling clamps", "500", true, 100},
		{"unparseable falls back to default", "abc", true, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("WAIL_IPC_LEAD_MS", tc.env)
			}
			if got := resolveIPCLeadMs(); got != tc.want {
				t.Fatalf("resolveIPCLeadMs() = %d, want %d", got, tc.want)
			}
		})
	}
}
