package abllink

/*
#include "capture.h"
*/
import "C"

import "unsafe"

// Capture ring defaults. The ring is sized to absorb the worst-case Go GC pause
// so the pure-C callback never drops (ADR-0002). 256 slots × 4096 int16 samples
// is ~2 MB per capture channel.
const (
	DefaultCaptureRingSlots         = 256
	DefaultCaptureMaxSamplesPerSlot = 4096
)

// CaptureBuffer is one Link Audio buffer drained from a Source's ring, off the
// realtime thread. Samples are interleaved int16 (NumFrames × NumChannels).
type CaptureBuffer struct {
	Count           uint64
	NumFrames       int
	NumChannels     int
	SampleRate      uint32
	SessionBeatTime float64
	TempoBPM        float64
	Samples         []int16

	// info is retained so BeginBeats can use the SDK's session-aware mapping.
	info C.struct_abl_link_audio_source_buffer_info
}

// Source subscribes to one local Link Audio channel. Its pure-C callback fills a
// C-owned ring; drain it from a single goroutine via Pop.
type Source struct {
	src        C.struct_abl_link_audio_source
	ring       *C.wail_capture_ring
	maxSamples int
	scratch    []int16
}

// NewSource creates a Link Audio source for the given channel. ringSlots and
// maxSamplesPerSlot may be <= 0 to use defaults. Returns nil if the ring cannot
// be allocated.
func (l *Link) NewSource(id ChannelID, ringSlots, maxSamplesPerSlot int) *Source {
	if ringSlots <= 0 {
		ringSlots = DefaultCaptureRingSlots
	}
	if maxSamplesPerSlot <= 0 {
		maxSamplesPerSlot = DefaultCaptureMaxSamplesPerSlot
	}
	ring := C.wail_capture_ring_create(C.size_t(ringSlots), C.size_t(maxSamplesPerSlot))
	if ring == nil {
		return nil
	}
	var cid C.struct_abl_link_audio_channel_id
	for i := 0; i < 8; i++ {
		cid.bytes[i] = C.uint8_t(id[i])
	}
	src := C.wail_capture_source_create(l.h, cid, ring)
	return &Source{
		src:        src,
		ring:       ring,
		maxSamples: maxSamplesPerSlot,
		scratch:    make([]int16, maxSamplesPerSlot),
	}
}

// Pop drains the oldest buffer from the ring. ok is false when the ring is empty.
// Call from a single drain goroutine.
func (s *Source) Pop() (CaptureBuffer, bool) {
	var info C.struct_abl_link_audio_source_buffer_info
	var n C.size_t
	var dropped C.uint64_t
	got := C.wail_capture_ring_pop(
		s.ring,
		&info,
		(*C.int16_t)(unsafe.Pointer(&s.scratch[0])),
		C.size_t(s.maxSamples),
		&n,
		&dropped,
	)
	if got == 0 {
		return CaptureBuffer{}, false
	}
	ns := int(n)
	samples := make([]int16, ns)
	copy(samples, s.scratch[:ns])
	return CaptureBuffer{
		Count:           uint64(info.count),
		NumFrames:       int(info.num_frames),
		NumChannels:     int(info.num_channels),
		SampleRate:      uint32(info.sample_rate),
		SessionBeatTime: float64(info.session_beat_time),
		TempoBPM:        float64(info.tempo),
		Samples:         samples,
		info:            info,
	}, true
}

// BeginBeats maps a captured buffer's begin time into the given local session
// state's beat timeline. ok is false if the buffer came from a different Link
// session (cross-session buffers can't be mapped).
func (s *Source) BeginBeats(buf *CaptureBuffer, ss *SessionState, quantum float64) (float64, bool) {
	var out C.double
	ok := C.abl_link_audio_source_buffer_info_begin_beats(&buf.info, ss.s, C.double(quantum), &out)
	return float64(out), bool(ok)
}

// Dropped returns the cumulative number of buffers the ring dropped because the
// drainer fell behind (e.g. an over-long GC pause). Should stay 0 in practice.
func (s *Source) Dropped() uint64 {
	return uint64(C.wail_capture_ring_dropped(s.ring))
}

// ChannelID returns the channel this source is subscribed to.
func (s *Source) ChannelID() ChannelID {
	return channelID(C.abl_link_audio_source_id(s.src))
}

// Close destroys the source (stopping the subscription and the callback) and
// then frees the ring. Order matters: the source must be destroyed first so the
// callback can never touch a freed ring.
func (s *Source) Close() {
	C.abl_link_audio_source_destroy(s.src)
	C.wail_capture_ring_destroy(s.ring)
}
