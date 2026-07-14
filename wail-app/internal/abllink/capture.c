/* See capture.h. Pure-C capture ring + source callback (ADR-0002). */
#include "capture.h"

#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

struct wail_capture_ring
{
  size_t num_slots;
  size_t max_samples; /* per slot */

  /* Parallel arrays, one entry per slot. */
  struct abl_link_audio_source_buffer_info* infos;
  int16_t* samples;   /* num_slots * max_samples */
  size_t* nsamples;   /* actual samples written into each slot */

  /* SPSC indices. write advances on the Link thread; read on the Go drainer.
   * A slot is occupied when write != read; full when write - read == num_slots. */
  atomic_size_t write;
  atomic_size_t read;
  atomic_uint_least64_t dropped;
};

wail_capture_ring* wail_capture_ring_create(size_t num_slots, size_t max_samples_per_slot)
{
  if (num_slots == 0 || max_samples_per_slot == 0)
  {
    return NULL;
  }
  wail_capture_ring* r = (wail_capture_ring*)calloc(1, sizeof(wail_capture_ring));
  if (!r)
  {
    return NULL;
  }
  r->num_slots = num_slots;
  r->max_samples = max_samples_per_slot;
  r->infos = (struct abl_link_audio_source_buffer_info*)calloc(
    num_slots, sizeof(struct abl_link_audio_source_buffer_info));
  r->samples = (int16_t*)calloc(num_slots * max_samples_per_slot, sizeof(int16_t));
  r->nsamples = (size_t*)calloc(num_slots, sizeof(size_t));
  atomic_init(&r->write, 0);
  atomic_init(&r->read, 0);
  atomic_init(&r->dropped, 0);
  if (!r->infos || !r->samples || !r->nsamples)
  {
    wail_capture_ring_destroy(r);
    return NULL;
  }
  return r;
}

void wail_capture_ring_destroy(wail_capture_ring* ring)
{
  if (!ring)
  {
    return;
  }
  free(ring->infos);
  free(ring->samples);
  free(ring->nsamples);
  free(ring);
}

/* The pure-C source callback. Runs on a Link-managed thread. Copies the buffer
 * into the ring and returns. No allocation, no locking, no Go entry. */
static void wail_capture_source_callback(
  const struct abl_link_audio_source_buffer* buffer, void* context)
{
  wail_capture_ring* r = (wail_capture_ring*)context;
  if (!r || !buffer)
  {
    return;
  }

  const size_t write = atomic_load_explicit(&r->write, memory_order_relaxed);
  const size_t read = atomic_load_explicit(&r->read, memory_order_acquire);
  if (write - read >= r->num_slots)
  {
    /* Ring full: the drainer fell behind. Drop this buffer and count it. */
    atomic_fetch_add_explicit(&r->dropped, 1, memory_order_relaxed);
    return;
  }

  const size_t slot = write % r->num_slots;
  size_t n = buffer->info.num_frames * buffer->info.num_channels;
  if (n > r->max_samples)
  {
    n = r->max_samples; /* defensive: never overflow the slot */
  }

  r->infos[slot] = buffer->info;
  r->nsamples[slot] = n;
  if (n > 0 && buffer->samples)
  {
    memcpy(r->samples + slot * r->max_samples, buffer->samples, n * sizeof(int16_t));
  }

  /* Publish: make the slot writes visible before advancing write. */
  atomic_store_explicit(&r->write, write + 1, memory_order_release);
}

struct abl_link_audio_source wail_capture_source_create(
  struct abl_link link, struct abl_link_audio_channel_id channel_id, wail_capture_ring* ring)
{
  return abl_link_audio_source_create(
    link, channel_id, wail_capture_source_callback, ring);
}

int wail_capture_ring_pop(wail_capture_ring* ring,
  struct abl_link_audio_source_buffer_info* out_info,
  int16_t* out_samples,
  size_t out_cap,
  size_t* out_nsamples,
  uint64_t* out_dropped)
{
  if (out_dropped)
  {
    *out_dropped = atomic_load_explicit(&ring->dropped, memory_order_relaxed);
  }

  const size_t read = atomic_load_explicit(&ring->read, memory_order_relaxed);
  const size_t write = atomic_load_explicit(&ring->write, memory_order_acquire);
  if (read == write)
  {
    return 0; /* empty */
  }

  const size_t slot = read % ring->num_slots;
  size_t n = ring->nsamples[slot];
  if (n > out_cap)
  {
    n = out_cap;
  }
  if (out_info)
  {
    *out_info = ring->infos[slot];
  }
  if (out_samples && n > 0)
  {
    memcpy(out_samples, ring->samples + slot * ring->max_samples, n * sizeof(int16_t));
  }
  if (out_nsamples)
  {
    *out_nsamples = n;
  }

  /* Release the slot back to the producer. */
  atomic_store_explicit(&ring->read, read + 1, memory_order_release);
  return 1;
}

uint64_t wail_capture_ring_dropped(wail_capture_ring* ring)
{
  return atomic_load_explicit(&ring->dropped, memory_order_relaxed);
}
