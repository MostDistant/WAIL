// Link Bridge spike (ADR-0007, gate 1): proves the Link SDK links into a
// .clap bundle and a plugin-hosted peer joins the LAN Link session. On
// activate it creates and enables a Link peer; a ~1Hz heartbeat logs peer
// count and session tempo to /tmp/linkbridge-spike.log. Success = peer count
// climbs when another Link peer (WAIL app, Live, Bitwig Link) is on the LAN
// and the session tempo matches.
//
// Dev spike, not a product plugin. Full vtable (Bitwig null-derefs empty
// entries — see transport_probe.c).

#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "clap/clap.h"
#include "linkbridge_link.h"

#define SPIKE_LOG "/tmp/linkbridge-spike.log"

typedef struct {
   clap_plugin_t      plugin;
   const clap_host_t *host;
   FILE              *log;
   lb_link           *link;
   uint64_t           last_peers;
   double             last_tempo;
   int                blocks;
} spike;

static void spike_log(spike *self, const char *fmt, ...) {
   if (!self->log) return;
   va_list ap;
   va_start(ap, fmt);
   vfprintf(self->log, fmt, ap);
   va_end(ap);
   fputc('\n', self->log);
   fflush(self->log);
}

static clap_process_status CLAP_ABI spike_process(const clap_plugin_t *plugin, const clap_process_t *p) {
   spike *self = plugin->plugin_data;
   (void)p;
   self->blocks++;
   uint64_t peers = lb_num_peers(self->link);
   double tempo = lb_tempo(self->link);
   if (peers != self->last_peers || tempo != self->last_tempo || self->blocks % 200 == 0) {
      spike_log(self, "peers=%llu tempo=%.3f", (unsigned long long)peers, tempo);
      self->last_peers = peers;
      self->last_tempo = tempo;
   }
   return CLAP_PROCESS_CONTINUE;
}

static bool CLAP_ABI spike_init(const clap_plugin_t *plugin) {
   (void)plugin;
   return true;
}
static void CLAP_ABI spike_destroy(const clap_plugin_t *plugin) {
   spike *self = plugin->plugin_data;
   free(self);
}
static bool CLAP_ABI spike_activate(const clap_plugin_t *plugin, double sr, uint32_t minf, uint32_t maxf) {
   (void)minf;
   (void)maxf;
   spike *self = plugin->plugin_data;
   self->log = fopen(SPIKE_LOG, "a");
   self->link = lb_create(120.0);
   lb_enable(self->link, true);
   spike_log(self, "=== spike activated: host=\"%s %s\" sr=%.0f — Link peer enabled ===",
             self->host && self->host->name ? self->host->name : "?",
             self->host && self->host->version ? self->host->version : "?", sr);
   self->last_peers = ~0ull;
   return true;
}
static void CLAP_ABI spike_deactivate(const clap_plugin_t *plugin) {
   spike *self = plugin->plugin_data;
   if (self->link) {
      spike_log(self, "=== spike deactivated (peers was %llu) ===",
                (unsigned long long)lb_num_peers(self->link));
      lb_enable(self->link, false);
      lb_destroy(self->link);
      self->link = NULL;
   }
   if (self->log) {
      fclose(self->log);
      self->log = NULL;
   }
}
static bool CLAP_ABI spike_start(const clap_plugin_t *p) {
   (void)p;
   return true;
}
static void CLAP_ABI spike_stop(const clap_plugin_t *p) {
   (void)p;
}
static void CLAP_ABI spike_reset(const clap_plugin_t *p) {
   (void)p;
}
static const void *CLAP_ABI spike_get_extension(const clap_plugin_t *p, const char *id) {
   (void)p;
   (void)id;
   return NULL;
}
static void CLAP_ABI spike_main_thread(const clap_plugin_t *p) {
   (void)p;
}

static const char *spike_features[] = {CLAP_PLUGIN_FEATURE_AUDIO_EFFECT, CLAP_PLUGIN_FEATURE_UTILITY, NULL};
static const clap_plugin_descriptor_t spike_desc = {
    .clap_version = CLAP_VERSION_INIT,
    .id = "software.linkbridge.spike",
    .name = "Link Bridge Spike",
    .vendor = "Link Bridge",
    .url = "https://github.com/nicholasgasior/wail",
    .version = "0.1.0",
    .description = "Dev spike (ADR-0007 gate 1): Link SDK inside a CLAP plugin — joins the session and logs peers/tempo.",
    .features = spike_features,
};

static const clap_plugin_t *CLAP_ABI spike_create(const clap_plugin_factory_t *f, const clap_host_t *host, const char *id) {
   (void)f;
   if (strcmp(id, spike_desc.id) != 0) return NULL;
   spike *self = calloc(1, sizeof(spike));
   if (!self) return NULL;
   self->host = host;
   self->plugin.desc = &spike_desc;
   self->plugin.plugin_data = self;
   self->plugin.init = spike_init;
   self->plugin.destroy = spike_destroy;
   self->plugin.activate = spike_activate;
   self->plugin.deactivate = spike_deactivate;
   self->plugin.start_processing = spike_start;
   self->plugin.stop_processing = spike_stop;
   self->plugin.reset = spike_reset;
   self->plugin.process = spike_process;
   self->plugin.get_extension = spike_get_extension;
   self->plugin.on_main_thread = spike_main_thread;
   return &self->plugin;
}

static uint32_t CLAP_ABI spike_factory_count(const clap_plugin_factory_t *f) {
   (void)f;
   return 1;
}
static const clap_plugin_descriptor_t *CLAP_ABI spike_factory_desc(const clap_plugin_factory_t *f, uint32_t idx) {
   (void)f;
   return idx == 0 ? &spike_desc : NULL;
}
static const clap_plugin_factory_t spike_factory = {spike_factory_count, spike_factory_desc, spike_create};

static bool CLAP_ABI entry_init(const char *path) {
   (void)path;
   return true;
}
static void CLAP_ABI entry_deinit(void) {}
static const void *CLAP_ABI entry_get_factory(const char *id) {
   return strcmp(id, CLAP_PLUGIN_FACTORY_ID) == 0 ? &spike_factory : NULL;
}

CLAP_EXPORT const clap_plugin_entry_t clap_entry = {
    .clap_version = CLAP_VERSION_INIT,
    .init = entry_init,
    .deinit = entry_deinit,
    .get_factory = entry_get_factory,
};
