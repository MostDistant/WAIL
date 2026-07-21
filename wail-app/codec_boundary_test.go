package main

import (
	"math"
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/capture"
	"github.com/nicholasgasior/wail/wail-app/internal/emit"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// Codec-inclusive boundary regression: run a continuous signal through the
// full send/receive pipeline — windowed assembler (continuation-padded final
// windows) → Opus encode per window → decode in arrival order → reassembler →
// boundary handoff playback — and compare against the same signal encoded
// continuously with no interval framing. The diff isolates what interval
// framing costs; it must stay at the codec noise floor.
//
// History: zero-padding the final window put a hard cut-to-silence and a
// mid-waveform restart into the encoded stream; the decoded first ~10ms of
// every interval measured −13 dBFS against the reference (an audible glitch
// at every boundary at non-frame-aligned tempos like 124.59 BPM), even after
// the receiver stopped playing the padding itself.
func TestCodecBoundaryStaysAtNoiseFloor(t *testing.T) {
	cfg := interval.Config{Bars: 4, Quantum: 4}
	tempo := 124.59
	rate := 48000
	ch := 2
	spf := samplesPerWaifFrame(rate)
	is := cfg.IntervalSamples(uint32(rate), tempo)
	pad := roundUp(is, spf) - is
	if pad == 0 {
		t.Fatal("test tempo unexpectedly frame-aligned")
	}

	// Continuous source: a chord, so the signal is non-trivial for the codec.
	total := 2*is + 48000
	src := make([]int16, total*ch)
	for i := 0; i < total; i++ {
		ts := float64(i) / float64(rate)
		v := 0.30*math.Sin(2*math.Pi*220*ts) + 0.25*math.Sin(2*math.Pi*277.18*ts) + 0.20*math.Sin(2*math.Pi*329.63*ts)
		s := int16(v * 20000)
		src[i*ch] = s
		src[i*ch+1] = s
	}

	const bitrateKbps = 256 // mirrors engineBitrateKbps (linkstub builds exclude it)
	newCodec := func() (*IntervalEncoder, *IntervalDecoder) {
		enc, err := NewIntervalEncoder(ch, rate, bitrateKbps)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := NewIntervalDecoder(ch, rate)
		if err != nil {
			t.Fatal(err)
		}
		return enc, dec
	}
	roundTrip := func(enc *IntervalEncoder, dec *IntervalDecoder, chunk []int16, seq uint32) []int16 {
		wire, err := enc.EncodeWindow(chunk, WindowMeta{Seq: seq})
		if err != nil {
			t.Fatal(err)
		}
		f, err := DecodeAudioFrameWire(wire)
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := dec.DecodeFrame(f.OpusData)
		if err != nil {
			t.Fatal(err)
		}
		cp := make([]int16, len(pcm))
		copy(cp, pcm)
		return cp
	}

	// Pipeline path: assembler windows through the codec into the reassembler,
	// in emission order (the decoder state mirrors a receiver's).
	enc, dec := newCodec()
	asm := capture.NewWindowed(cfg, ch, uint32(rate), spf)
	reasm := emit.New(ch, spf)
	frameBeats := tempo / 60.0 / float64(rate)
	var seq uint32
	const bufFrames = 480
	for off := 0; off+bufFrames <= total; off += bufFrames {
		beat := float64(off) * frameBeats
		for _, w := range asm.AddWindows(beat, tempo, src[off*ch:(off+bufFrames)*ch], bufFrames) {
			reasm.Add(w.IntervalIndex, w.Number, roundTrip(enc, dec, w.Samples, seq), w.IsFinal, w.Total)
			seq++
		}
	}
	out := runBoundaryPlayback(t, reasm, tempo, cfg, 2)

	// Reference path: the same source encoded continuously, no interval cut.
	refEnc, refDec := newCodec()
	var ref []int16
	for k := 0; (k+1)*spf <= total; k++ {
		ref = append(ref, roundTrip(refEnc, refDec, src[k*spf*ch:(k+1)*spf*ch], uint32(k))...)
	}

	// Align the emitted stream to the source frame domain by its first stamp
	// (as assertBoundaryContinuity does) and diff against the reference.
	bpi := cfg.BeatsPerInterval()
	pos := int(math.Round((out[0].beat - bpi) * 60.0 / tempo * float64(rate)))
	played := make([]int16, pos*ch, total*ch)
	for _, c := range out {
		played = append(played, c.samples...)
	}
	rmsDiff := func(fromFrame, frames int) float64 {
		var acc float64
		cnt := 0
		for i := fromFrame * ch; i < (fromFrame+frames)*ch && i < len(played) && i < len(ref); i++ {
			d := float64(played[i]) - float64(ref[i])
			acc += d * d
			cnt++
		}
		if cnt == 0 {
			t.Fatalf("empty diff window at frame %d", fromFrame)
		}
		return math.Sqrt(acc / float64(cnt))
	}
	db := func(x float64) float64 { return 20 * math.Log10(x/20000+1e-12) }

	// The whole boundary neighborhood — the interval tail, the pad region, and
	// the first 20ms after the switch to the next interval's stream — must sit
	// at the codec noise floor (≈ −48 dBFS measured; −36 dBFS as the gate).
	const floor = 300.0
	zones := []struct {
		name        string
		from, count int
	}{
		{"pre-boundary 10ms", is - 480, 480},
		{"pad region", is, pad},
		{"post-switch 20ms", is + pad, 960},
		{"steady mid-interval", is + is/2, 960},
	}
	for _, z := range zones {
		got := rmsDiff(z.from, z.count)
		t.Logf("%-22s rms=%8.2f (%6.1f dBFS)", z.name, got, db(got))
		if got > floor {
			t.Errorf("%s: rms diff %.1f (%.1f dBFS) above noise-floor gate %.0f", z.name, got, db(got), floor)
		}
	}
}
