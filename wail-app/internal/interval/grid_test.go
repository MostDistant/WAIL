package interval

import "testing"

func TestWrapPhase(t *testing.T) {
	const period = 8_000_000 // 8s interval
	cases := []struct {
		delta, period, want int64
	}{
		{0, period, 0},
		{100, period, 100},
		{-100, period, -100},
		{period / 2, period, period / 2},        // exactly half: stays positive
		{-period / 2, period, period / 2},       // negative half wraps to positive
		{period/2 + 1, period, -(period/2 - 1)}, // just past half: re-express as early
		{period + 100, period, 100},             // full period + error: only the error remains
		{-period - 100, period, -100},
		{100, 0, 0}, // defensive: no period, no wrap
	}
	for _, c := range cases {
		if got := WrapPhase(c.delta, c.period); got != c.want {
			t.Errorf("WrapPhase(%d, %d) = %d, want %d", c.delta, c.period, got, c.want)
		}
	}
}

func TestGridAlignerNotReadyUntilAnchorAndOffset(t *testing.T) {
	g := NewGridAligner()
	if g.Ready() {
		t.Fatal("empty aligner must not be Ready")
	}
	g.SetAnchor(8_000_000, 120, 16)
	if g.Ready() {
		t.Fatal("anchor alone must not make aligner Ready")
	}
	g.ObserveServerTime(1_000_000, 500_000, 100_000)
	if g.Ready() {
		t.Fatal("one offset sample must not make aligner Ready (wild-RTT guard)")
	}
	g.ObserveServerTime(1_000_000, 500_000, 100_000)
	if g.Ready() {
		t.Fatal("two offset samples must not make aligner Ready")
	}
	g.ObserveServerTime(1_000_000, 500_000, 100_000)
	if !g.Ready() {
		t.Fatal("anchor + 3 offset samples should make aligner Ready")
	}
}

func TestGridAlignerRejectsZeroAnchor(t *testing.T) {
	g := NewGridAligner()
	g.SetAnchor(0, 120, 16) // old server: field absent
	for i := 0; i < minOffsetSamples; i++ {
		g.ObserveServerTime(1_000_000, 500_000, 100_000)
	}
	if g.Ready() {
		t.Fatal("zero next-boundary (old server) must leave aligner un-Ready")
	}
	if _, ok := g.RoomBPM(); ok {
		t.Fatal("RoomBPM must not be valid without an anchor")
	}
}

// observe3 feeds three identical clean samples (enough for Ready).
func observe3(g *GridAligner, serverNowEstUs, localNowUs int64) {
	for i := 0; i < minOffsetSamples; i++ {
		g.ObserveServerTime(serverNowEstUs, localNowUs, 100_000)
	}
}

// With the offset known, a local grid whose next boundary lands exactly on a
// room boundary has δ = 0; late and early grids report signed errors.
func TestGridAlignerDelta(t *testing.T) {
	g := NewGridAligner()
	// 120 BPM, 16 beats per interval → 8s period.
	g.SetAnchor(8_000_000, 120, 16)
	// Server clock runs 2s ahead of local: server 10s ↔ local 8s.
	observe3(g, 10_000_000, 8_000_000)

	// Room boundaries in local domain: 8s − 2s = 6s, then 14s, 22s, ...
	// A local grid aligned to the room has its next boundary at 14s.
	if d, ok := g.Delta(14_000_000); !ok || d != 0 {
		t.Fatalf("aligned grid: Delta = (%d, %v), want (0, true)", d, ok)
	}
	// Local grid 30ms late.
	if d, _ := g.Delta(14_030_000); d != 30_000 {
		t.Fatalf("late grid: Delta = %d, want 30000", d)
	}
	// Local grid 30ms early.
	if d, _ := g.Delta(13_970_000); d != -30_000 {
		t.Fatalf("early grid: Delta = %d, want -30000", d)
	}
	// A whole period off still reads as aligned (grid phase is mod period).
	if d, _ := g.Delta(22_000_000); d != 0 {
		t.Fatalf("period-shifted grid: Delta = %d, want 0", d)
	}
	// Just under half a period late beats just over half (wraps to early).
	if d, _ := g.Delta(14_000_000 + 4_000_001); d != -3_999_999 {
		t.Fatalf("wrap: Delta = %d, want -3999999", d)
	}
}

func TestGridAlignerOffsetMinRTT(t *testing.T) {
	g := NewGridAligner()
	// The offset comes from the LOWEST-RTT sample: buffering only adds
	// delay, so the cleanest sample is the most accurate. A stalled pong
	// (sleep, Wi-Fi stall — rtt 64s here) must never poison the estimate.
	g.ObserveServerTime(10_000_000, 8_000_000, 100_000)        // offset 2s, clean
	g.ObserveServerTime(10_000_100, 8_000_000, 120_000)        // offset 2.0001s, within 1.5× rtt
	g.ObserveServerTime(74_000_000, 8_000_200, 64_000_000_000) // stalled: offset 66s — rejected
	off, ok := g.OffsetUs()
	if !ok {
		t.Fatal("no offset after samples")
	}
	if off != 2_000_100 {
		t.Fatalf("offset = %d, want 2000100 (best-RTT sample, stall rejected)", off)
	}
	// A later CLEANER sample replaces the estimate.
	g.ObserveServerTime(10_000_400, 8_000_000, 50_000)
	off, _ = g.OffsetUs()
	if off != 2_000_400 {
		t.Fatalf("offset = %d, want 2000400 (cleaner sample wins)", off)
	}
	// A sample beyond 1.5× the best RTT is rejected even without being wild.
	g.ObserveServerTime(11_000_000, 8_000_500, 200_000) // 4× best rtt — rejected
	off, _ = g.OffsetUs()
	if off != 2_000_400 {
		t.Fatalf("offset = %d, want 2000400 (outlier beyond 1.5x rejected)", off)
	}
}

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func TestSlewTempo(t *testing.T) {
	const period = 8_000_000
	// Inside the deadband: exact room tempo, not active.
	if target, active := SlewTempo(120, 5_000, period); active || target != 120 {
		t.Fatalf("deadband: = (%.3f, %v), want (120, false)", target, active)
	}
	// Outside the deadband, local late (positive δ): speed up, proportional.
	target, active := SlewTempo(120, 1_000_000, period)
	if !active || target <= 120 {
		t.Fatalf("late: = (%.3f, %v), want >120, true", target, active)
	}
	// 1s late on an 8s period = 12.5% > cap → clamped to 0.05%.
	if !approxEq(target, 120*(1+SlewMaxFraction)) {
		t.Fatalf("clamp: = %.6f, want %.6f", target, 120*(1+SlewMaxFraction))
	}
	// Early: slow down.
	target, active = SlewTempo(120, -1_000_000, period)
	if !active || !approxEq(target, 120*(1-SlewMaxFraction)) {
		t.Fatalf("early: = (%.6f, %v), want %.6f, true", target, active, 120*(1-SlewMaxFraction))
	}
	// The field case (2026-07-25): a recurring 12ms skew δ on an 8s period
	// nudged at 0.15% (2.6 cents — audible on sustained material). At the
	// cruise clamp it must come out at 0.05% (0.86 cents — below JND).
	target, active = SlewTempo(120, 12_000, period)
	if !active || !approxEq(target, 120*(1+SlewMaxFraction)) {
		t.Fatalf("skew δ: = (%.6f, %v), want %.6f, true", target, active, 120*(1+SlewMaxFraction))
	}
	// The proportional (unclamped) window only exists on very long periods
	// (δ/period < 0.05% with |δ| > 10ms deadband → period > 20s): 12ms on a
	// 32s period (8 bars at 60 BPM) = 0.0375% < cap.
	target, _ = SlewTempo(120, 12_000, 32_000_000)
	if !approxEq(target, 120*1.000375) {
		t.Fatalf("proportional: = %.6f, want %.6f", target, 120*1.000375)
	}
	// Defensive: bad inputs never slew.
	if _, active := SlewTempo(0, 100_000, period); active {
		t.Fatal("zero tempo must not slew")
	}
	if _, active := SlewTempo(120, 100_000, 0); active {
		t.Fatal("zero period must not slew")
	}
}
