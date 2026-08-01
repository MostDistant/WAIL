// wail_link.h — C-facing Link session API shared by the WAIL Send / WAIL
// Receive CLAP plugins (ADR-0007). Each plugin instance owns one abl_link
// handle (one LAN peer): session membership and timeline math only in the
// spike; Link Audio sink/source wraps come with the Send/Recv halves.
//
// Threading: create/enable/disable/destroy from the plugin's main or activate
// path (never the audio thread — they spawn/join Link's threads). The state
// snapshot/query calls are realtime-safe (abl_link session-state capture is
// lock-free by design).
#pragma once

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>

#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#endif

#ifdef __cplusplus
extern "C" {
#endif

// lb_temp_log_path resolves a writable log path for the bridge plugins'
// diagnostic logs: %TEMP% on Windows (there is no /tmp — the spike's CI
// failure there), /tmp elsewhere.
static inline void lb_temp_log_path(const char *filename, char *buf, unsigned long cap) {
#ifdef _WIN32
   const char *tmp = getenv("TEMP");
   if (!tmp || !tmp[0]) tmp = getenv("TMP");
   if (!tmp || !tmp[0]) tmp = ".";
   snprintf(buf, cap, "%s\\%s", tmp, filename);
#else
   snprintf(buf, cap, "/tmp/%s", filename);
#endif
}

// lb_module_stamp identifies the binary this code was loaded from, by the
// mtime of that file, and reports its path.
//
// A compile-time macro cannot answer the question it looks like it answers.
// __DATE__/__TIME__ record when one translation unit was compiled, not when
// the bundle was linked or installed — edit a sibling source, rebuild, and the
// bundle changes while the stamp does not. And a DAW keeps a plugin mapped for
// the life of the process, so "which build is actually running" is exactly the
// question that comes up, and it must not be answerable wrongly. Reading the
// loaded module's own file cannot go stale: it is the file the running code
// came from.
static inline void lb_module_stamp(char *stamp, unsigned long stampCap, char *path,
                                   unsigned long pathCap) {
   snprintf(stamp, stampCap, "unknown");
   if (path && pathCap) snprintf(path, pathCap, "?");
   const char *found = NULL;
#ifdef _WIN32
   char self[MAX_PATH];
   HMODULE mod = NULL;
   if (GetModuleHandleExA(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
                              GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                          (LPCSTR)(void *)&lb_module_stamp, &mod) &&
       GetModuleFileNameA(mod, self, (DWORD)sizeof(self)))
      found = self;
#else
   Dl_info info;
   if (dladdr((void *)&lb_module_stamp, &info) && info.dli_fname) found = info.dli_fname;
#endif
   if (!found) return;
   if (path && pathCap) snprintf(path, pathCap, "%s", found);

   struct stat st;
   if (stat(found, &st) != 0) return;
   time_t   mt = st.st_mtime;
   struct tm tmv;
#ifdef _WIN32
   if (localtime_s(&tmv, &mt) != 0) return;
#else
   if (!localtime_r(&mt, &tmv)) return;
#endif
   if (strftime(stamp, stampCap, "%Y-%m-%d %H:%M:%S", &tmv) == 0)
      snprintf(stamp, stampCap, "unknown");
}

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
   double        session_beat_time; // buffer begin on the publisher's beat timeline
   double        tempo;
   double        begin_beat;        // buffer begin mapped onto OUR session state
                                    // at the given quantum; 0 = unmappable
                                    // (cross-session buffer) — skip it
} lb_source_buffer;
bool lb_source_pop(lb_source *s, lb_source_buffer *out, double quantum);

#ifdef __cplusplus
}
#endif
