// WAIL Send — a thin CLAP plugin that captures its track's audio and streams it as
// raw PCM to the WAIL app over loopback TCP (ADR-0005). No codec, no networking
// beyond local IPC: the app does all Opus/WAIF/interval/relay work. Insert it on a
// track; it passes audio through and taps a copy to WAIL.
//
// Threading (ADR-0002 discipline): process() only does lock-free ring writes +
// memcpy — never a syscall, lock, or allocation. A dedicated IPC thread owns the
// socket and drains the ring.
#include <stdatomic.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>

#include "clap/clap.h"
#include "wail_ipc.h"

#define SEND_RING_SLOTS 64
#define SEND_CHANNELS 2

typedef struct {
   uint64_t begin_frame; // plugin frame counter at this block's first sample
   uint32_t nframes;
   uint8_t  playing;
   float   *pcm; // interleaved stereo, capacity max_frames*SEND_CHANNELS
} send_slot;

typedef struct {
   clap_plugin_t      plugin;
   const clap_host_t *host;
   double             sample_rate;
   uint32_t           max_frames;

   // SPSC ring: audio thread (tail) → IPC thread (head).
   send_slot        slots[SEND_RING_SLOTS];
   _Atomic uint32_t head;
   _Atomic uint32_t tail;
   _Atomic uint64_t dropped;

   uint64_t     frame_counter; // audio-thread owned
   _Atomic int  stream_index;  // from the "Stream Index" param; read by IPC thread

   wail_thread  ipc_thread;
   _Atomic bool running;
} wail_send;

// --- audio-thread capture ---

static void send_ring_push(wail_send *self, const float *inL, const float *inR, uint32_t n, uint8_t playing) {
   uint32_t tail = atomic_load_explicit(&self->tail, memory_order_relaxed);
   uint32_t head = atomic_load_explicit(&self->head, memory_order_acquire);
   if (tail - head >= SEND_RING_SLOTS) { // full — drop, stay RT-safe
      atomic_fetch_add_explicit(&self->dropped, 1, memory_order_relaxed);
      return;
   }
   send_slot *s = &self->slots[tail % SEND_RING_SLOTS];
   if (n > self->max_frames) n = self->max_frames;
   for (uint32_t i = 0; i < n; i++) {
      s->pcm[2 * i] = inL ? inL[i] : 0.0f;
      s->pcm[2 * i + 1] = inR ? inR[i] : 0.0f;
   }
   s->begin_frame = self->frame_counter;
   s->nframes = n;
   s->playing = playing;
   atomic_store_explicit(&self->tail, tail + 1, memory_order_release);
}

static clap_process_status CLAP_ABI send_process(const clap_plugin_t *plugin, const clap_process_t *p) {
   wail_send *self = plugin->plugin_data;
   uint32_t n = p->frames_count;
   uint8_t playing = (p->transport && (p->transport->flags & CLAP_TRANSPORT_IS_PLAYING)) ? 1 : 0;

   const float *inL = NULL, *inR = NULL;
   if (p->audio_inputs_count > 0 && p->audio_inputs[0].data32) {
      inL = p->audio_inputs[0].data32[0];
      inR = p->audio_inputs[0].channel_count > 1 ? p->audio_inputs[0].data32[1] : inL;
   }
   send_ring_push(self, inL, inR, n, playing);

   // Passthrough so inserting the plugin doesn't mute the track.
   if (p->audio_outputs_count > 0 && p->audio_outputs[0].data32) {
      float *outL = p->audio_outputs[0].data32[0];
      float *outR = p->audio_outputs[0].channel_count > 1 ? p->audio_outputs[0].data32[1] : NULL;
      for (uint32_t i = 0; i < n; i++) {
         if (outL) outL[i] = inL ? inL[i] : 0.0f;
         if (outR) outR[i] = inR ? inR[i] : 0.0f;
      }
   }
   self->frame_counter += n;

   if (p->in_events) {
      uint32_t ne = p->in_events->size(p->in_events);
      for (uint32_t i = 0; i < ne; i++) {
         const clap_event_header_t *h = p->in_events->get(p->in_events, i);
         if (h->space_id == CLAP_CORE_EVENT_SPACE_ID && h->type == CLAP_EVENT_PARAM_VALUE) {
            const clap_event_param_value_t *ev = (const clap_event_param_value_t *)h;
            if (ev->param_id == 0)
               atomic_store_explicit(&self->stream_index, (int)ev->value, memory_order_relaxed);
         }
      }
   }
   return CLAP_PROCESS_CONTINUE;
}

// --- IPC thread ---

static void *send_ipc_thread(void *arg) {
   wail_send *self = arg;
   char host[128];
   int port;
   wail_ipc_resolve(host, sizeof(host), &port);
   wail_sock sock = WAIL_INVALID_SOCK;
   uint8_t *payload = NULL;
   size_t payload_cap = 0;

   while (atomic_load_explicit(&self->running, memory_order_acquire)) {
      if (sock == WAIL_INVALID_SOCK) {
         sock = wail_sock_connect(host, port);
         if (sock == WAIL_INVALID_SOCK) {
            wail_sleep_ms(500);
            continue;
         }
         uint8_t hs[3];
         size_t off = 0;
         hs[off++] = WAIL_IPC_ROLE_SEND;
         wail_put_u16(hs, &off, (uint16_t)atomic_load_explicit(&self->stream_index, memory_order_relaxed));
         if (wail_sock_write_all(sock, hs, off) != 0) {
            wail_sock_close(sock);
            sock = WAIL_INVALID_SOCK;
            continue;
         }
      }
      uint32_t head = atomic_load_explicit(&self->head, memory_order_relaxed);
      uint32_t tail = atomic_load_explicit(&self->tail, memory_order_acquire);
      if (head == tail) {
         wail_sleep_ms(2);
         continue;
      }
      send_slot *s = &self->slots[head % SEND_RING_SLOTS];
      size_t need = 17 + (size_t)s->nframes * SEND_CHANNELS * sizeof(float);
      if (need > payload_cap) {
         uint8_t *np = realloc(payload, need);
         if (!np) break;
         payload = np;
         payload_cap = need;
      }
      size_t off = 0;
      payload[off++] = WAIL_TAG_RAWPCM;
      wail_put_u16(payload, &off, (uint16_t)atomic_load_explicit(&self->stream_index, memory_order_relaxed));
      payload[off++] = (uint8_t)(s->playing ? WAIL_RAW_FLAG_PLAYING : 0); // float32 payload
      payload[off++] = SEND_CHANNELS;
      wail_put_u32(payload, &off, (uint32_t)self->sample_rate);
      wail_put_u64(payload, &off, s->begin_frame);
      size_t bytes = (size_t)s->nframes * SEND_CHANNELS * sizeof(float);
      memcpy(payload + off, s->pcm, bytes);
      off += bytes;
      if (wail_send_frame(sock, payload, (uint32_t)off) != 0) {
         wail_sock_close(sock);
         sock = WAIL_INVALID_SOCK;
         continue; // reconnect; don't advance head — the block is re-sent
      }
      atomic_store_explicit(&self->head, head + 1, memory_order_release);
   }
   if (sock != WAIL_INVALID_SOCK) wail_sock_close(sock);
   free(payload);
   return NULL;
}

// --- plugin lifecycle ---

static bool CLAP_ABI send_init(const clap_plugin_t *plugin) {
   (void)plugin;
   return true;
}
static void CLAP_ABI send_destroy(const clap_plugin_t *plugin) { free(plugin->plugin_data); }

static bool CLAP_ABI send_activate(const clap_plugin_t *plugin, double sr, uint32_t minf, uint32_t maxf) {
   (void)minf;
   wail_send *self = plugin->plugin_data;
   self->sample_rate = sr;
   self->max_frames = maxf;
   for (int i = 0; i < SEND_RING_SLOTS; i++) {
      self->slots[i].pcm = calloc((size_t)maxf * SEND_CHANNELS, sizeof(float));
      if (!self->slots[i].pcm) {
         for (int j = 0; j < i; j++) {
            free(self->slots[j].pcm);
            self->slots[j].pcm = NULL;
         }
         return false;
      }
   }
   atomic_store(&self->head, 0);
   atomic_store(&self->tail, 0);
   self->frame_counter = 0;
   atomic_store(&self->running, true);
   if (wail_thread_create(&self->ipc_thread, send_ipc_thread, self) != 0) {
      atomic_store(&self->running, false);
      return false;
   }
   return true;
}

static void CLAP_ABI send_deactivate(const clap_plugin_t *plugin) {
   wail_send *self = plugin->plugin_data;
   if (atomic_exchange(&self->running, false)) wail_thread_join(self->ipc_thread);
   for (int i = 0; i < SEND_RING_SLOTS; i++) {
      free(self->slots[i].pcm);
      self->slots[i].pcm = NULL;
   }
}

static bool CLAP_ABI send_start(const clap_plugin_t *p) {
   (void)p;
   return true;
}
static void CLAP_ABI send_stop(const clap_plugin_t *p) { (void)p; }
static void CLAP_ABI send_reset(const clap_plugin_t *p) { (void)p; }
static void CLAP_ABI send_main_thread(const clap_plugin_t *p) { (void)p; }

// --- audio-ports extension: one stereo in, one stereo out ---

static uint32_t CLAP_ABI send_ap_count(const clap_plugin_t *p, bool is_input) {
   (void)p;
   (void)is_input;
   return 1;
}
static bool CLAP_ABI send_ap_get(const clap_plugin_t *p, uint32_t idx, bool is_input, clap_audio_port_info_t *info) {
   (void)p;
   if (idx != 0) return false;
   info->id = 0;
   snprintf(info->name, sizeof(info->name), "%s", is_input ? "Input" : "Output");
   info->flags = CLAP_AUDIO_PORT_IS_MAIN;
   info->channel_count = SEND_CHANNELS;
   info->port_type = CLAP_PORT_STEREO;
   info->in_place_pair = CLAP_INVALID_ID;
   return true;
}
static const clap_plugin_audio_ports_t send_audio_ports = {send_ap_count, send_ap_get};

// --- params extension: "Stream Index" (0..15) ---

static uint32_t CLAP_ABI send_pp_count(const clap_plugin_t *p) {
   (void)p;
   return 1;
}
static bool CLAP_ABI send_pp_info(const clap_plugin_t *p, uint32_t idx, clap_param_info_t *info) {
   (void)p;
   if (idx != 0) return false;
   memset(info, 0, sizeof(*info));
   info->id = 0;
   info->flags = CLAP_PARAM_IS_STEPPED | CLAP_PARAM_IS_AUTOMATABLE;
   snprintf(info->name, sizeof(info->name), "Stream Index");
   info->min_value = 0;
   info->max_value = 15;
   info->default_value = 0;
   return true;
}
static bool CLAP_ABI send_pp_value(const clap_plugin_t *p, clap_id id, double *out) {
   if (id != 0) return false;
   wail_send *self = p->plugin_data;
   *out = (double)atomic_load_explicit(&self->stream_index, memory_order_relaxed);
   return true;
}
static bool CLAP_ABI send_pp_v2t(const clap_plugin_t *p, clap_id id, double v, char *buf, uint32_t cap) {
   (void)p;
   if (id != 0) return false;
   snprintf(buf, cap, "%d", (int)v);
   return true;
}
static bool CLAP_ABI send_pp_t2v(const clap_plugin_t *p, clap_id id, const char *txt, double *out) {
   (void)p;
   if (id != 0) return false;
   *out = atof(txt);
   return true;
}
static void CLAP_ABI send_pp_flush(const clap_plugin_t *p, const clap_input_events_t *in, const clap_output_events_t *out) {
   (void)out;
   if (!in) return;
   wail_send *self = p->plugin_data;
   uint32_t ne = in->size(in);
   for (uint32_t i = 0; i < ne; i++) {
      const clap_event_header_t *h = in->get(in, i);
      if (h->space_id == CLAP_CORE_EVENT_SPACE_ID && h->type == CLAP_EVENT_PARAM_VALUE) {
         const clap_event_param_value_t *ev = (const clap_event_param_value_t *)h;
         if (ev->param_id == 0)
            atomic_store_explicit(&self->stream_index, (int)ev->value, memory_order_relaxed);
      }
   }
}
static const clap_plugin_params_t send_params = {
    send_pp_count, send_pp_info, send_pp_value, send_pp_v2t, send_pp_t2v, send_pp_flush};

static const void *CLAP_ABI send_get_extension(const clap_plugin_t *p, const char *id) {
   (void)p;
   if (!strcmp(id, CLAP_EXT_AUDIO_PORTS)) return &send_audio_ports;
   if (!strcmp(id, CLAP_EXT_PARAMS)) return &send_params;
   return NULL;
}

// --- descriptor / factory / entry ---

static const char *send_features[] = {CLAP_PLUGIN_FEATURE_AUDIO_EFFECT, CLAP_PLUGIN_FEATURE_UTILITY, NULL};
static const clap_plugin_descriptor_t send_desc = {
    .clap_version = CLAP_VERSION_INIT,
    .id = "software.wail.send",
    .name = "WAIL Send",
    .vendor = "WAIL",
    .url = "https://github.com/nicholasgasior/wail",
    .version = "0.1.0",
    .description = "Capture DAW audio into a WAIL room (thin PCM bridge to the WAIL app).",
    .features = send_features,
};

static const clap_plugin_t *CLAP_ABI send_create(const clap_plugin_factory_t *f, const clap_host_t *host, const char *id) {
   (void)f;
   if (strcmp(id, send_desc.id) != 0) return NULL;
   wail_send *self = calloc(1, sizeof(wail_send));
   if (!self) return NULL;
   self->host = host;
   atomic_store(&self->stream_index, 0);
   self->plugin.desc = &send_desc;
   self->plugin.plugin_data = self;
   self->plugin.init = send_init;
   self->plugin.destroy = send_destroy;
   self->plugin.activate = send_activate;
   self->plugin.deactivate = send_deactivate;
   self->plugin.start_processing = send_start;
   self->plugin.stop_processing = send_stop;
   self->plugin.reset = send_reset;
   self->plugin.process = send_process;
   self->plugin.get_extension = send_get_extension;
   self->plugin.on_main_thread = send_main_thread;
   return &self->plugin;
}

static uint32_t CLAP_ABI send_factory_count(const clap_plugin_factory_t *f) {
   (void)f;
   return 1;
}
static const clap_plugin_descriptor_t *CLAP_ABI send_factory_desc(const clap_plugin_factory_t *f, uint32_t idx) {
   (void)f;
   return idx == 0 ? &send_desc : NULL;
}
static const clap_plugin_factory_t send_factory = {send_factory_count, send_factory_desc, send_create};

static bool CLAP_ABI entry_init(const char *path) {
   (void)path;
   return true;
}
static void CLAP_ABI entry_deinit(void) {}
static const void *CLAP_ABI entry_get_factory(const char *id) {
   return strcmp(id, CLAP_PLUGIN_FACTORY_ID) == 0 ? &send_factory : NULL;
}

CLAP_EXPORT const clap_plugin_entry_t clap_entry = {
    .clap_version = CLAP_VERSION_INIT,
    .init = entry_init,
    .deinit = entry_deinit,
    .get_factory = entry_get_factory,
};
