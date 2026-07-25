package main

/*
#cgo pkg-config: opus
#include <opus.h>

// cgo can't pass pointers through the varargs CTL; wrap it.
static int32_t wail_opus_lookahead(OpusEncoder *enc) {
    opus_int32 la = 0;
    opus_encoder_ctl(enc, OPUS_GET_LOOKAHEAD(&la));
    return (int32_t)la;
}
*/
import "C"

// opusEncoderLookahead returns OPUS_GET_LOOKAHEAD in samples per channel: the
// codec's total algorithmic delay. The decoder trims this many samples from
// the front of each stream (Opus pre-skip semantics) so decoded audio lands
// on the sender's grid instead of ~6ms late (measured end-to-end by the
// mini-DAW harness: every transient was +5.94ms).
func opusEncoderLookahead(channels, sampleRate int) int {
	var cerr C.int
	enc := C.opus_encoder_create(C.opus_int32(sampleRate), C.int(channels), C.OPUS_APPLICATION_AUDIO, &cerr)
	if enc == nil {
		return 0
	}
	defer C.opus_encoder_destroy(enc)
	return int(C.wail_opus_lookahead(enc))
}
