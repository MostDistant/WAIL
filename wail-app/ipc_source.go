//go:build !linkstub

package main

import (
	"encoding/binary"
	"log"
	"math"
	"os"
	"strconv"
	"sync"

	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
)

// ipcCaptureRingBlocks bounds the drop-oldest ring of pending PCM blocks. The
// drain goroutine empties it every ~5ms, so this only has to absorb a brief drain
// stall / GC pause; a few hundred host buffers is ample.
const ipcCaptureRingBlocks = 256

// ipcRawBlock is one PCM block received from a Send plugin, already converted to
// the engine's interleaved int16. beginFrame is the plugin's monotonic per-channel
// frame counter at the block's first sample; seq is a receive-order counter used
// only for drop detection (see ipcCaptureSource.PopMapped).
type ipcRawBlock struct {
	samples    []int16
	beginFrame uint64
	channels   int
	sampleRate uint32
	seq        uint64
}

// ipcCaptureSource is a captureSource fed by RawPCM blocks from a CLAP Send plugin
// over IPC. A per-connection reader goroutine calls Push; the engine's drain
// goroutine calls PopMapped. It reconstructs a sample-accurate begin beat by
// anchoring the plugin's frame counter to the app's local Link clock once, then
// extrapolating by sample count — matching the Link Audio path's begin-beat
// fidelity (via Source.BeginBeats) rather than stamping at socket-arrival time,
// which would smear timing by IPC + drain jitter (ADR-0005). The DAW is a Link
// sync peer, so this maps onto the shared session timeline.
type ipcCaptureSource struct {
	nowMicros func() int64 // link.ClockMicros; injected so anchoring is testable
	// leadUs stamps captured audio ahead of the capture clock: the block being
	// captured now reaches the DAW's DAC one output pipeline later, and that
	// DAC time is the audio's true grid time (same fix as the Link Bridge
	// send stamp-ahead). Without it every IPC send peer sits ~output-latency
	// early on the room grid. One constant per setup; the stream-offsets
	// panel (#440) measures it in the field.
	leadUs int64

	mu      sync.Mutex
	ring    []ipcRawBlock
	pushSeq uint64
	dropped uint64
	closed  bool

	haveAnchor   bool
	anchorMicros int64
	anchorFrames uint64
}

func newIPCCaptureSource(link *abllink.Link) *ipcCaptureSource {
	return &ipcCaptureSource{nowMicros: link.ClockMicros, leadUs: ipcSendLeadUs()}
}

// ipcSendLead* — the capture stamp-ahead. Default 10ms (typical callback→DAC
// latency); WAIL_IPC_SEND_LEAD_MS overrides (clamped 0–50).
const (
	ipcSendLeadMsDefault = 10
	ipcSendLeadMsMax     = 50
)

func resolveIPCSendLeadUs() int64 {
	ms := ipcSendLeadMsDefault
	src := "default"
	if v := os.Getenv("WAIL_IPC_SEND_LEAD_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			ms = min(max(n, 0), ipcSendLeadMsMax)
			src = "WAIL_IPC_SEND_LEAD_MS"
		}
	}
	log.Printf("[audio] ipc capture stamp lead: %dms (%s)", ms, src)
	return int64(ms) * 1000
}

var ipcSendLeadUs = sync.OnceValue(resolveIPCSendLeadUs)

// Push enqueues one converted PCM block. On overflow it drops the oldest block and
// bumps the drop counter — the resulting seq gap makes PopMapped's caller re-anchor
// so the lost span reads as silence rather than being spliced out.
func (s *ipcCaptureSource) Push(samples []int16, beginFrame uint64, channels int, sampleRate uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.pushSeq++
	blk := ipcRawBlock{samples: samples, beginFrame: beginFrame, channels: channels, sampleRate: sampleRate, seq: s.pushSeq}
	if len(s.ring) >= ipcCaptureRingBlocks {
		copy(s.ring, s.ring[1:])
		s.ring[len(s.ring)-1] = blk
		s.dropped++
		return
	}
	s.ring = append(s.ring, blk)
}

// ipcStaleStampThresholdUs bounds how far a reconstructed stamp may lie in
// the FUTURE before the anchor is re-armed. Stamps should sit at or behind
// the pop instant (capture → IPC → drain); a stamp seconds ahead means the
// anchor was armed on a stale re-sent block — wail_send.c preserves and
// re-sends the send ring through a WAIL outage, so the first blocks after a
// reconnect can be arbitrarily old. Extrapolating from such an anchor stamps
// all subsequent audio by the outage duration (field finding: an 11-minute
// outage put both peers' capture +82/+83 intervals ahead, frozen for the
// session). 2s: far above any backlog, far below any plausible outage.
const ipcStaleStampThresholdUs = 2_000_000

// PopMapped drains the oldest block from the ring. ok is false when the ring is empty.
// Call from a single drain goroutine.
func (s *ipcCaptureSource) PopMapped(ss *abllink.SessionState, quantum float64) (captureBuffer, float64, bool, bool) {
	s.mu.Lock()
	if len(s.ring) == 0 {
		s.mu.Unlock()
		return captureBuffer{}, 0, false, false
	}
	blk := s.ring[0]
	copy(s.ring, s.ring[1:])
	s.ring = s.ring[:len(s.ring)-1]

	// (Re)anchor on the first block, or if the counter went backwards (a plugin
	// reconnect that reset it). Drops don't need a re-anchor: beginFrame is
	// absolute, so extrapolation stays correct across a gap.
	if !s.haveAnchor || blk.beginFrame < s.anchorFrames {
		s.anchorMicros = s.nowMicros() + s.leadUs
		s.anchorFrames = blk.beginFrame
		s.haveAnchor = true
	}
	stampMicros := s.anchorMicros
	if blk.sampleRate > 0 {
		stampMicros += int64(blk.beginFrame-s.anchorFrames) * 1_000_000 / int64(blk.sampleRate)
	}
	// Stale-ring guard (see ipcStaleStampThresholdUs): re-arm the anchor on
	// the current block when the reconstructed stamp jumps implausibly far
	// into the future.
	if stampMicros-s.nowMicros() > ipcStaleStampThresholdUs {
		s.anchorMicros = s.nowMicros() + s.leadUs
		s.anchorFrames = blk.beginFrame
		stampMicros = s.anchorMicros
	}
	s.mu.Unlock()

	nframes := 0
	if blk.channels > 0 {
		nframes = len(blk.samples) / blk.channels
	}
	return captureBuffer{
		Count:       blk.seq,
		NumFrames:   nframes,
		NumChannels: blk.channels,
		SampleRate:  blk.sampleRate,
		TempoBPM:    ss.Tempo(), // DAW is Link-synced → session tempo is the DAW tempo
		Samples:     blk.samples,
	}, ss.BeatAtTime(stampMicros, quantum), true, true
}

func (s *ipcCaptureSource) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *ipcCaptureSource) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.ring = nil
}

// rawPCMToInt16 converts a RawPCM payload to the engine's interleaved int16.
// The plugin sends float32 LE by default (native DAW format); IPCRawFlagInt16
// selects an int16 LE payload that passes through unchanged.
func rawPCMToInt16(flags byte, pcm []byte) []int16 {
	if flags&IPCRawFlagInt16 != 0 {
		out := make([]int16, len(pcm)/2)
		for i := range out {
			out[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
		}
		return out
	}
	out := make([]int16, len(pcm)/4)
	for i := range out {
		f := math.Float32frombits(binary.LittleEndian.Uint32(pcm[4*i:]))
		v := float64(f) * 32767.0
		switch {
		case v > 32767:
			v = 32767
		case v < -32768:
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
}
