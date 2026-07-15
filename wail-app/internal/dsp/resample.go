// Package dsp holds small, dependency-free signal-processing helpers shared
// across the app (capture edge, test-WAV sender).
package dsp

// ResampleLinearInterleaved resamples interleaved int16 audio from srcRate to
// dstRate with per-channel linear interpolation. It works for any channel count
// and returns src unchanged when no resampling is needed (equal rates, empty
// input, or a non-positive srcRate). Basic but adequate for a jam on-ramp; a
// higher-quality resampler is a follow-up (migration-plan open question §68).
func ResampleLinearInterleaved(src []int16, channels, srcRate, dstRate int) []int16 {
	if channels < 1 {
		channels = 1
	}
	if srcRate == dstRate || srcRate <= 0 || len(src) == 0 {
		return src
	}
	srcFrames := len(src) / channels
	dstFrames := srcFrames * dstRate / srcRate
	out := make([]int16, dstFrames*channels)
	ratio := float64(srcRate) / float64(dstRate)
	for i := 0; i < dstFrames; i++ {
		pos := float64(i) * ratio
		j := int(pos)
		frac := pos - float64(j)
		for c := 0; c < channels; c++ {
			a := int(src[j*channels+c])
			b := a
			if (j+1)*channels+c < len(src) {
				b = int(src[(j+1)*channels+c])
			}
			out[i*channels+c] = int16(float64(a) + frac*float64(b-a))
		}
	}
	return out
}
