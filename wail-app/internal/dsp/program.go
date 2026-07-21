package dsp

import "math"

// GenerateComplexProgram synthesizes deterministic, music-like stereo program
// material designed to stress a lossy codec path: a detuned saw-stack pad with
// tremolo, a stepping bass line, periodic percussive noise bursts alternating
// between channels, and a low decorrelated noise floor. Unlike a sine sweep,
// this has dense spectra, sharp transients, and inter-channel differences —
// the things Opus artifacts and dropped 20ms windows show up against.
// Returns interleaved stereo int16 at the given rate, peak-normalized to ~-2 dBFS.
func GenerateComplexProgram(frames, rate int) []int16 {
	if frames <= 0 || rate <= 0 {
		return nil
	}
	sr := float64(rate)
	l := make([]float64, frames)
	r := make([]float64, frames)

	// Detuned saw-stack pad: three notes (A3, C4, E4) × three detunes, twelve
	// harmonics at 1/h, 4 Hz tremolo. Distinct phase offsets decorrelate L/R.
	notes := []float64{220.0, 261.63, 329.63}
	detunes := []float64{-0.003, 0, 0.003}
	for ni, note := range notes {
		for di, d := range detunes {
			base := note * (1 + d)
			phL := float64(ni)*1.7 + float64(di)*0.9
			phR := phL + 0.5
			for h := 1; h <= 12; h++ {
				fh := base * float64(h)
				if fh > 0.45*sr {
					break
				}
				amp := 0.055 / float64(h)
				w := 2 * math.Pi * fh / sr
				for i := 0; i < frames; i++ {
					t := float64(i)
					trem := 1 + 0.35*math.Sin(2*math.Pi*4*t/sr)
					l[i] += amp * trem * math.Sin(w*t+phL)
					r[i] += amp * trem * math.Sin(w*t+phR)
				}
			}
		}
	}

	// Stepping bass: root changes every 500ms; running phase keeps the
	// waveform continuous across steps, a short ramp adds an attack.
	bassPat := []float64{55.0, 73.42, 82.41, 61.74}
	stepLen := rate / 2
	var ph1, ph3, ph5 float64
	for i := 0; i < frames; i++ {
		f := bassPat[(i/stepLen)%len(bassPat)]
		ph1 += 2 * math.Pi * f / sr
		ph3 += 2 * math.Pi * 3 * f / sr
		ph5 += 2 * math.Pi * 5 * f / sr
		env := 1.0
		if pos := float64(i%stepLen) / float64(stepLen); pos < 0.01 {
			env = pos / 0.01
		}
		v := env * (0.25*math.Sin(ph1) + 0.08*math.Sin(ph3) + 0.05*math.Sin(ph5))
		l[i] += v
		r[i] += v
	}

	// Deterministic noise via an LCG (no global rand state).
	lcg := uint32(0x1234567)
	noise := func() float64 {
		lcg = lcg*1664525 + 1013904223
		return float64(lcg>>8)/float64(1<<23) - 1
	}

	// Percussive hits: a 30ms exponentially decaying noise burst every 250ms,
	// panned alternately left/right — sharp transients stress the codec and
	// make a dropped window audible/measurable.
	hitLen := rate * 30 / 1000
	hitPeriod := rate / 4
	for start, n := 0, 0; start < frames; start, n = start+hitPeriod, n+1 {
		panL, panR := 0.85, 0.35
		if n%2 == 1 {
			panL, panR = 0.35, 0.85
		}
		for j := 0; j < hitLen && start+j < frames; j++ {
			v := 0.5 * math.Exp(-6*float64(j)/float64(hitLen)) * noise()
			l[start+j] += v * panL
			r[start+j] += v * panR
		}
	}

	// Low decorrelated noise floor.
	for i := 0; i < frames; i++ {
		l[i] += 0.004 * noise()
		r[i] += 0.004 * noise()
	}

	// Peak-normalize to ~-2 dBFS and interleave.
	peak := 0.0
	for i := 0; i < frames; i++ {
		if a := math.Abs(l[i]); a > peak {
			peak = a
		}
		if a := math.Abs(r[i]); a > peak {
			peak = a
		}
	}
	scale := 0.8 * 32767.0
	if peak > 0 {
		scale /= peak
	}
	out := make([]int16, frames*2)
	for i := 0; i < frames; i++ {
		out[i*2] = int16(l[i] * scale)
		out[i*2+1] = int16(r[i] * scale)
	}
	return out
}
