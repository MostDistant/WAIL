package main

import (
	"math"
	"testing"

	"github.com/nicholasgasior/wail/wail-app/internal/emit"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

func rms(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		f := float64(v)
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(s)))
}

// TestIntervalCodecLoopback verifies the whole PCM → Opus/WAIF → PCM pipeline
// (encode, wire framing, decode, reassembly) in-process, without Link hardware.
// This exercises exactly the capture-encode and playback-decode path that
// replaces the Rust plugins.
func TestIntervalCodecLoopback(t *testing.T) {
	const (
		sr       = 48000
		channels = 2
		bpm      = 120.0
	)
	cfg := interval.Config{Bars: 1, Quantum: 1} // 1 beat @120 BPM = 0.5s = 24000 frames
	intervalFrames := cfg.IntervalSamples(sr, bpm)

	// A 440 Hz stereo sine over the whole interval.
	pcm := make([]int16, intervalFrames*channels)
	for i := 0; i < intervalFrames; i++ {
		v := int16(math.Sin(2*math.Pi*440*float64(i)/sr) * 8000)
		pcm[i*channels] = v
		pcm[i*channels+1] = v
	}
	inRMS := rms(pcm)

	enc, err := NewIntervalEncoder(channels, sr, 128)
	if err != nil {
		t.Fatalf("NewIntervalEncoder: %v", err)
	}
	const roomIndex = int64(42)
	const streamID = uint16(3)
	frames, nextSeq, err := enc.EncodeInterval(pcm, roomIndex, streamID, 0, bpm, float64(cfg.Quantum), cfg.Bars)
	if err != nil {
		t.Fatalf("EncodeInterval: %v", err)
	}
	wantFrames := (intervalFrames + samplesPerWaifFrame(sr) - 1) / samplesPerWaifFrame(sr)
	if len(frames) != wantFrames {
		t.Fatalf("encoded %d frames, want %d", len(frames), wantFrames)
	}
	if int(nextSeq) != wantFrames {
		t.Fatalf("nextSeq = %d, want %d", nextSeq, wantFrames)
	}

	// Decode each WAIF frame and reassemble.
	dec, err := NewIntervalDecoder(channels, sr)
	if err != nil {
		t.Fatalf("NewIntervalDecoder: %v", err)
	}
	reasm := emit.New(channels, samplesPerWaifFrame(sr))
	for i, wire := range frames {
		f, err := DecodeAudioFrameWire(wire)
		if err != nil {
			t.Fatalf("frame %d wire decode: %v", i, err)
		}
		if f.IntervalIndex != roomIndex || f.StreamID != streamID {
			t.Fatalf("frame %d label=(%d,%d), want (%d,%d)", i, f.IntervalIndex, f.StreamID, roomIndex, streamID)
		}
		if f.FrameSeq != uint32(i) {
			t.Fatalf("frame %d seq = %d, want %d", i, f.FrameSeq, i)
		}
		samples, err := dec.DecodeFrame(f.OpusData)
		if err != nil {
			t.Fatalf("frame %d opus decode: %v", i, err)
		}
		reasm.Add(f.IntervalIndex, int(f.FrameNumber), samples, f.IsFinal, int(f.TotalFrames))
	}

	if !reasm.Complete(roomIndex) {
		t.Fatal("reassembled interval should be complete")
	}
	out, received, total, ok := reasm.Take(roomIndex)
	if !ok || received != wantFrames || total != wantFrames {
		t.Fatalf("Take received=%d total=%d ok=%v", received, total, ok)
	}
	if len(out) != wantFrames*samplesPerWaifFrame(sr)*channels {
		t.Fatalf("reassembled len = %d, want %d", len(out), wantFrames*samplesPerWaifFrame(sr)*channels)
	}

	// Opus is lossy, but energy must be broadly preserved and non-silent.
	outRMS := rms(out)
	if outRMS < 100 {
		t.Fatalf("decoded audio is (near) silent: RMS=%v", outRMS)
	}
	if ratio := outRMS / inRMS; ratio < 0.4 || ratio > 1.8 {
		t.Fatalf("RMS ratio out/in = %.2f (in=%.0f out=%.0f), want ~1", ratio, inRMS, outRMS)
	}
}

// A click placed at a known frame offset must come back at the SAME offset:
// the codec's algorithmic delay (OPUS_GET_LOOKAHEAD) must be trimmed at
// decode start, or every transient lands ~5-6ms late on the room grid
// (measured end-to-end by the mini-DAW harness).
func TestIntervalCodecPreservesClickPosition(t *testing.T) {
	const (
		sr        = 48000
		channels  = 2
		bpm       = 120.0
		clickAt   = 10000 // frames
		tolerance = 64    // frames
	)
	cfg := interval.Config{Bars: 1, Quantum: 1}
	intervalFrames := cfg.IntervalSamples(sr, bpm)

	pcm := make([]int16, intervalFrames*channels)
	for i := 0; i < 480; i++ { // 10ms click at clickAt
		pcm[(clickAt+i)*channels] = 12000
		pcm[(clickAt+i)*channels+1] = 12000
	}

	enc, err := NewIntervalEncoder(channels, sr, 128)
	if err != nil {
		t.Fatalf("NewIntervalEncoder: %v", err)
	}
	frames, _, err := enc.EncodeInterval(pcm, 7, 1, 0, bpm, float64(cfg.Quantum), cfg.Bars)
	if err != nil {
		t.Fatalf("EncodeInterval: %v", err)
	}

	dec, err := NewIntervalDecoder(channels, sr)
	if err != nil {
		t.Fatalf("NewIntervalDecoder: %v", err)
	}
	reasm := emit.New(channels, samplesPerWaifFrame(sr))
	for i, wire := range frames {
		f, err := DecodeAudioFrameWire(wire)
		if err != nil {
			t.Fatalf("frame %d wire decode: %v", i, err)
		}
		samples, err := dec.DecodeFrame(f.OpusData)
		if err != nil {
			t.Fatalf("frame %d opus decode: %v", i, err)
		}
		reasm.Add(f.IntervalIndex, int(f.FrameNumber), samples, f.IsFinal, int(f.TotalFrames))
	}
	// Read through the codec-delay realignment the emit path uses
	// (emit.Reassembler.ShiftedPCM with the decoder's lookahead).
	src, _, _, ok := reasm.Interval(7)
	if !ok {
		t.Fatal("no interval reassembled")
	}
	out := reasm.ShiftedPCM(make([]int16, len(src)), 7, 8, dec.Lookahead())

	pos := -1
	for i := 0; i < len(out)/channels; i++ {
		v := out[i*channels]
		if v < 0 {
			v = -v
		}
		if v > 500 {
			pos = i
			break
		}
	}
	if pos < 0 {
		t.Fatal("click not found in decoded interval")
	}
	if d := pos - clickAt; d < -tolerance || d > tolerance {
		t.Fatalf("click at frame %d, want %d±%d (shifted %d frames = %.1fms — codec delay not trimmed)",
			pos, clickAt, tolerance, d, float64(d)/float64(sr)*1000)
	}
}

// TestIntervalCodecEmptyStillFinal ensures even an empty/near-empty interval
// emits a final frame so receivers learn the total.
func TestIntervalCodecEmptyStillFinal(t *testing.T) {
	enc, err := NewIntervalEncoder(1, 48000, 64)
	if err != nil {
		t.Fatal(err)
	}
	frames, _, err := enc.EncodeInterval(nil, 0, 0, 0, 120, 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("empty interval produced %d frames, want 1 (final)", len(frames))
	}
	f, err := DecodeAudioFrameWire(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	if !f.IsFinal || f.TotalFrames != 1 {
		t.Fatalf("lone frame should be final with total=1, got final=%v total=%d", f.IsFinal, f.TotalFrames)
	}
}

// plcSineWindow returns one interleaved int16 window of a continuous sine
// (phase advances across calls so windows join without a click). Local copy:
// the capture-dump test helpers are !linkstub-only, this test is not.
func plcSineWindow(spf, channels int, freq float64, phase *float64) []int16 {
	s := make([]int16, spf*channels)
	for i := 0; i < spf; i++ {
		v := int16(8000 * math.Sin(*phase))
		for c := 0; c < channels; c++ {
			s[i*channels+c] = v
		}
		*phase += 2 * math.Pi * freq / 48000
	}
	return s
}

// TestDecodePLCBridgesAGap decodes around a deliberately skipped frame: the
// PLC window must be a full frame, non-silent for loud material, and must not
// break the decoder for subsequent real frames.
func TestDecodePLCBridgesAGap(t *testing.T) {
	enc, err := NewIntervalEncoder(2, 48000, 128)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewIntervalDecoder(2, 48000)
	if err != nil {
		t.Fatal(err)
	}

	// Five loud sine windows through the streaming encoder.
	phase := 0.0
	wires := make([][]byte, 5)
	for i := range wires {
		w := plcSineWindow(960, 2, 330, &phase)
		wire, err := enc.EncodeWindow(w, WindowMeta{
			RoomIndex: 0, FrameNumber: uint32(i), Seq: uint32(i),
			IsFinal: i == 4, TotalFrames: 5, BPM: 120, Quantum: 4, Bars: 4,
		})
		if err != nil {
			t.Fatal(err)
		}
		wires[i] = wire
	}

	decode := func(wire []byte) []int16 {
		f, err := DecodeAudioFrameWire(wire)
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := dec.DecodeFrame(f.OpusData)
		if err != nil {
			t.Fatal(err)
		}
		return pcm
	}

	decode(wires[0])
	decode(wires[1])
	// Frame 2 "lost": conceal it.
	plc, err := dec.DecodePLC()
	if err != nil {
		t.Fatalf("DecodePLC: %v", err)
	}
	if len(plc) != 960*2 {
		t.Fatalf("PLC window = %d samples, want %d", len(plc), 960*2)
	}
	var energy int64
	for _, s := range plc {
		energy += int64(s) * int64(s)
	}
	if energy == 0 {
		t.Fatal("PLC of a loud sine should not be silent")
	}
	// PLC output must survive the next real decode (separate scratch).
	firstPLC := plc[0]
	pcm := decode(wires[3])
	if len(pcm) != 960*2 {
		t.Fatalf("post-PLC decode = %d samples", len(pcm))
	}
	if plc[0] != firstPLC {
		t.Fatal("PLC buffer was clobbered by the next real decode")
	}
	decode(wires[4])
}
