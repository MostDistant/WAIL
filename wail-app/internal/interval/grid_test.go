package interval

import "testing"

// SlewAuthorityBPM is the tempo detector's de-noising bar (ADR-0009): the
// nesting every threshold discussion this repo has had ultimately rests on it
// being exactly SlewMaxFraction expressed in BPM.
func TestSlewAuthorityBPM(t *testing.T) {
	cases := []struct {
		bpm  float64
		want float64
	}{
		{120, 0.06},
		{60, 0.03},
		{174, 0.087},
		{0, 0},
		{-5, 0},
	}
	for _, c := range cases {
		got := SlewAuthorityBPM(c.bpm)
		if d := got - c.want; d > 1e-9 || d < -1e-9 {
			t.Errorf("SlewAuthorityBPM(%v) = %v, want %v", c.bpm, got, c.want)
		}
	}
}
