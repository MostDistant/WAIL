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

#ifdef __cplusplus
}
#endif
