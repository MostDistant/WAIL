package capture

import (
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// The engine creates one Assembler per capture channel at channel start,
// baking in the interval config of that moment. When the room config changes
// (SetInterval, or a joiner adopting the room's anchor), the labeler and the
// emit side move to the new grid — the assembler must follow, or its labels
// tick at the old rate against a room clock on the new rate and the peer
// drifts further out of sync every interval. SetConfig is that follow-up.
func TestSetConfigReanchorsOntoNewGrid(t *testing.T) {
	a := NewWindowed(wcfg(), 1, wsr, wframes) // 16-beat grid: 800-frame intervals

	// Establish the stream on the old grid: two windows of interval 0.
	ws := a.AddWindows(beatAt(0), 120, fill(250, 1, 1), 250)
	if len(ws) != 2 || ws[0].IntervalIndex != 0 {
		t.Fatalf("setup windows wrong: %+v", ws)
	}

	// Room switches 4 bars → 2 bars (8-beat intervals: 400 frames, 4 windows).
	a.SetConfig(interval.Config{Bars: 2, Quantum: 4})

	// Feed audio starting at beat 4 (= old-grid interval 0, new-grid interval 0)
	// through beat 13. Beats 8+ are the tell: old grid says interval 0
	// (floor(8/16)), new grid says interval 1 (floor(8/8)).
	ws = a.AddWindows(beatAt(200), 120, fill(450, 1, 2), 450)
	if len(ws) == 0 {
		t.Fatal("no windows after SetConfig — assembler stalled")
	}
	var sawNewGrid bool
	for _, w := range ws {
		if w.IntervalIndex == 1 {
			sawNewGrid = true
		}
		if w.IntervalIndex > 1 {
			t.Fatalf("window skipped to interval %d — unexpected jump", w.IntervalIndex)
		}
	}
	if !sawNewGrid {
		t.Fatal("windows past beat 8 still on the old 16-beat grid (interval 0)")
	}
	if n := a.DroppedLate(); n != 0 {
		t.Fatalf("DroppedLate = %d after SetConfig — assembler must re-anchor, not drop", n)
	}
}

// A BPI increase (2 → 4 bars) makes the new grid's indices SMALLER than the
// old grid's current one. A naive cfg swap would leave every buffer "late"
// (beatIdx < cur.index) and freeze capture; SetConfig must reset instead.
func TestSetConfigBPIIncreaseDoesNotFreeze(t *testing.T) {
	a := NewWindowed(interval.Config{Bars: 2, Quantum: 4}, 1, wsr, wframes) // 8-beat grid

	// Drive into interval 1 on the small grid (past beat 8 = frame 400).
	a.AddWindows(beatAt(400), 120, fill(100, 1, 1), 100)

	// Room switches to 16-beat intervals. Beat 9 is new-grid interval 0 —
	// behind the assembler's old-grid interval 1.
	a.SetConfig(interval.Config{Bars: 4, Quantum: 4})
	ws := a.AddWindows(beatAt(450), 120, fill(100, 1, 2), 100)

	if n := a.DroppedLate(); n != 0 {
		t.Fatalf("DroppedLate = %d — capture froze on a BPI increase", n)
	}
	// Keep feeding until a full new-grid window closes; it must be labeled 0.
	ws = collectWindows(ws, a.AddWindows(beatAt(550), 120, fill(800, 1, 3), 800))
	var first *Window
	for i := range ws {
		if ws[i].IntervalIndex == 0 {
			first = &ws[i]
			break
		}
	}
	if first == nil {
		t.Fatalf("no new-grid interval-0 window emitted: %+v", ws)
	}
}
