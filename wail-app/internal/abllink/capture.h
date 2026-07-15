/* C-owned capture ring for Link Audio source buffers.
 *
 * ADR-0002 invariant: the Link Audio source callback is a *pure C* function
 * (wail_capture_source_callback) that memcpys the received buffer into this
 * preallocated ring and returns. It never enters the Go runtime, so it runs
 * straight through a Go GC stop-the-world pause and can never block Link's audio
 * thread — structurally eliminating GC-induced capture loss. A Go goroutine
 * drains the ring off-thread via wail_capture_ring_pop.
 *
 * The ring is a single-producer (Link thread) / single-consumer (Go drainer)
 * lock-free queue. The callback allocates nothing and takes no locks.
 */
#ifndef WAIL_CAPTURE_H
#define WAIL_CAPTURE_H

#include <stddef.h>
#include <stdint.h>

#include "abl_link.h"

#ifdef __cplusplus
extern "C" {
#endif

typedef struct wail_capture_ring wail_capture_ring;

/* Create a ring holding num_slots buffers, each up to max_samples_per_slot
 * int16 samples (num_frames * num_channels). Returns NULL on allocation
 * failure. */
wail_capture_ring* wail_capture_ring_create(size_t num_slots, size_t max_samples_per_slot);

/* Destroy a ring. Must not be called while a source using it is still alive. */
void wail_capture_ring_destroy(wail_capture_ring* ring);

/* Create a Link Audio source for channel_id whose pure-C callback pushes into
 * ring. The callback pointer never crosses into Go. */
struct abl_link_audio_source wail_capture_source_create(
  struct abl_link link, struct abl_link_audio_channel_id channel_id, wail_capture_ring* ring);

/* Pop the oldest buffer from the ring (consumer side, called from Go).
 * Returns 1 and fills *out_info + copies samples into out_samples (capacity
 * out_cap int16s, actual count in *out_nsamples) when a buffer was available;
 * returns 0 when the ring is empty. *out_dropped receives the cumulative count
 * of buffers dropped because the ring was full (producer overran the consumer,
 * e.g. an over-long GC pause). */
int wail_capture_ring_pop(wail_capture_ring* ring,
  struct abl_link_audio_source_buffer_info* out_info,
  int16_t* out_samples,
  size_t out_cap,
  size_t* out_nsamples,
  uint64_t* out_dropped);

/* Cumulative buffers dropped because the ring was full. Safe to poll for
 * metrics without consuming a slot. */
uint64_t wail_capture_ring_dropped(wail_capture_ring* ring);

#ifdef __cplusplus
}
#endif

#endif /* WAIL_CAPTURE_H */
