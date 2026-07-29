// WAIL Send (ADR-0007) — publishes the DAW track this instance sits on
// as a Link Audio channel. One channel per instance, named from the host's
// track-info (no "WAIL · " prefix — the prefix marks room-published channels;
// this is a raw LAN channel any Link Audio app can hear, including the WAIL
// app's capture).
//
// Audio path (process()): passthrough, then commit the input block to the
// Link Audio sink, stamped with the session beat at commit time. Link Audio
// is 48kHz-only: at any other host rate the instance passes audio through
// but publishes nothing, and says so loudly (log + port name). Realtime
// discipline: process() only calls the SDK's realtime-safe retain/commit
// (plus float→int16 conversion); peer/sink lifecycle lives on the activate
// path. Full factory + vtable (Bitwig scan discipline).

#include <stdarg.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "clap/clap.h"
#include "wail_link.h"

#define LB_QUANTUM 4.0 // phase lens for buffer stamps (beats; beat values are absolute)
// Stamp-ahead: the block being committed now reaches the sender's DAC one
// output pipeline later — that IS the audio's correct session-grid play time,
// so stamps run ahead of the callback clock. It doubles as the receiver's
// delivery margin (network + drain cadence). 10ms approximates the typical
// callback→DAC latency; the residual error is one constant, field-measurable
// per setup (same class as the recv path's output-path constant).
#define LBS_STAMP_AHEAD_US 10000

typedef struct {
   clap_plugin_t      plugin;
   const clap_host_t *host;

   lb_link *link;
   lb_sink *sink;
   bool     rate_ok; // host runs at 48k

   // Track name: written on the main thread (activate / track_info.changed),
   // read on the same thread for sink naming — sink_set_name is main-thread
   // anyway, so no cross-thread handoff is needed.
   char track_name[CLAP_NAME_SIZE];

   FILE *log;
   bool  logged_commit;
} lb_send;

static void lbs_log(lb_send *self, const char *fmt, ...) {
   if (!self->log) return;
   va_list ap;
   va_start(ap, fmt);
   vfprintf(self->log, fmt, ap);
   va_end(ap);
   fputc('\n', self->log);
   fflush(self->log);
}

// refresh_track_name queries clap.track-info for this instance's track and
// renames the sink channel to match. [main thread]
static void refresh_track_name(lb_send *self) {
   const clap_host_track_info_t *ti = self->host->get_extension(self->host, CLAP_EXT_TRACK_INFO);
   if (!ti || !ti->get)
      ti = self->host->get_extension(self->host, CLAP_EXT_TRACK_INFO_COMPAT);
   if (!ti || !ti->get)
      return;
   clap_track_info_t info;
   memset(&info, 0, sizeof(info));
   if (!ti->get(self->host, &info))
      return;
   if (!(info.flags & CLAP_TRACK_INFO_HAS_TRACK_NAME) || info.name[0] == '\0')
      return;
   if (strncmp(self->track_name, info.name, CLAP_NAME_SIZE - 1) == 0)
      return;
   snprintf(self->track_name, sizeof(self->track_name), "%s", info.name);
   if (self->sink) {
      lb_sink_set_name(self->sink, self->track_name);
      lbs_log(self, "channel renamed to \"%s\"", self->track_name);
   }
}

static clap_process_status CLAP_ABI lbs_process(const clap_plugin_t *plugin, const clap_process_t *p) {
   lb_send *self = plugin->plugin_data;
   uint32_t n = p->frames_count;

   // Passthrough: input 0 → output 0 (inline insert stays audible).
   if (p->audio_inputs_count > 0 && p->audio_outputs_count > 0) {
      const clap_audio_buffer_t *in = &p->audio_inputs[0];
      clap_audio_buffer_t *out = &p->audio_outputs[0];
      for (uint32_t ch = 0; ch < out->channel_count && ch < in->channel_count; ch++)
         if (out->data32[ch] && in->data32[ch] && out->data32[ch] != in->data32[ch])
            memcpy(out->data32[ch], in->data32[ch], n * sizeof(float));
   }

   if (!self->sink || !self->rate_ok || p->audio_inputs_count == 0)
      return CLAP_PROCESS_CONTINUE;

   const clap_audio_buffer_t *in = &p->audio_inputs[0];
   if (!in->data32 || !in->data32[0])
      return CLAP_PROCESS_CONTINUE;

   // Stamp the block with the session beat at its *play* time (stamp-ahead —
   // see LBS_STAMP_AHEAD_US).
   lb_state *st = lb_capture(self->link);
   double beat = lb_beat_at_time(st, lb_clock_micros(self->link) + LBS_STAMP_AHEAD_US, LB_QUANTUM);
   bool ok = lb_sink_commit(self->sink, st, beat, LB_QUANTUM,
                            in->data32[0], in->channel_count > 1 ? in->data32[1] : NULL, n, 48000);
   lb_release(st);
   if (!self->logged_commit) {
      self->logged_commit = true;
      lbs_log(self, "first commit: %s (frames=%u)", ok ? "ok" : "WITHHELD (no source subscribed?)", n);
   }
   return CLAP_PROCESS_CONTINUE;
}

static bool CLAP_ABI lbs_init(const clap_plugin_t *plugin) {
   (void)plugin;
   return true;
}
static void CLAP_ABI lbs_destroy(const clap_plugin_t *plugin) {
   lb_send *self = plugin->plugin_data;
   free(self);
}
static bool CLAP_ABI lbs_activate(const clap_plugin_t *plugin, double sr, uint32_t minf, uint32_t maxf) {
   (void)minf;
   (void)maxf;
   lb_send *self = plugin->plugin_data;
   { char lp[512]; lb_temp_log_path("wail-send.log", lp, sizeof(lp)); self->log = fopen(lp, "a"); }
   self->rate_ok = (sr == 48000.0);
   self->link = lb_create(120.0);
   lb_enable(self->link, true);
   lb_enable_audio(self->link, true);
   refresh_track_name(self);
   const char *name = self->track_name[0] ? self->track_name : "WAIL Send";
   if (self->rate_ok) {
      self->sink = lb_sink_create(self->link, name, 16384);
      lbs_log(self, "=== send activated: host=\"%s %s\" sr=%.0f channel=\"%s\" ===",
              self->host && self->host->name ? self->host->name : "?",
              self->host && self->host->version ? self->host->version : "?", sr, name);
   } else {
      lbs_log(self, "!!! send activated at sr=%.0f — Link Audio is 48kHz-only; "
                    "this instance passes audio but publishes NOTHING "
                    "(host=\"%s %s\")", sr,
              self->host && self->host->name ? self->host->name : "?",
              self->host && self->host->version ? self->host->version : "?");
   }
   return true;
}
static void CLAP_ABI lbs_deactivate(const clap_plugin_t *plugin) {
   lb_send *self = plugin->plugin_data;
   if (self->sink) {
      lb_sink_destroy(self->sink);
      self->sink = NULL;
   }
   if (self->link) {
      lb_enable(self->link, false);
      lb_destroy(self->link);
      self->link = NULL;
   }
   if (self->log) {
      fprintf(self->log, "=== send deactivated ===\n");
      fclose(self->log);
      self->log = NULL;
   }
}
static bool CLAP_ABI lbs_start(const clap_plugin_t *p) {
   (void)p;
   return true;
}
static void CLAP_ABI lbs_stop(const clap_plugin_t *p) {
   (void)p;
}
static void CLAP_ABI lbs_reset(const clap_plugin_t *p) {
   (void)p;
}

// --- audio ports: 1 stereo in (main) + 1 stereo out (passthrough) ---

static uint32_t CLAP_ABI lbs_ap_count(const clap_plugin_t *p, bool is_input) {
   (void)p;
   (void)is_input;
   return 1;
}
static bool CLAP_ABI lbs_ap_get(const clap_plugin_t *p, uint32_t idx, bool is_input, clap_audio_port_info_t *info) {
   (void)p;
   if (idx != 0) return false;
   info->id = 0;
   snprintf(info->name, sizeof(info->name), "%s", is_input ? "Input" : "Output");
   info->flags = CLAP_AUDIO_PORT_IS_MAIN;
   info->channel_count = 2;
   info->port_type = CLAP_PORT_STEREO;
   info->in_place_pair = CLAP_INVALID_ID;
   return true;
}
static const clap_plugin_audio_ports_t lbs_audio_ports = {lbs_ap_count, lbs_ap_get};

// --- track-info: rename the channel when the track renames ---

static void CLAP_ABI lbs_track_info_changed(const clap_plugin_t *plugin) {
   refresh_track_name(plugin->plugin_data);
}
static const clap_plugin_track_info_t lbs_track_info = {lbs_track_info_changed};

static const void *CLAP_ABI lbs_get_extension(const clap_plugin_t *p, const char *id) {
   (void)p;
   if (!strcmp(id, CLAP_EXT_AUDIO_PORTS)) return &lbs_audio_ports;
   if (!strcmp(id, CLAP_EXT_TRACK_INFO)) return &lbs_track_info;
   if (!strcmp(id, CLAP_EXT_TRACK_INFO_COMPAT)) return &lbs_track_info;
   return NULL;
}
static void CLAP_ABI lbs_main_thread(const clap_plugin_t *p) {
   (void)p;
}

static const char *lbs_features[] = {CLAP_PLUGIN_FEATURE_AUDIO_EFFECT, CLAP_PLUGIN_FEATURE_UTILITY, NULL};
static const clap_plugin_descriptor_t lbs_desc = {
    .clap_version = CLAP_VERSION_INIT,
    .id = "software.wail.send",
    .name = "WAIL Send",
    .vendor = "WAIL",
    .url = "https://github.com/nicholasgasior/wail",
    .version = "0.1.0",
    .description = "Publish this track as a Link Audio channel (ADR-0007).",
    .features = lbs_features,
};

static const clap_plugin_t *CLAP_ABI lbs_create(const clap_plugin_factory_t *f, const clap_host_t *host, const char *id) {
   (void)f;
   if (strcmp(id, lbs_desc.id) != 0) return NULL;
   lb_send *self = calloc(1, sizeof(lb_send));
   if (!self) return NULL;
   self->host = host;
   self->plugin.desc = &lbs_desc;
   self->plugin.plugin_data = self;
   self->plugin.init = lbs_init;
   self->plugin.destroy = lbs_destroy;
   self->plugin.activate = lbs_activate;
   self->plugin.deactivate = lbs_deactivate;
   self->plugin.start_processing = lbs_start;
   self->plugin.stop_processing = lbs_stop;
   self->plugin.reset = lbs_reset;
   self->plugin.process = lbs_process;
   self->plugin.get_extension = lbs_get_extension;
   self->plugin.on_main_thread = lbs_main_thread;
   return &self->plugin;
}

static uint32_t CLAP_ABI lbs_factory_count(const clap_plugin_factory_t *f) {
   (void)f;
   return 1;
}
static const clap_plugin_descriptor_t *CLAP_ABI lbs_factory_desc(const clap_plugin_factory_t *f, uint32_t idx) {
   (void)f;
   return idx == 0 ? &lbs_desc : NULL;
}
static const clap_plugin_factory_t lbs_factory = {lbs_factory_count, lbs_factory_desc, lbs_create};

static bool CLAP_ABI entry_init(const char *path) {
   (void)path;
   return true;
}
static void CLAP_ABI entry_deinit(void) {}
static const void *CLAP_ABI entry_get_factory(const char *id) {
   return strcmp(id, CLAP_PLUGIN_FACTORY_ID) == 0 ? &lbs_factory : NULL;
}

CLAP_EXPORT const clap_plugin_entry_t clap_entry = {
    .clap_version = CLAP_VERSION_INIT,
    .init = entry_init,
    .deinit = entry_deinit,
    .get_factory = entry_get_factory,
};
