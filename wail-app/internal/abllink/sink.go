package abllink

/*
#include <stdlib.h>
#include "abl_link.h"
*/
import "C"

import "unsafe"

// Sink publishes one Link Audio channel. WAIL creates one Sink per remote
// (identity, stream) and tops up its deep internal queue from Go (ADR-0002:
// deep-queue emit, not a C-side pacing thread). The SDK sink queue holds 128
// buffers, so a rare Go/GC pause only risks a far-LAN playout blip, never loss
// of delivered WAN data.
type Sink struct {
	sink C.struct_abl_link_audio_sink
}

// NewSink creates a Link Audio sink announcing a channel with the given display
// name. maxNumSamples bounds one commit (num_frames × num_channels).
func (l *Link) NewSink(name string, maxNumSamples int) *Sink {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	return &Sink{sink: C.abl_link_audio_sink_create(l.h, cName, C.size_t(maxNumSamples))}
}

// Close destroys the sink (its channel disappears from the LAN).
func (s *Sink) Close() {
	C.abl_link_audio_sink_destroy(s.sink)
}

// SetName updates the sink's display name (channel affinity: rename without
// re-minting the channel id).
func (s *Sink) SetName(name string) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	C.abl_link_audio_sink_set_name(s.sink, cName)
}

// MaxNumSamples returns the current maximum samples a committed buffer may hold.
func (s *Sink) MaxNumSamples() int {
	return int(C.abl_link_audio_sink_max_num_samples(s.sink))
}

// RequestMaxNumSamples asks for a larger future buffer capacity.
func (s *Sink) RequestMaxNumSamples(n int) {
	C.abl_link_audio_sink_request_max_num_samples(s.sink, C.size_t(n))
}

// WriteInterleaved commits one buffer of interleaved int16 samples, timestamped
// at beatsAtBegin. It returns false when the buffer could not be sent — either
// no source is subscribed (nobody is listening, so the SDK withholds a slot) or
// the queue is momentarily full; the caller simply tries again next tick. This
// is the normal deep-queue top-up contract, not an error.
func (s *Sink) WriteInterleaved(samples []int16, ss *SessionState, beatsAtBegin, quantum float64, numFrames, numChannels int, sampleRate uint32) bool {
	handle := C.abl_link_audio_sink_retain_buffer(s.sink)
	if !C.abl_link_audio_sink_buffer_is_valid(&handle) {
		// Must release even an invalid handle: retain_buffer always allocates.
		C.abl_link_audio_sink_buffer_release(&handle)
		return false
	}

	n := numFrames * numChannels
	if cap := int(handle.max_num_samples); n > cap {
		n = cap
	}
	if n > len(samples) {
		n = len(samples)
	}
	if n > 0 {
		dst := unsafe.Slice((*int16)(unsafe.Pointer(handle.samples)), n)
		copy(dst, samples[:n])
	}

	return bool(C.abl_link_audio_sink_buffer_commit(
		&handle,
		ss.s,
		C.double(beatsAtBegin),
		C.double(quantum),
		C.size_t(numFrames),
		C.size_t(numChannels),
		C.uint32_t(sampleRate),
	))
}
