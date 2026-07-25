package main

import (
	"log"

	"gopkg.in/hraban/opus.v2"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// Interval Opus codec: turns a full interval of PCM into a sequence of 20ms WAIF
// frames and back. This is the encode/decode that used to live in the Rust
// plugins; on the Link Audio path it runs in the Go app (capture encodes,
// playback decodes). Pure in-process (libopus via cgo) — no Link hardware — so
// the whole PCM→WAIF→PCM pipeline is loopback-testable.

const (
	waifFrameMs = 20 // WAIF frames are 20ms Opus packets (CONTEXT.md glossary)
)

// samplesPerWaifFrame returns the per-channel sample count of one 20ms frame.
func samplesPerWaifFrame(sampleRate int) int { return sampleRate * waifFrameMs / 1000 }

// intervalPlayoutFrames is the exact frame count the playout reader emits for
// one interval — deliberately NOT rounded up to WAIF-frame granularity. When
// the interval isn't a multiple of the 20ms window (most tempos), the final
// window carries padding past the interval end; playing zero padding splices
// silence into continuous audio and stamps it past the next interval's anchor
// — an audible click at every boundary.
func intervalPlayoutFrames(cfg interval.Config, sampleRate uint32, tempoBPM float64) int {
	return cfg.IntervalSamples(sampleRate, tempoBPM)
}

// intervalPaddedFrames is the interval length rounded up to whole WAIF
// windows — the full extent of the reassembled buffer including the final
// window's padding.
func intervalPaddedFrames(cfg interval.Config, sampleRate uint32, tempoBPM float64) int {
	return roundUp(intervalPlayoutFrames(cfg, sampleRate, tempoBPM), samplesPerWaifFrame(int(sampleRate)))
}

// padAudioFloor is the peak below which final-window padding is treated as
// silence (a zero-padding pre-continuation sender): decoded zeros stay within
// codec ringing of a few dozen; real program material sits far above.
const padAudioFloor = 256

// padCarriesAudio reports whether interval PCM s holds real audio in
// [fromFrame, toFrame) — the continuation region a new sender fills with the
// next interval's head. False when the region is absent or silent, keeping
// old zero-padding senders on the truncate-at-interval-end playout path.
func padCarriesAudio(s []int16, fromFrame, toFrame, channels int) bool {
	lo, hi := fromFrame*channels, toFrame*channels
	if lo < 0 || hi > len(s) {
		return false
	}
	for _, v := range s[lo:hi] {
		if v > padAudioFloor || v < -padAudioFloor {
			return true
		}
	}
	return false
}

func roundUp(n, multiple int) int {
	if multiple <= 0 {
		return n
	}
	return ((n + multiple - 1) / multiple) * multiple
}

// IntervalEncoder Opus-encodes interval PCM into WAIF frames for one stream.
type IntervalEncoder struct {
	enc             *opus.Encoder
	channels        int
	sampleRate      int
	samplesPerFrame int // per channel
	opusBuf         []byte
	scratch         []int16 // reused padded frame buffer
}

// NewIntervalEncoder creates an encoder for the given channel count and rate.
func NewIntervalEncoder(channels, sampleRate, bitrateKbps int) (*IntervalEncoder, error) {
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppAudio)
	if err != nil {
		return nil, err
	}
	if bitrateKbps > 0 {
		if err := enc.SetBitrate(bitrateKbps * 1000); err != nil {
			log.Printf("[audio] warn: set Opus bitrate %dkbps failed, using default: %v", bitrateKbps, err)
		}
	}
	// Max encoder effort: a few % of one core per stereo stream, off the RT
	// thread — pure quality win. (No DTX, no in-band FEC: DTX adds comfort-noise
	// transitions to music; FEC is SILK-only and pointless on our TCP transport.)
	if err := enc.SetComplexity(10); err != nil {
		log.Printf("[audio] warn: set Opus complexity failed, using default: %v", err)
	}
	spf := samplesPerWaifFrame(sampleRate)
	return &IntervalEncoder{
		enc:             enc,
		channels:        channels,
		sampleRate:      sampleRate,
		samplesPerFrame: spf,
		opusBuf:         make([]byte, 4096),
		scratch:         make([]int16, spf*channels),
	}, nil
}

// WindowMeta labels one 20ms window for WAIF encoding. FrameNumber is the
// window's index within its interval; Seq is the running per-stream frame
// sequence (WAN loss detection). The trailer fields (TotalFrames, BPM,
// Quantum, Bars) are written only when IsFinal is set.
type WindowMeta struct {
	RoomIndex   int64
	StreamID    uint16
	FrameNumber uint32
	Seq         uint32
	IsFinal     bool
	TotalFrames uint32
	BPM         float64
	Quantum     float64
	Bars        uint32
}

// EncodeWindow Opus-encodes one 20ms window into one WAIF frame. A chunk
// shorter than a full window is zero-padded; windows must be fed in order on
// one encoder (Opus is stateful). This is the streaming unit: the capture path
// calls it as each window fills, so frames leave in real time during the
// interval instead of bursting at its boundary.
func (e *IntervalEncoder) EncodeWindow(chunk []int16, m WindowMeta) ([]byte, error) {
	frameLen := e.samplesPerFrame * e.channels
	if len(chunk) < frameLen {
		padded := e.zeroScratch()
		copy(padded, chunk)
		chunk = padded
	} else if len(chunk) > frameLen {
		chunk = chunk[:frameLen]
	}

	n, err := e.enc.Encode(chunk, e.opusBuf)
	if err != nil {
		return nil, err
	}
	opusData := make([]byte, n)
	copy(opusData, e.opusBuf[:n])

	f := &AudioFrame{
		IntervalIndex: m.RoomIndex,
		StreamID:      m.StreamID,
		FrameNumber:   m.FrameNumber,
		FrameSeq:      m.Seq,
		Channels:      uint16(e.channels),
		OpusData:      opusData,
		IsFinal:       m.IsFinal,
	}
	if m.IsFinal {
		f.SampleRate = uint32(e.sampleRate)
		f.TotalFrames = m.TotalFrames
		f.BPM = m.BPM
		f.Quantum = m.Quantum
		f.Bars = m.Bars
	}
	return EncodeAudioFrameWire(f), nil
}

// EncodeInterval splits interleaved interval PCM into 20ms Opus WAIF frames
// labeled with the shared room interval index. seqStart is the running per-stream
// frame sequence (for WAN loss detection); the returned nextSeq continues it. The
// final frame carries the interval trailer (rate, total frames, bpm, quantum,
// bars). A short trailing chunk is zero-padded to a full Opus frame.
func (e *IntervalEncoder) EncodeInterval(pcm []int16, roomIndex int64, streamID uint16, seqStart uint32, bpm, quantum float64, bars uint32) ([][]byte, uint32, error) {
	frameLen := e.samplesPerFrame * e.channels
	if frameLen == 0 {
		return nil, seqStart, nil
	}
	numFrames := (len(pcm) + frameLen - 1) / frameLen
	if numFrames == 0 {
		numFrames = 1 // always emit at least a final frame so receivers learn the total
	}

	frames := make([][]byte, 0, numFrames)
	seq := seqStart
	for i := 0; i < numFrames; i++ {
		start := i * frameLen
		var chunk []int16
		if start >= len(pcm) {
			chunk = nil // EncodeWindow pads to a full frame of silence
		} else if end := start + frameLen; end <= len(pcm) {
			chunk = pcm[start:end]
		} else {
			chunk = pcm[start:]
		}

		wire, err := e.EncodeWindow(chunk, WindowMeta{
			RoomIndex:   roomIndex,
			StreamID:    streamID,
			FrameNumber: uint32(i),
			Seq:         seq,
			IsFinal:     i == numFrames-1,
			TotalFrames: uint32(numFrames),
			BPM:         bpm,
			Quantum:     quantum,
			Bars:        bars,
		})
		if err != nil {
			return frames, seq, err
		}
		frames = append(frames, wire)
		seq++
	}
	return frames, seq, nil
}

func (e *IntervalEncoder) zeroScratch() []int16 {
	for i := range e.scratch {
		e.scratch[i] = 0
	}
	return e.scratch
}

// IntervalDecoder Opus-decodes WAIF frames back to PCM for one stream.
type IntervalDecoder struct {
	dec             *opus.Decoder
	channels        int
	samplesPerFrame int
	out             []int16
	plcOut          []int16 // separate scratch: a PLC window must survive the next real decode
	lookahead       int
}

// NewIntervalDecoder creates a decoder for the given channels and rate.
func NewIntervalDecoder(channels, sampleRate int) (*IntervalDecoder, error) {
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		return nil, err
	}
	spf := samplesPerWaifFrame(sampleRate)
	return &IntervalDecoder{
		dec:             dec,
		channels:        channels,
		samplesPerFrame: spf,
		out:             make([]int16, spf*channels),
		lookahead:       opusEncoderLookahead(channels, sampleRate),
	}, nil
}

// Lookahead returns the codec's algorithmic delay in samples per channel
// (OPUS_GET_LOOKAHEAD): every decoded stream runs this many samples late.
// The emit path realigns by reading the reassembly shifted by this amount.
func (d *IntervalDecoder) Lookahead() int { return d.lookahead }

// DecodeFrame decodes one WAIF frame's Opus payload to interleaved PCM. The
// returned slice aliases an internal buffer and is only valid until the next
// DecodeFrame call on this decoder — copy it to retain it. (Reassembler.Add,
// the sole production consumer, copies immediately.)
func (d *IntervalDecoder) DecodeFrame(opusData []byte) ([]int16, error) {
	n, err := d.dec.Decode(opusData, d.out)
	if err != nil {
		return nil, err
	}
	return d.out[:n*d.channels], nil
}

// DecodePLC synthesizes one 20ms window of packet-loss concealment from the
// decoder's current state — libopus extrapolates the previous audio and fades
// over sustained loss. Call it in stream order for each frame that never
// arrived, between the decodes of its neighbors, so the decoder state stays
// continuous and the next real frame splices smoothly. The returned slice
// aliases an internal buffer valid until the next DecodePLC call — copy it.
func (d *IntervalDecoder) DecodePLC() ([]int16, error) {
	if d.plcOut == nil {
		d.plcOut = make([]int16, d.samplesPerFrame*d.channels)
	}
	if err := d.dec.DecodePLC(d.plcOut); err != nil {
		return nil, err
	}
	return d.plcOut, nil
}
