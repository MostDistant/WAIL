// linkbridge_link.h — C-facing Link session API for the Link Bridge plugins
// (ADR-0007). Each plugin instance owns one abl_link handle (one LAN peer):
// session membership and timeline math only in the spike; Link Audio
// sink/source wraps come with the Send/Recv halves.
//
// Threading: create/enable/disable/destroy from the plugin's main or activate
// path (never the audio thread — they spawn/join Link's threads). The state
// snapshot/query calls are realtime-safe (abl_link session-state capture is
// lock-free by design).
#pragma once

#include <stdbool.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct lb_link lb_link;

lb_link *lb_create(double tempo);
void lb_destroy(lb_link *l);
void lb_enable(lb_link *l, bool on);
// Link Audio is a separate enable on top of the base session — without it the
// peer syncs (visible in peer counts) but neither announces nor hears channels.
void lb_enable_audio(lb_link *l, bool on);

uint64_t lb_num_peers(lb_link *l);
double lb_tempo(lb_link *l);

// Session timeline (capture a snapshot, query, release — the abl_link pattern;
// safe from process()).
typedef struct lb_state lb_state;
lb_state *lb_capture(lb_link *l);
void lb_release(lb_state *s);
double lb_beat_at_time(lb_state *s, int64_t micros, double quantum);
int64_t lb_time_at_beat(lb_state *s, double beat, double quantum);
int64_t lb_clock_micros(lb_link *l);

// --- Link Audio sink (publishing; Send plugin) ---
// Commit is realtime-safe (the SDK's retain/commit is designed for the audio
// thread). Create/destroy/set_name are main-thread calls.
typedef struct lb_sink lb_sink;
lb_sink *lb_sink_create(lb_link *l, const char *name, size_t max_samples);
void lb_sink_destroy(lb_sink *s);
void lb_sink_set_name(lb_sink *s, const char *name);
// Commit one stereo block of float32 (converted to int16, clamped). l may be
// NULL (silence); r may be NULL (duplicates l). Returns false when no source
// subscribes or the queue is momentarily full (try next block, not an error).
bool lb_sink_commit(lb_sink *s, lb_state *st, double beats_at_begin, double quantum,
                    const float *l, const float *r, size_t num_frames, uint32_t sample_rate);

// --- Link Audio channels + source (subscribing; tests now, Recv later) ---
// Discovery is main-thread only (not realtime-safe).
#define LB_MAX_CHANNELS 64
typedef struct {
   uint64_t id_u64;                    // channel id, packed (8 bytes)
   char     name[128];
   char     peer_name[128];
} lb_channel_info;
size_t lb_channels(lb_link *l, lb_channel_info *out, size_t max);

// A source runs the SDK callback (Link-managed thread) into a lock-free SPSC
// ring — the ADR-0002 pattern, pure C/C++ all the way. Pop from any one
// non-Link thread (test driver, or the Recv plugin's process()).
typedef struct lb_source lb_source;
lb_source *lb_source_create(lb_link *l, uint64_t channel_id_u64);
void lb_source_destroy(lb_source *s);
typedef struct {
   const int16_t *samples; // interleaved; valid until the next lb_source_pop
   size_t        num_frames;
   size_t        num_channels;
   uint32_t      sample_rate;
   uint64_t      count;             // publisher's buffer counter (gap detection)
   double        session_beat_time; // buffer begin on the session timeline
   double        tempo;
} lb_source_buffer;
bool lb_source_pop(lb_source *s, lb_source_buffer *out);

#ifdef __cplusplus
}
#endif
