// Transport Probe — a diagnostic CLAP plugin that logs what the host provides
// in clap_process.transport: presence, flags, song_pos_beats (fixed-point,
// decoded), tempo, and steady_time. Exists to answer, per host (Live, Bitwig):
// "does this host populate song_pos_beats for CLAP plugins?" (WAIL issue #444).
//
// Not a product plugin: dev diagnostic only. Writes append to
// /tmp/clap-transport-probe.log on activate, on any transport-state change,
// and as a 1Hz heartbeat while processing. 1Hz fprintf on the audio thread is
// a realtime sin we accept for a dev tool — it must survive host crashes.

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "clap/clap.h"

#define PROBE_LOG "/tmp/clap-transport-probe.log"

typedef struct {
   clap_plugin_t      plugin;
   const clap_host_t *host;
   FILE              *log;
   // last-reported state, for change detection
   uint32_t last_flags;
   bool     have_last;
   bool     last_null;
   double   last_pos;   // beats
   double   last_tempo;
   int      blocks;
} probe;

static void probe_report(probe *self, const clap_process_t *p, const char *why) {
   if (!self->log) return;
   const clap_event_transport_t *tr = p->transport;
   if (!tr) {
      if (!self->have_last || !self->last_null) {
         fprintf(self->log, "[%s] transport = NULL (steady_time=%lld)\n", why,
                 (long long)p->steady_time);
         fflush(self->log);
         self->last_null = true;
         self->have_last = true;
      }
      return;
   }
   double pos = (double)tr->song_pos_beats / (double)CLAP_BEATTIME_FACTOR;
   double tempo = tr->tempo;
   bool changed = !self->have_last || self->last_null || tr->flags != self->last_flags ||
                  pos != self->last_pos || tempo != self->last_tempo;
   self->last_null = false;
   if (changed || self->blocks % 100 == 0) {
      fprintf(self->log,
              "[%s] flags=0x%02x playing=%d beatsTimeline=%d tempo=%d song_pos_beats=%.4f "
              "tempo_bpm=%.3f steady_time=%lld\n",
              why, tr->flags, !!(tr->flags & CLAP_TRANSPORT_IS_PLAYING),
              !!(tr->flags & CLAP_TRANSPORT_HAS_BEATS_TIMELINE),
              !!(tr->flags & CLAP_TRANSPORT_HAS_TEMPO), pos, tempo,
              (long long)p->steady_time);
      fflush(self->log);
      self->last_flags = tr->flags;
      self->last_pos = pos;
      self->last_tempo = tempo;
      self->have_last = true;
   }
}

static clap_process_status CLAP_ABI probe_process(const clap_plugin_t *plugin, const clap_process_t *p) {
   probe *self = plugin->plugin_data;
   self->blocks++;
   probe_report(self, p, "process");
   // Passthrough: copy inputs to outputs (probe is inserted on a track).
   for (uint32_t port = 0; port < p->audio_outputs_count && port < p->audio_inputs_count; port++) {
      const clap_audio_buffer_t *in = &p->audio_inputs[port];
      clap_audio_buffer_t *out = &p->audio_outputs[port];
      for (uint32_t ch = 0; ch < out->channel_count && ch < in->channel_count; ch++)
         if (out->data32[ch] && in->data32[ch] && out->data32[ch] != in->data32[ch])
            memcpy(out->data32[ch], in->data32[ch], p->frames_count * sizeof(float));
   }
   return CLAP_PROCESS_CONTINUE;
}

static bool CLAP_ABI probe_init(const clap_plugin_t *plugin) {
   (void)plugin;
   return true;
}
static void CLAP_ABI probe_destroy(const clap_plugin_t *plugin) {
   probe *self = plugin->plugin_data;
   free(self);
}
static bool CLAP_ABI probe_activate(const clap_plugin_t *plugin, double sr, uint32_t minf, uint32_t maxf) {
   (void)minf;
   (void)maxf;
   probe *self = plugin->plugin_data;
   self->log = fopen(PROBE_LOG, "a");
   if (self->log) {
      fprintf(self->log, "=== probe activated: host=\"%s %s\" sr=%.0f ===\n",
              self->host && self->host->name ? self->host->name : "?",
              self->host && self->host->version ? self->host->version : "?", sr);
      fflush(self->log);
   }
   self->have_last = false;
   self->blocks = 0;
   return true;
}
static void CLAP_ABI probe_deactivate(const clap_plugin_t *plugin) {
   probe *self = plugin->plugin_data;
   if (self->log) {
      fprintf(self->log, "=== probe deactivated ===\n");
      fclose(self->log);
      self->log = NULL;
   }
}
static bool CLAP_ABI probe_start(const clap_plugin_t *p) {
   (void)p;
   return true;
}
static void CLAP_ABI probe_stop(const clap_plugin_t *p) {
   (void)p;
}
static void CLAP_ABI probe_reset(const clap_plugin_t *p) {
   (void)p;
}
// Hosts (Bitwig) call these unconditionally during scan/load — never NULL.
static const void *CLAP_ABI probe_get_extension(const clap_plugin_t *p, const char *id) {
   (void)p;
   (void)id;
   return NULL;
}
static void CLAP_ABI probe_main_thread(const clap_plugin_t *p) {
   (void)p;
}

static const char *probe_features[] = {CLAP_PLUGIN_FEATURE_AUDIO_EFFECT, CLAP_PLUGIN_FEATURE_UTILITY, NULL};
static const clap_plugin_descriptor_t probe_desc = {
    .clap_version = CLAP_VERSION_INIT,
    .id = "software.wail.transport-probe",
    .name = "WAIL Transport Probe",
    .vendor = "WAIL",
    .url = "https://github.com/nicholasgasior/wail",
    .version = "0.1.0",
    .description = "Dev diagnostic: logs host transport fields (song_pos_beats, tempo, flags).",
    .features = probe_features,
};

static const clap_plugin_t *CLAP_ABI probe_create(const clap_plugin_factory_t *f, const clap_host_t *host, const char *id) {
   (void)f;
   if (strcmp(id, probe_desc.id) != 0) return NULL;
   probe *self = calloc(1, sizeof(probe));
   if (!self) return NULL;
   self->host = host;
   self->plugin.desc = &probe_desc;
   self->plugin.plugin_data = self;
   self->plugin.init = probe_init;
   self->plugin.destroy = probe_destroy;
   self->plugin.activate = probe_activate;
   self->plugin.deactivate = probe_deactivate;
   self->plugin.start_processing = probe_start;
   self->plugin.stop_processing = probe_stop;
   self->plugin.reset = probe_reset;
   self->plugin.process = probe_process;
   self->plugin.get_extension = probe_get_extension;
   self->plugin.on_main_thread = probe_main_thread;
   return &self->plugin;
}

static uint32_t CLAP_ABI probe_factory_count(const clap_plugin_factory_t *f) {
   (void)f;
   return 1;
}
static const clap_plugin_descriptor_t *CLAP_ABI probe_factory_desc(const clap_plugin_factory_t *f, uint32_t idx) {
   (void)f;
   return idx == 0 ? &probe_desc : NULL;
}
static const clap_plugin_factory_t probe_factory = {probe_factory_count, probe_factory_desc, probe_create};

static bool CLAP_ABI entry_init(const char *path) {
   (void)path;
   return true;
}
static void CLAP_ABI entry_deinit(void) {}
static const void *CLAP_ABI entry_get_factory(const char *id) {
   return strcmp(id, CLAP_PLUGIN_FACTORY_ID) == 0 ? &probe_factory : NULL;
}

CLAP_EXPORT const clap_plugin_entry_t clap_entry = {
    .clap_version = CLAP_VERSION_INIT,
    .init = entry_init,
    .deinit = entry_deinit,
    .get_factory = entry_get_factory,
};
