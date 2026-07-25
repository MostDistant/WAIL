package abllink

import (
	"testing"
	"time"
)

// MonoMicros is the machine monotonic clock shared with the CLAP plugins
// (wail_mono_micros): the bridge domain the app converts Link-timeline chunk
// stamps into, so a plugin can render against its host sample clock. It must
// be monotonic and tick at real-time rate.
func TestMonoMicrosMonotonicAndRealTime(t *testing.T) {
	start := MonoMicros()
	time.Sleep(50 * time.Millisecond)
	end := MonoMicros()

	if end <= start {
		t.Fatalf("not monotonic: start=%d end=%d", start, end)
	}
	elapsed := end - start
	// 50ms of wall time, generous bounds for scheduling jitter.
	if elapsed < 40_000 || elapsed > 500_000 {
		t.Fatalf("50ms sleep measured as %d µs — wrong clock rate?", elapsed)
	}
}
