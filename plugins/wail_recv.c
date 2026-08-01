// WAIL Recv (ADR-0007) — a 16-port Link Audio receiver for WAIL room
// streams. Subscribes only to room-published channels (names carrying the
// "WAIL · " room prefix — remote streams "WAIL · {peer} · {stream}" and the
// metronome), one port per channel, auto-assigned and live-renamed. A peer
// joining the jam appears as a named sub-chain with no user action.
//
// Slot identity is the channel id, not its name: the app renames a room
// channel in place once the sender's stream name arrives (affinity — the id
// survives), so a name-keyed slot would take a second port for the renamed
// channel and never release the first (its id is still live, so the
// disappeared-channel reclaim never fires). Name matching remains only as a
// first-wins guard so two WAILs bridging one room don't double a port.
//
// Rendering is stamp-aligned: per-slot anchor ring (chunk start frame ↔ begin
// beat / play-at µs), pad when early, skip when late, 32-frame deadband. Two
// modes chosen per block: host
// transport rolling with a beats timeline → fractional-beat-phase alignment
// (sample-accurate to the DAW grid; the host's transport→sample mapping
// absorbs output latency); otherwise session-clock mono alignment (the
// plugin's own Link peer maps beats → µs — self-consistent domain, no bridge).
//
// Threading: process() owns slot rings/anchors entirely (source drain happens
// here). A manager thread does channel discovery + slot assign/free + port
// renames (none of that is realtime-safe or belongs on the audio thread).
// 48kHz-only: other host rates output silence and log loudly.

#include <math.h>
#include <stdarg.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "clap/clap.h"
#include "wail_link.h"
#include "wail_thread.h" // wail_thread / wail_mutex / wail_sleep_ms

// Names the release in the log; lb_module_stamp supplies which build of it is
// actually loaded (the version alone can't — the stale case is "same version,
// older build", which is every case during development).
#ifndef WAIL_PLUGIN_VERSION
#define WAIL_PLUGIN_VERSION "unknown"
#endif

#define LBR_SLOTS 16
#define LBR_RING_FRAMES 32768 // power of two; ~0.68s stereo @48k
#define LBR_ANCHORS 512
#define LBR_QUANTUM 4.0
// One prefix, one meaning: it both selects room-published channels and is
// stripped for the port label. A looser filter here than the app's publish
// prefix would subscribe raw LAN channels that merely start with "WAIL" — a
// WAIL Send track named "WAIL Bass" would come back as a receive port.
#define LBR_ROOM_PREFIX "WAIL · "
// Length of the literal above, compile-time — the filter runs it per channel
// per poll in three places.
#define LBR_ROOM_PREFIX_LEN (sizeof(LBR_ROOM_PREFIX) - 1)
#define LBR_GONE_POLLS 6 // ~3s of discovery misses → slot freed

typedef struct {
   int64_t  micros; // session-clock µs at which frame should play (mono mode)
   double   beat;   // session begin beat (phase mode)
   uint32_t frame;
} lbr_anchor;

typedef struct {
   int16_t  samples[LBR_RING_FRAMES * 2]; // interleaved stereo
   uint32_t head, tail;                   // process()-owned (single thread)
} lbr_ring;

typedef struct {
   _Atomic bool assigned; // manager writes, process reads
   uint64_t   channel_id;
   char       chan_name[128]; // last-seen channel name (rename detection)
   lb_source *source;         // created before assigned=true; destroyed after clearing it

   // process()-owned render state:
   lbr_ring    ring;
   lbr_anchor  anchors[LBR_ANCHORS];
   uint32_t    ar_head, ar_tail;
   lbr_anchor  last;
   bool        has_last;
   uint32_t    rate;
   uint64_t    align_dropped;
   bool        logged_pop; // process(): first pop handed off already

   // First-pop details, filled by process() and written out by the manager —
   // logging from the audio thread is blocking I/O and is not allowed here.
   // Published by pop_pending; process() writes the fields before the release
   // store and never touches them again, so the manager reads them safely.
   _Atomic bool pop_pending;
   struct {
      size_t   frames, channels;
      uint32_t rate;
      double   begin_beat;
   } pop;
} lbr_slot;

typedef struct {
   clap_plugin_t      plugin;
   const clap_host_t *host;

   lb_link *link;
   bool     rate_ok;

   lbr_slot slots[LBR_SLOTS];

   // Port names: manager thread writes, get()/on_main_thread reads (all
   // non-audio threads), guarded by names_mu.
   wail_mutex   names_mu;
   char         name[LBR_SLOTS][CLAP_NAME_SIZE];
   _Atomic bool names_dirty;

   wail_thread  mgr_thread;
   _Atomic bool running;

   uint64_t snapshot_sig; // manager-thread only; last logged discovery+slot state

   FILE *log;
} lbr_recv;

static void lbr_log(lbr_recv *self, const char *fmt, ...) {
   if (!self->log) return;
   va_list ap;
   va_start(ap, fmt);
   vfprintf(self->log, fmt, ap);
   va_end(ap);
   fputc('\n', self->log);
   fflush(self->log);
}

// --- slot ring (process-thread only) ---

static void ring_push(lbr_slot *s, const int16_t *pcm, uint32_t nframes) {
   uint32_t space = LBR_RING_FRAMES - (s->ring.tail - s->ring.head);
   uint32_t put = nframes < space ? nframes : space; // drop overflow
   for (uint32_t i = 0; i < put; i++) {
      uint32_t idx = (s->ring.tail + i) % LBR_RING_FRAMES;
      s->ring.samples[2 * idx] = pcm[2 * i];
      s->ring.samples[2 * idx + 1] = pcm[2 * i + 1];
   }
   s->ring.tail += put;
}

static void ring_pop(lbr_slot *s, float *outL, float *outR, uint32_t n) {
   uint32_t avail = s->ring.tail - s->ring.head;
   uint32_t take = avail < n ? avail : n;
   for (uint32_t i = 0; i < take; i++) {
      uint32_t idx = (s->ring.head + i) % LBR_RING_FRAMES;
      if (outL) outL[i] = (float)s->ring.samples[2 * idx] / 32768.0f;
      if (outR) outR[i] = (float)s->ring.samples[2 * idx + 1] / 32768.0f;
   }
   for (uint32_t i = take; i < n; i++) { // underrun → silence
      if (outL) outL[i] = 0.0f;
      if (outR) outR[i] = 0.0f;
   }
   s->ring.head += take;
}

// --- drain: source (wrapper ring) → slot ring + anchors [process thread] ---

static void slot_drain(lbr_recv *self, lbr_slot *s) {
   lb_source_buffer b;
   lb_state *st = lb_capture(self->link);
   while (lb_source_pop(s->source, &b, LBR_QUANTUM)) {
      if (!s->logged_pop) {
         // Hand the first-pop details to the manager to write. This runs on
         // the audio thread, where lbr_log's buffered write + fflush is a
         // blocking disk I/O the threading contract here forbids outright —
         // and it shares its FILE with the manager, so it could block on
         // another thread's write to /tmp inside the process deadline.
         s->pop.frames = b.num_frames;
         s->pop.channels = b.num_channels;
         s->pop.rate = b.sample_rate;
         s->pop.begin_beat = b.begin_beat;
         atomic_store_explicit(&s->pop_pending, true, memory_order_release);
         s->logged_pop = true;
      }
      if (b.num_frames == 0 || b.begin_beat == 0) continue; // unmapped (cross-session) — skip
      uint32_t nf = (uint32_t)b.num_frames;
      if (nf * 2 > LBR_RING_FRAMES) nf = LBR_RING_FRAMES / 2;
      uint32_t frameStart = s->ring.tail;
      ring_push(s, b.samples, nf);
      // Anchor: begin beat → session-clock µs for the mono fallback.
      int64_t micros = lb_time_at_beat(st, b.begin_beat, LBR_QUANTUM);
      uint32_t at = s->ar_tail;
      if (at - s->ar_head == LBR_ANCHORS) s->ar_head++; // drop oldest (stale anyway)
      s->anchors[at % LBR_ANCHORS].micros = micros;
      s->anchors[at % LBR_ANCHORS].beat = b.begin_beat;
      s->anchors[at % LBR_ANCHORS].frame = frameStart;
      s->ar_tail = at + 1;
      s->rate = b.sample_rate ? b.sample_rate : 48000;
   }
   lb_release(st);
}

// --- align: pad early / skip late, deadband 32 frames [process thread] ---
// Pad early / skip late against the host transport: phase
// mode uses the chunk's session beat vs the host transport's song position;
// mono mode uses session-clock µs. Phase targets snap by whole beats to the
// mono target (µs error is ms; the ±0.5-beat ambiguity is hundreds of ms).

static uint32_t slot_align(lbr_recv *self, lbr_slot *s, uint32_t n, int64_t nowMicros,
                           bool usePhase, double blockBeat0, double tempo) {
   uint32_t ah = s->ar_head, at = s->ar_tail;
   uint32_t head = s->ring.head;

   while (at - ah > 1 && (int32_t)(s->anchors[(ah + 1) % LBR_ANCHORS].frame - head) <= 0) {
      s->last = s->anchors[ah % LBR_ANCHORS];
      s->has_last = true;
      ah++;
   }
   s->ar_head = ah;

   lbr_anchor a;
   bool have = false;
   for (uint32_t i = at; i > ah; i--) {
      if (s->anchors[(i - 1) % LBR_ANCHORS].micros <= nowMicros) {
         a = s->anchors[(i - 1) % LBR_ANCHORS];
         have = true;
         break;
      }
   }
   if (!have && ah != at) {
      a = s->anchors[ah % LBR_ANCHORS];
      have = true;
   }
   if (!have) {
      if (!s->has_last) return 0;
      a = s->last;
   }

   uint32_t rate = s->rate ? s->rate : 48000;
   int32_t err;
   if (usePhase && a.beat != 0) {
      double spb = (double)rate * 60.0 / tempo;
      double phi = a.beat - blockBeat0;
      phi -= floor(phi + 0.5);
      double targetD = (double)a.frame - phi * spb;
      int64_t targetMono = (int64_t)a.frame + (nowMicros - a.micros) * (int64_t)rate / 1000000;
      targetD += floor(((double)targetMono - targetD) / spb + 0.5) * spb;
      err = (int32_t)(head - (uint32_t)(int64_t)targetD);
   } else {
      int64_t target = (int64_t)a.frame + (nowMicros - a.micros) * (int64_t)rate / 1000000;
      err = (int32_t)(head - (uint32_t)target);
   }

   const int32_t band = 32; // ~0.67ms at 48k — jitter absorbs here, not as clicks
   if (err > band) {
      uint32_t pad = (uint32_t)(err - band);
      return pad < n ? pad : n;
   }
   if (err < -band) {
      uint32_t avail = s->ring.tail - head;
      uint32_t skip = (uint32_t)(-err - band);
      if (skip > avail) skip = avail;
      if (skip > 0) {
         s->ring.head += skip;
         s->align_dropped += skip;
      }
   }
   return 0;
}

// --- process ---

static clap_process_status CLAP_ABI lbr_process(const clap_plugin_t *plugin, const clap_process_t *p) {
   lbr_recv *self = plugin->plugin_data;
   uint32_t n = p->frames_count;

   bool usePhase = false;
   double blockBeat0 = 0, tempo = 120;
   const clap_event_transport_t *tr = p->transport;
   if (tr && (tr->flags & CLAP_TRANSPORT_IS_PLAYING) &&
       (tr->flags & CLAP_TRANSPORT_HAS_BEATS_TIMELINE) &&
       (tr->flags & CLAP_TRANSPORT_HAS_TEMPO) && tr->tempo > 0) {
      usePhase = true;
      blockBeat0 = (double)tr->song_pos_beats / (double)CLAP_BEATTIME_FACTOR; // fixed-point!
      tempo = tr->tempo;
   }

   int64_t nowMicros = self->rate_ok ? lb_clock_micros(self->link) : 0;

   for (uint32_t port = 0; port < p->audio_outputs_count && port < LBR_SLOTS; port++) {
      clap_audio_buffer_t *out = &p->audio_outputs[port];
      if (!out->data32) continue;
      float *outL = out->data32[0];
      float *outR = out->channel_count > 1 ? out->data32[1] : NULL;
      lbr_slot *s = &self->slots[port];
      if (!self->rate_ok || !atomic_load_explicit(&s->assigned, memory_order_acquire) || !s->source) {
         for (uint32_t i = 0; i < n; i++) {
            if (outL) outL[i] = 0.0f;
            if (outR) outR[i] = 0.0f;
         }
         continue;
      }
      slot_drain(self, s);
      uint32_t pad = slot_align(self, s, n, nowMicros, usePhase, blockBeat0, tempo);
      uint32_t i;
      for (i = 0; i < pad; i++) {
         if (outL) outL[i] = 0.0f;
         if (outR) outR[i] = 0.0f;
      }
      ring_pop(s, outL ? outL + pad : NULL, outR ? outR + pad : NULL, n - pad);
   }
   return CLAP_PROCESS_CONTINUE;
}

// --- manager thread: discovery, assign/free, names ---

static void set_port_name(lbr_recv *self, int slot, const char *name) {
   wail_mutex_lock(&self->names_mu);
   snprintf(self->name[slot], CLAP_NAME_SIZE, "%s", name);
   wail_mutex_unlock(&self->names_mu);
   atomic_store_explicit(&self->names_dirty, true, memory_order_release);
   if (self->host && self->host->request_callback) self->host->request_callback(self->host);
}

// display_name strips the room-published dot-prefix for the port label.
static const char *display_name(const char *chan, char *buf, size_t cap) {
   size_t p = LBR_ROOM_PREFIX_LEN;
   if (strncmp(chan, LBR_ROOM_PREFIX, p) == 0) {
      snprintf(buf, cap, "%s", chan + p);
      return buf;
   }
   snprintf(buf, cap, "%s", chan);
   return buf;
}

// Snapshot signature: FNV-1a per entry, summed. Summing is commutative, so a
// reordered discovery list is not mistaken for a change; slot entries mix in
// the index, so a slot's position does count.
static uint64_t lbr_entry_hash(uint64_t id, const char *name) {
   uint64_t h = 1469598103934665603ULL;
   const uint8_t *idb = (const uint8_t *)&id;
   for (size_t i = 0; i < sizeof id; i++) {
      h ^= idb[i];
      h *= 1099511628211ULL;
   }
   for (const char *p = name; *p; p++) {
      h ^= (uint8_t)*p;
      h *= 1099511628211ULL;
   }
   return h;
}

// log_snapshot records discovery and the slot table, but only when either has
// changed. Channel identity across a rename is the premise slot keying rests
// on, so those transitions must be readable from the log rather than inferred
// after the fact — while the steady state, at two polls a second, is pure
// repetition and would bury them.
static void log_snapshot(lbr_recv *self, const lb_channel_info *chans, size_t nch) {
   uint64_t sig = 0;
   for (size_t c = 0; c < nch; c++) {
      // Only channels this plugin could carry. Every other Link Audio channel
      // on the LAN — another app's tracks being renamed or armed — would
      // otherwise churn the signature and dump the table for something we
      // never act on.
      if (strncmp(chans[c].name, LBR_ROOM_PREFIX, LBR_ROOM_PREFIX_LEN) != 0) continue;
      sig += lbr_entry_hash(chans[c].id_u64, chans[c].name);
   }
   for (int i = 0; i < LBR_SLOTS; i++)
      if (atomic_load_explicit(&self->slots[i].assigned, memory_order_acquire))
         sig += lbr_entry_hash(self->slots[i].channel_id ^ (uint64_t)(i + 1),
                               self->slots[i].chan_name);
   if (sig == self->snapshot_sig) return;
   self->snapshot_sig = sig;

   for (size_t c = 0; c < nch; c++) {
      if (strncmp(chans[c].name, LBR_ROOM_PREFIX, LBR_ROOM_PREFIX_LEN) != 0) continue;
      lbr_log(self, "  chan id=%016llx name=\"%s\"", (unsigned long long)chans[c].id_u64,
              chans[c].name);
   }
   for (int i = 0; i < LBR_SLOTS; i++)
      if (atomic_load_explicit(&self->slots[i].assigned, memory_order_acquire))
         lbr_log(self, "  slot %d id=%016llx name=\"%s\"", i,
                 (unsigned long long)self->slots[i].channel_id, self->slots[i].chan_name);
}

// free_slot releases a slot's subscription and clears its port label. Ordering
// matters: clear assigned first so process() stops touching the source, then
// give any in-flight process() time to return before destroying it.
static void free_slot(lbr_recv *self, int i, int *miss, const char *why) {
   lbr_log(self, "%s: \"%s\" (slot %d freed)", why, self->slots[i].chan_name, i);
   atomic_store_explicit(&self->slots[i].assigned, false, memory_order_release);
   wail_sleep_ms(50);
   lb_source_destroy(self->slots[i].source);
   self->slots[i].source = NULL;
   self->slots[i].chan_name[0] = '\0';
   self->slots[i].channel_id = 0;
   miss[i] = 0;
   set_port_name(self, i, "");
}

static void *mgr_main(void *arg) {
   lbr_recv *self = arg;
   int miss[LBR_SLOTS] = {0};
   while (atomic_load_explicit(&self->running, memory_order_acquire)) {
      wail_sleep_ms(500);
      lb_channel_info chans[LB_MAX_CHANNELS];
      size_t nch = lb_channels(self->link, chans, LB_MAX_CHANNELS);

      // Write out any first-pop details process() handed over.
      for (int i = 0; i < LBR_SLOTS; i++) {
         lbr_slot *s = &self->slots[i];
         if (!atomic_load_explicit(&s->pop_pending, memory_order_acquire)) continue;
         atomic_store_explicit(&s->pop_pending, false, memory_order_relaxed);
         lbr_log(self, "first pop on port %d: frames=%zu ch=%zu rate=%u begin_beat=%.3f (0=unmapped)",
                 i + 1, s->pop.frames, s->pop.channels, s->pop.rate, s->pop.begin_beat);
      }

      // Free slots whose channel has disappeared for a while.
      for (int i = 0; i < LBR_SLOTS; i++) {
         if (!atomic_load_explicit(&self->slots[i].assigned, memory_order_acquire)) continue;
         bool seen = false;
         for (size_t c = 0; c < nch; c++)
            if (chans[c].id_u64 == self->slots[i].channel_id) seen = true;
         miss[i] = seen ? 0 : miss[i] + 1;
         if (miss[i] >= LBR_GONE_POLLS) free_slot(self, i, miss, "channel gone");
      }

      // Assign new room-published channels.
      for (size_t c = 0; c < nch; c++) {
         if (strncmp(chans[c].name, LBR_ROOM_PREFIX, LBR_ROOM_PREFIX_LEN) != 0) continue;

         // Already ours? Match on channel id first, so an in-place rename
         // relabels the port this channel already holds instead of taking a
         // second one. Checked across every slot before falling back to the
         // name guard, else two channels swapping names would strand a label.
         int owner = -1;
         for (int i = 0; i < LBR_SLOTS; i++) {
            if (atomic_load_explicit(&self->slots[i].assigned, memory_order_acquire) &&
                self->slots[i].channel_id == chans[c].id_u64) {
               owner = i;
               break;
            }
         }
         if (owner >= 0) {
            if (strcmp(self->slots[owner].chan_name, chans[c].name) != 0) {
               lbr_log(self, "renamed: \"%s\" → \"%s\" (port %d)", self->slots[owner].chan_name,
                       chans[c].name, owner + 1);
               snprintf(self->slots[owner].chan_name, sizeof(self->slots[owner].chan_name), "%s",
                        chans[c].name);
               char disp[128];
               set_port_name(self, owner, display_name(chans[c].name, disp, sizeof(disp)));
            }
            continue;
         }

         // Distinct channel under a name we already carry: first-wins, so two
         // WAILs bridging the same room don't double a port.
         bool dup_name = false;
         for (int i = 0; i < LBR_SLOTS; i++)
            if (atomic_load_explicit(&self->slots[i].assigned, memory_order_acquire) &&
                strcmp(self->slots[i].chan_name, chans[c].name) == 0)
               dup_name = true;
         if (dup_name) continue;

         for (int i = 0; i < LBR_SLOTS; i++) {
            if (atomic_load_explicit(&self->slots[i].assigned, memory_order_acquire)) continue;
            lbr_slot *s = &self->slots[i];
            memset(&s->ring, 0, sizeof(s->ring));
            s->ar_head = s->ar_tail = 0;
            s->has_last = false;
            s->rate = 0;
            s->align_dropped = 0;
            s->logged_pop = false;
            s->channel_id = chans[c].id_u64;
            snprintf(s->chan_name, sizeof(s->chan_name), "%s", chans[c].name);
            s->source = lb_source_create(self->link, chans[c].id_u64);
            if (!s->source) break;
            atomic_store_explicit(&s->assigned, true, memory_order_release);
            char disp[128];
            set_port_name(self, i, display_name(chans[c].name, disp, sizeof(disp)));
            lbr_log(self, "subscribed: \"%s\" → port %d", chans[c].name, i + 1);
            break;
         }
      }

      // First-wins by name, reconciled after the fact. Two WAILs bridging one
      // room rename their copies of a stream independently, so the second's
      // old name matches no slot for a poll or two and takes a port the guard
      // above would have refused. Converge by freeing the later duplicate.
      for (int i = 0; i < LBR_SLOTS; i++) {
         if (!atomic_load_explicit(&self->slots[i].assigned, memory_order_acquire)) continue;
         for (int j = i + 1; j < LBR_SLOTS; j++) {
            if (!atomic_load_explicit(&self->slots[j].assigned, memory_order_acquire)) continue;
            if (strcmp(self->slots[i].chan_name, self->slots[j].chan_name) == 0)
               free_slot(self, j, miss, "duplicate of a port we already carry");
         }
      }

      // After the poll's decisions, so the slot table logged is the outcome.
      log_snapshot(self, chans, nch);
   }
   return NULL;
}

// --- lifecycle ---

static bool CLAP_ABI lbr_init(const clap_plugin_t *plugin) {
   (void)plugin;
   return true;
}
static void CLAP_ABI lbr_destroy(const clap_plugin_t *plugin) {
   lbr_recv *self = plugin->plugin_data;
   free(self);
}
static bool CLAP_ABI lbr_activate(const clap_plugin_t *plugin, double sr, uint32_t minf, uint32_t maxf) {
   (void)minf;
   (void)maxf;
   lbr_recv *self = plugin->plugin_data;
   { char lp[512]; lb_temp_log_path("wail-recv.log", lp, sizeof(lp)); self->log = fopen(lp, "a"); }
   self->rate_ok = (sr == 48000.0);
   self->link = lb_create(120.0);
   lb_enable(self->link, true);
   lb_enable_audio(self->link, true);
   char stamp[64], modpath[512];
   lb_module_stamp(stamp, sizeof(stamp), modpath, sizeof(modpath));
   lbr_log(self, "=== recv activated: build %s (%s) host=\"%s %s\" sr=%.0f%s ===",
           WAIL_PLUGIN_VERSION, stamp,
           self->host && self->host->name ? self->host->name : "?",
           self->host && self->host->version ? self->host->version : "?", sr,
           self->rate_ok ? "" : " — NOT 48k: outputting silence");
   // Which copy the host actually loaded — installed bundle, build tree, or a
   // stale duplicate — is the other half of "is this the build I think it is".
   lbr_log(self, "    loaded from %s", modpath);
   // Fresh log file, so forget what the previous activation had recorded —
   // otherwise an unchanged channel set means the reopened log never states
   // the state it exists to record.
   self->snapshot_sig = 0;
   atomic_store(&self->running, true);
   if (wail_thread_create(&self->mgr_thread, mgr_main, self) != 0) {
      atomic_store(&self->running, false);
      return false;
   }
   return true;
}
static void CLAP_ABI lbr_deactivate(const clap_plugin_t *plugin) {
   lbr_recv *self = plugin->plugin_data;
   if (atomic_exchange(&self->running, false)) wail_thread_join(self->mgr_thread);
   for (int i = 0; i < LBR_SLOTS; i++) {
      if (self->slots[i].source) {
         lb_source_destroy(self->slots[i].source);
         self->slots[i].source = NULL;
      }
      // Release the slot too, not just its source: a reactivate re-runs
      // discovery, and a slot still marked assigned for a live channel id is
      // neither re-subscribed nor freed — it just renders silence forever.
      atomic_store_explicit(&self->slots[i].assigned, false, memory_order_release);
      self->slots[i].chan_name[0] = '\0';
      self->slots[i].channel_id = 0;
   }
   if (self->link) {
      lb_enable(self->link, false);
      lb_destroy(self->link);
      self->link = NULL;
   }
   if (self->log) {
      fprintf(self->log, "=== recv deactivated ===\n");
      fclose(self->log);
      self->log = NULL;
   }
}
static bool CLAP_ABI lbr_start(const clap_plugin_t *p) {
   (void)p;
   return true;
}
static void CLAP_ABI lbr_stop(const clap_plugin_t *p) {
   (void)p;
}
static void CLAP_ABI lbr_reset(const clap_plugin_t *p) {
   (void)p;
}
static void CLAP_ABI lbr_on_main_thread(const clap_plugin_t *plugin) {
   lbr_recv *self = plugin->plugin_data;
   if (!atomic_exchange(&self->names_dirty, false)) return;
   const clap_host_audio_ports_t *hap = self->host->get_extension(self->host, CLAP_EXT_AUDIO_PORTS);
   if (hap && hap->rescan && hap->is_rescan_flag_supported &&
       hap->is_rescan_flag_supported(self->host, CLAP_AUDIO_PORTS_RESCAN_NAMES))
      hap->rescan(self->host, CLAP_AUDIO_PORTS_RESCAN_NAMES);
}

// --- audio ports: 1 stereo input (ignored) + 16 stereo outputs ---

static uint32_t CLAP_ABI lbr_ap_count(const clap_plugin_t *p, bool is_input) {
   (void)p;
   return is_input ? 1 : LBR_SLOTS;
}
static bool CLAP_ABI lbr_ap_get(const clap_plugin_t *plugin, uint32_t idx, bool is_input, clap_audio_port_info_t *info) {
   lbr_recv *self = plugin->plugin_data;
   if (is_input) {
      if (idx != 0) return false;
      info->id = 0;
      snprintf(info->name, sizeof(info->name), "Input");
      info->flags = CLAP_AUDIO_PORT_IS_MAIN;
      info->channel_count = 2;
      info->port_type = CLAP_PORT_STEREO;
      info->in_place_pair = CLAP_INVALID_ID;
      return true;
   }
   if (idx >= LBR_SLOTS) return false;
   info->id = idx;
   wail_mutex_lock(&self->names_mu);
   if (self->name[idx][0])
      snprintf(info->name, sizeof(info->name), "%s", self->name[idx]);
   else
      snprintf(info->name, sizeof(info->name), "WAIL Recv %u", idx + 1);
   wail_mutex_unlock(&self->names_mu);
   info->flags = idx == 0 ? CLAP_AUDIO_PORT_IS_MAIN : 0;
   info->channel_count = 2;
   info->port_type = CLAP_PORT_STEREO;
   info->in_place_pair = CLAP_INVALID_ID;
   return true;
}
static const clap_plugin_audio_ports_t lbr_audio_ports = {lbr_ap_count, lbr_ap_get};

static const void *CLAP_ABI lbr_get_extension(const clap_plugin_t *p, const char *id) {
   (void)p;
   if (!strcmp(id, CLAP_EXT_AUDIO_PORTS)) return &lbr_audio_ports;
   return NULL;
}

static const char *lbr_features[] = {CLAP_PLUGIN_FEATURE_AUDIO_EFFECT, CLAP_PLUGIN_FEATURE_UTILITY, NULL};
static const clap_plugin_descriptor_t lbr_desc = {
    .clap_version = CLAP_VERSION_INIT,
    .id = "software.wail.recv",
    .name = "WAIL Receive",
    .vendor = "WAIL",
    .url = "https://github.com/nicholasgasior/wail",
    .version = "0.1.0",
    .description = "Hear WAIL room streams as named Link Audio sub-chains (ADR-0007).",
    .features = lbr_features,
};

static const clap_plugin_t *CLAP_ABI lbr_create(const clap_plugin_factory_t *f, const clap_host_t *host, const char *id) {
   (void)f;
   if (strcmp(id, lbr_desc.id) != 0) return NULL;
   lbr_recv *self = calloc(1, sizeof(lbr_recv));
   if (!self) return NULL;
   self->host = host;
   wail_mutex_init(&self->names_mu);
   self->plugin.desc = &lbr_desc;
   self->plugin.plugin_data = self;
   self->plugin.init = lbr_init;
   self->plugin.destroy = lbr_destroy;
   self->plugin.activate = lbr_activate;
   self->plugin.deactivate = lbr_deactivate;
   self->plugin.start_processing = lbr_start;
   self->plugin.stop_processing = lbr_stop;
   self->plugin.reset = lbr_reset;
   self->plugin.process = lbr_process;
   self->plugin.get_extension = lbr_get_extension;
   self->plugin.on_main_thread = lbr_on_main_thread;
   return &self->plugin;
}

static uint32_t CLAP_ABI lbr_factory_count(const clap_plugin_factory_t *f) {
   (void)f;
   return 1;
}
static const clap_plugin_descriptor_t *CLAP_ABI lbr_factory_desc(const clap_plugin_factory_t *f, uint32_t idx) {
   (void)f;
   return idx == 0 ? &lbr_desc : NULL;
}
static const clap_plugin_factory_t lbr_factory = {lbr_factory_count, lbr_factory_desc, lbr_create};

static bool CLAP_ABI entry_init(const char *path) {
   (void)path;
   return true;
}
static void CLAP_ABI entry_deinit(void) {}
static const void *CLAP_ABI entry_get_factory(const char *id) {
   return strcmp(id, CLAP_PLUGIN_FACTORY_ID) == 0 ? &lbr_factory : NULL;
}

CLAP_EXPORT const clap_plugin_entry_t clap_entry = {
    .clap_version = CLAP_VERSION_INIT,
    .init = entry_init,
    .deinit = entry_deinit,
    .get_factory = entry_get_factory,
};
