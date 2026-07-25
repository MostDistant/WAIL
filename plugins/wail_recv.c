// WAIL Recv — a thin CLAP plugin that receives remote WAIL streams as raw PCM from
// the WAIL app over loopback TCP (ADR-0005) and plays each on its own stereo output
// port. No codec, no networking beyond local IPC: the app does all the decode /
// interval / playout work and hands this already-paced int16 PCM.
//
// Threading (ADR-0002 discipline): process() only drains lock-free SPSC rings +
// writes output — never a syscall, lock, or allocation. A dedicated IPC thread owns
// the socket and fills the rings. Port renaming happens on the main thread via
// on_main_thread (CLAP requires rescan there).
#include <math.h>
#include <stdatomic.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>

#include "clap/clap.h"
#include "wail_ipc.h"

#define RECV_SLOTS 16
#define RECV_RING_FRAMES 32768 // power of two; ~0.68s stereo jitter buffer @ 48k
#define RECV_CHANNELS 2
#define RECV_ANCHORS 512      // power of two; > max in-flight chunks (0.68s / 5ms = 136)
// Stamps below this are a legacy app's interval index, not a timestamp: the
// app stamps chunks with machine-monotonic µs (wail_mono_micros), which is
// uptime-scaled — 1e9 µs ≈ 16.7 min of uptime. An old app always fails this
// threshold and gets FIFO playback; a new app on a freshly-booted machine
// degrades to FIFO for the first minutes, never to wrong alignment.
#define RECV_STAMP_MIN_US 1000000000LL

// recv_anchor maps one ring frame to the monotonic-µs instant it should play
// (the app's Link-beat→mono conversion). IPC thread pushes one per stamped
// chunk; the audio thread extrapolates the frame due *now* from the latest
// anchor — aligning playback to the shared timeline instead of arrival order
// (the Link Audio receiver contract: render buffer timing against local audio).
typedef struct {
   int64_t  micros; // mono-µs play-at of frame (fallback / stopped transport)
   double   beat;   // Link beat of frame (v2 chunks; 0 = absent → mono mode)
   uint32_t frame;
} recv_anchor;

// recv_ring is an SPSC float ring (IPC thread produces, audio thread consumes).
// head/tail are monotonic frame counters; index = frame % RECV_RING_FRAMES.
typedef struct {
   float            samples[RECV_RING_FRAMES * RECV_CHANNELS]; // interleaved stereo
   _Atomic uint32_t head;                                      // audio reads
   _Atomic uint32_t tail;                                      // IPC writes
} recv_ring;

typedef struct {
   _Atomic bool assigned; // audio thread: is this port's stream live?
   char         peer_id[64];
   uint16_t     stream_id;
   recv_ring    ring;

   // Alignment state (SPSC like the ring: IPC writes anchors, audio reads).
   recv_anchor      anchors[RECV_ANCHORS];
   _Atomic uint32_t ar_head; // audio consumes
   _Atomic uint32_t ar_tail; // IPC writes
   recv_anchor      last;    // audio-side copy of the latest consumed anchor
   bool             has_last;
   _Atomic uint32_t rate;    // chunk sample rate (engine rate; 0 → 48000)
   _Atomic uint64_t align_dropped; // frames skipped as late (alignment drops)
} recv_slot;

typedef struct {
   clap_plugin_t      plugin;
   const clap_host_t *host;
   double             sample_rate;

   recv_slot slots[RECV_SLOTS];

   // Port names: written by the IPC thread, read by get()/on_main_thread — all
   // non-audio threads, guarded by names_mu. "" means the default "WAIL N".
   wail_mutex   names_mu;
   char         name[RECV_SLOTS][CLAP_NAME_SIZE];
   _Atomic bool names_dirty;

   wail_thread  ipc_thread;
   _Atomic bool running;
   uint8_t      role_pref; // wire role to try: v2 first, toggled to v1 if the
                           // app drops a frameless v2 connection (old app)
} wail_recv;

// --- rings ---

static void recv_ring_pop(recv_ring *r, float *outL, float *outR, uint32_t n) {
   uint32_t head = atomic_load_explicit(&r->head, memory_order_relaxed);
   uint32_t tail = atomic_load_explicit(&r->tail, memory_order_acquire);
   uint32_t avail = tail - head;
   uint32_t take = avail < n ? avail : n;
   for (uint32_t i = 0; i < take; i++) {
      uint32_t idx = (head + i) % RECV_RING_FRAMES;
      if (outL) outL[i] = r->samples[2 * idx];
      if (outR) outR[i] = r->samples[2 * idx + 1];
   }
   for (uint32_t i = take; i < n; i++) { // underrun → silence
      if (outL) outL[i] = 0.0f;
      if (outR) outR[i] = 0.0f;
   }
   atomic_store_explicit(&r->head, head + take, memory_order_release);
}

static void recv_ring_push(recv_ring *r, const uint8_t *pcm, uint32_t nframes, int channels) {
   uint32_t tail = atomic_load_explicit(&r->tail, memory_order_relaxed);
   uint32_t head = atomic_load_explicit(&r->head, memory_order_acquire);
   uint32_t space = RECV_RING_FRAMES - (tail - head);
   uint32_t put = nframes < space ? nframes : space; // drop overflow
   for (uint32_t i = 0; i < put; i++) {
      uint32_t idx = (tail + i) % RECV_RING_FRAMES;
      int16_t l, rr;
      if (channels >= 2) {
         l = (int16_t)wail_get_u16(pcm + (size_t)(channels * i) * 2);
         rr = (int16_t)wail_get_u16(pcm + (size_t)(channels * i + 1) * 2);
      } else {
         l = rr = (int16_t)wail_get_u16(pcm + (size_t)i * 2);
      }
      r->samples[2 * idx] = (float)l / 32768.0f;
      r->samples[2 * idx + 1] = (float)rr / 32768.0f;
   }
   atomic_store_explicit(&r->tail, tail + put, memory_order_release);
}

// --- IPC-thread slot + name management ---

// find_or_assign returns the slot index for (peer_id, stream_id), assigning a free
// slot if needed. Returns -1 if all slots are in use. IPC-thread only.
static int find_or_assign(wail_recv *self, const char *pid, uint16_t sid) {
   int freeslot = -1;
   for (int i = 0; i < RECV_SLOTS; i++) {
      recv_slot *s = &self->slots[i];
      if (atomic_load_explicit(&s->assigned, memory_order_acquire)) {
         if (s->stream_id == sid && strcmp(s->peer_id, pid) == 0) return i;
      } else if (freeslot < 0) {
         freeslot = i;
      }
   }
   if (freeslot < 0) return -1;
   recv_slot *s = &self->slots[freeslot];
   snprintf(s->peer_id, sizeof(s->peer_id), "%s", pid);
   s->stream_id = sid;
   atomic_store_explicit(&s->ring.head, 0, memory_order_relaxed);
   atomic_store_explicit(&s->ring.tail, 0, memory_order_relaxed);
   atomic_store_explicit(&s->assigned, true, memory_order_release);
   return freeslot;
}

static void set_slot_name(wail_recv *self, int slot, const char *name) {
   wail_mutex_lock(&self->names_mu);
   snprintf(self->name[slot], CLAP_NAME_SIZE, "%s", name);
   wail_mutex_unlock(&self->names_mu);
   atomic_store_explicit(&self->names_dirty, true, memory_order_release);
   if (self->host && self->host->request_callback) self->host->request_callback(self->host);
}

static void handle_remote_pcm(wail_recv *self, const uint8_t *p, uint32_t len, bool v2) {
   size_t off = 1;
   if (off >= len) return;
   uint32_t pidLen = p[off++];
   if (off + pidLen + 15 > len) return;
   char pid[64];
   uint32_t cp = pidLen < sizeof(pid) - 1 ? pidLen : sizeof(pid) - 1;
   memcpy(pid, p + off, cp);
   pid[cp] = '\0';
   off += pidLen;
   uint16_t sid = wail_get_u16(p + off);
   off += 2;
   int channels = p[off++];
   uint32_t rate = wail_get_u32(p + off);
   off += 4;
   int64_t micros = (int64_t)wail_get_u64(p + off);
   off += 8;
   double beat = 0;
   if (v2) {
      if (off + 8 > len) return;
      uint64_t bits = wail_get_u64(p + off);
      memcpy(&beat, &bits, 8);
      off += 8;
   }
   if (channels < 1) return;
   uint32_t nbytes = len - (uint32_t)off;
   uint32_t nsamples = nbytes / 2;
   uint32_t nframes = nsamples / (uint32_t)channels;
   int slot = find_or_assign(self, pid, sid);
   if (slot < 0) return; // pool exhausted
   recv_slot *s = &self->slots[slot];
   // Anchor first, then the data: a block that runs between the two sees a
   // stamp with no audio yet and pads (correct), never plays the chunk early
   // as stamp-less FIFO (the reverse order's race). The ring tail's release
   // store then publishes the data; the anchor's frameStart is valid either
   // way because the IPC thread is the only tail writer.
   uint32_t frameStart = atomic_load_explicit(&s->ring.tail, memory_order_relaxed);
   atomic_store_explicit(&s->rate, rate, memory_order_relaxed);
   if (micros >= RECV_STAMP_MIN_US) {
      uint32_t at = atomic_load_explicit(&s->ar_tail, memory_order_relaxed);
      uint32_t ah = atomic_load_explicit(&s->ar_head, memory_order_acquire);
      if (at - ah == RECV_ANCHORS) // defensive: drop the oldest (stale anyway)
         atomic_store_explicit(&s->ar_head, ah + 1, memory_order_release);
      s->anchors[at % RECV_ANCHORS].micros = micros;
      s->anchors[at % RECV_ANCHORS].beat = beat;
      s->anchors[at % RECV_ANCHORS].frame = frameStart;
      atomic_store_explicit(&s->ar_tail, at + 1, memory_order_release);
   }
   recv_ring_push(&s->ring, p + off, nframes, channels);
}

static void handle_stream_name(wail_recv *self, const uint8_t *p, uint32_t len) {
   size_t off = 1;
   if (off >= len) return;
   uint32_t pidLen = p[off++];
   if (off + pidLen + 4 > len) return;
   char pid[64];
   uint32_t cp = pidLen < sizeof(pid) - 1 ? pidLen : sizeof(pid) - 1;
   memcpy(pid, p + off, cp);
   pid[cp] = '\0';
   off += pidLen;
   uint16_t sid = wail_get_u16(p + off);
   off += 2;
   uint32_t nameLen = wail_get_u16(p + off);
   off += 2;
   if (off + nameLen > len) return;
   char name[CLAP_NAME_SIZE];
   uint32_t cn = nameLen < sizeof(name) - 1 ? nameLen : sizeof(name) - 1;
   memcpy(name, p + off, cn);
   name[cn] = '\0';
   int slot = find_or_assign(self, pid, sid); // pre-labels the port before audio
   if (slot < 0) return;
   set_slot_name(self, slot, name);
}

static void handle_stream_gone(wail_recv *self, const uint8_t *p, uint32_t len) {
   size_t off = 1;
   if (off >= len) return;
   uint32_t pidLen = p[off++];
   if (off + pidLen + 2 > len) return;
   char pid[64];
   uint32_t cp = pidLen < sizeof(pid) - 1 ? pidLen : sizeof(pid) - 1;
   memcpy(pid, p + off, cp);
   pid[cp] = '\0';
   off += pidLen;
   uint16_t sid = wail_get_u16(p + off);
   for (int i = 0; i < RECV_SLOTS; i++) {
      recv_slot *s = &self->slots[i];
      if (atomic_load_explicit(&s->assigned, memory_order_acquire) &&
          s->stream_id == sid && strcmp(s->peer_id, pid) == 0) {
         atomic_store_explicit(&s->assigned, false, memory_order_release);
         set_slot_name(self, i, ""); // revert the port to its default label
         return;
      }
   }
}

static void dispatch_frame(wail_recv *self, const uint8_t *p, uint32_t len) {
   if (len == 0) return;
   switch (p[0]) {
   case WAIL_TAG_REMOTEPCM:
      handle_remote_pcm(self, p, len, false);
      break;
   case WAIL_TAG_REMOTEPCM2:
      handle_remote_pcm(self, p, len, true);
      break;
   case WAIL_TAG_STREAMNAME:
      handle_stream_name(self, p, len);
      break;
   case WAIL_TAG_STREAMGONE:
      handle_stream_gone(self, p, len);
      break;
   default:
      break;
   }
}

static void *recv_ipc_thread(void *arg) {
   wail_recv *self = arg;
   char host[128];
   int port;
   wail_ipc_resolve(host, sizeof(host), &port);
   wail_sock sock = WAIL_INVALID_SOCK;
   uint8_t *rbuf = NULL;
   size_t rlen = 0, rcap = 0;
   uint8_t chunk[8192];
   int idleTicks = 0;
   uint64_t lastReported = 0;
   bool gotFrame = false;

   while (atomic_load_explicit(&self->running, memory_order_acquire)) {
      if (sock == WAIL_INVALID_SOCK) {
         sock = wail_sock_connect(host, port);
         if (sock == WAIL_INVALID_SOCK) {
            wail_sleep_ms(500);
            continue;
         }
         uint8_t role = self->role_pref;
         if (wail_sock_write_all(sock, &role, 1) != 0) {
            wail_sock_close(sock);
            sock = WAIL_INVALID_SOCK;
            continue;
         }
         gotFrame = false;
         rlen = 0; // reset the reassembly buffer for the new connection
         // Bounded recv timeout so an idle-but-open connection doesn't park this
         // thread forever — deactivate() sets running=false and joins us.
         wail_sock_set_recv_timeout_ms(sock, 250);
      }
#ifdef _WIN32
      int n = recv(sock, (char *)chunk, (int)sizeof(chunk), 0);
#else
      ssize_t n = recv(sock, chunk, sizeof(chunk), 0);
#endif
      if (n < 0 && wail_sock_timed_out()) {
         // ~1s idle tick: report cumulative alignment drops so the app can
         // surface stamp-mode health (DecodeMetrics on the Go side).
         if (++idleTicks >= 4) {
            idleTicks = 0;
            uint64_t total = 0;
            for (int i = 0; i < RECV_SLOTS; i++)
               total += atomic_load_explicit(&self->slots[i].align_dropped, memory_order_relaxed);
            if (total != lastReported) {
               lastReported = total;
               uint8_t m[13], *mp = m;
               size_t mo = 0;
               wail_put_u32(mp, &mo, 9);
               mp[mo++] = WAIL_TAG_METRICS;
               wail_put_u64(mp, &mo, total);
               if (wail_sock_write_all(sock, m, (int)mo) != 0) {
                  wail_sock_close(sock);
                  sock = WAIL_INVALID_SOCK;
               }
            }
         }
         continue; // idle tick: re-check running, keep the connection
      }
      if (n <= 0) {
         wail_sock_close(sock);
         sock = WAIL_INVALID_SOCK;
         // A v2 connection dropped before any frame arrived means the app
         // rejected the unknown role byte — fall back to v1 (and back if a
         // future app update speaks v2 again after a downgrade stuck).
         if (!gotFrame) self->role_pref = self->role_pref == WAIL_IPC_ROLE_RECV_V2 ? WAIL_IPC_ROLE_RECV : WAIL_IPC_ROLE_RECV_V2;
         continue;
      }
      gotFrame = true;
      if (rlen + (size_t)n > rcap) {
         size_t ncap = rcap ? rcap * 2 : 16384;
         while (ncap < rlen + (size_t)n) ncap *= 2;
         uint8_t *nb = realloc(rbuf, ncap);
         if (!nb) break;
         rbuf = nb;
         rcap = ncap;
      }
      memcpy(rbuf + rlen, chunk, (size_t)n);
      rlen += (size_t)n;

      size_t pos = 0;
      while (rlen - pos >= 4) {
         uint32_t flen = wail_get_u32(rbuf + pos);
         if (flen > (16u << 20)) { // framing violation → drop the connection
            wail_sock_close(sock);
            sock = WAIL_INVALID_SOCK;
            rlen = 0;
            break;
         }
         if (rlen - pos < 4 + (size_t)flen) break; // need more bytes
         dispatch_frame(self, rbuf + pos + 4, flen);
         pos += 4 + flen;
      }
      if (pos > 0 && pos <= rlen) {
         memmove(rbuf, rbuf + pos, rlen - pos);
         rlen -= pos;
      }
   }
   if (sock != WAIL_INVALID_SOCK) wail_sock_close(sock);
   free(rbuf);
   return NULL;
}

// --- audio ---

// recv_align adjusts one slot's ring head against the stamped timeline and
// returns how many leading output frames must be silence this block. err>0
// (head ahead of the frame due now — we're early, e.g. delivery lead): pad
// that many frames, decoupling delivery lead from playback offset. err<0
// (frames due in the past): skip them — late audio is dropped, not played
// late. Anchor math is wrap-safe via uint32 frame counters (24h wrap at 48k).
// usePhase selects transport-phase alignment (host transport rolling with a
// beats timeline): the target is derived from the chunk's Link beat and the
// block's song position — the host's own transport→sample mapping absorbs the
// output pipeline latency, making placement sample-accurate relative to the
// DAW grid. Otherwise the mono-clock target is used (constant output-path
// offset, documented in tradeoffs.md).
static uint32_t recv_align(recv_slot *s, uint32_t n, int64_t nowUs,
                           bool usePhase, double blockBeat0, double tempo) {
   uint32_t ah = atomic_load_explicit(&s->ar_head, memory_order_relaxed);
   uint32_t at = atomic_load_explicit(&s->ar_tail, memory_order_acquire);
   uint32_t head = atomic_load_explicit(&s->ring.head, memory_order_relaxed);

   // Consume anchors the head has fully passed (keep the one in force).
   while (at - ah > 1 && (int32_t)(s->anchors[(ah + 1) % RECV_ANCHORS].frame - head) <= 0) {
      s->last = s->anchors[ah % RECV_ANCHORS];
      s->has_last = true;
      ah++;
   }
   atomic_store_explicit(&s->ar_head, ah, memory_order_release);

   // Pick the extrapolation anchor: newest stamp not in the future, else the
   // oldest pending (backward extrapolation pads a not-yet-due chunk), else
   // the last consumed one (keeps continuity across anchor-less stretches).
   recv_anchor a;
   bool have = false;
   for (uint32_t i = at; i > ah; i--) {
      if (s->anchors[(i - 1) % RECV_ANCHORS].micros <= nowUs) {
         a = s->anchors[(i - 1) % RECV_ANCHORS];
         have = true;
         break;
      }
   }
   if (!have && ah != at) {
      a = s->anchors[ah % RECV_ANCHORS];
      have = true;
   }
   if (!have) {
      if (!s->has_last) return 0; // no stamp ever: FIFO
      a = s->last;
   }

   uint32_t rate = atomic_load_explicit(&s->rate, memory_order_relaxed);
   if (rate == 0) rate = 48000;
   int32_t err;
   if (usePhase && a.beat != 0) {
      double spb = (double)rate * 60.0 / tempo; // samples per beat
      // Minimal signed phase residue anchor-vs-block in [-0.5, 0.5) beats:
      // φ>0 → the chunk head is φ beats in the future (pad); φ<0 → late (skip).
      double phi = a.beat - blockBeat0;
      phi -= floor(phi + 0.5);
      double targetD = (double)a.frame - phi * spb;
      // Snap by whole beats to the position nearest the mono-µs target (beat
      // numbering differs from song position by an unknown constant; the µs
      // stamp's error is ms, the beat ambiguity is spb/2 — hundreds of ms —
      // so the µs target always disambiguates, including the exact ±0.5 edge).
      int64_t targetMono = (int64_t)a.frame + (nowUs - a.micros) * (int64_t)rate / 1000000;
      targetD += floor(((double)targetMono - targetD) / spb + 0.5) * spb;
      // No clamping to 0: a future chunk's target is legitimately negative
      // (head hasn't reached it — that's the pad case), and the uint32 wrap
      // arithmetic expresses that correctly as a positive err.
      err = (int32_t)(head - (uint32_t)(int64_t)targetD);
   } else {
      int64_t target = (int64_t)a.frame + (nowUs - a.micros) * (int64_t)rate / 1000000;
      err = (int32_t)(head - (uint32_t)target);
   }
   // Deadband: |err| below this plays straight. Cadence jitter between the
   // host audio clock and the mono clock sits here (ppm-level drift ≈ sub-
   // frame per block); correcting it would click every block, and a sub-ms
   // wander is inaudible. Only genuine offset (delivery lead, late chunks)
   // crosses the band, and corrections return to the edge, not to zero.
   const int32_t band = 32; // ~0.67ms at 48k
   if (err > band) {
      uint32_t pad = (uint32_t)(err - band);
      return pad < n ? pad : n;
   }
   if (err < -band) {
      uint32_t tail = atomic_load_explicit(&s->ring.tail, memory_order_acquire);
      uint32_t avail = tail - head;
      uint32_t skip = (uint32_t)(-err - band);
      if (skip > avail) skip = avail;
      if (skip > 0) {
         atomic_store_explicit(&s->ring.head, head + skip, memory_order_release);
         atomic_fetch_add_explicit(&s->align_dropped, skip, memory_order_relaxed);
      }
   }
   return 0;
}

static clap_process_status CLAP_ABI recv_process(const clap_plugin_t *plugin, const clap_process_t *p) {
   wail_recv *self = plugin->plugin_data;
   uint32_t n = p->frames_count;
   int64_t nowUs = wail_mono_micros(); // one clock read per block (vDSO)
   // Transport-phase alignment is available when the host is rolling with a
   // beats timeline and a tempo.
   bool usePhase = false;
   double blockBeat0 = 0, tempo = 120;
   const clap_event_transport_t *tr = p->transport;
   if (tr && (tr->flags & CLAP_TRANSPORT_IS_PLAYING) &&
       (tr->flags & CLAP_TRANSPORT_HAS_BEATS_TIMELINE) &&
       (tr->flags & CLAP_TRANSPORT_HAS_TEMPO) && tr->tempo > 0) {
      usePhase = true;
      // clap_beattime is fixed-point beats × 2^31, NOT a double.
      blockBeat0 = (double)tr->song_pos_beats / (double)CLAP_BEATTIME_FACTOR;
      tempo = tr->tempo;
   }
   for (uint32_t port = 0; port < p->audio_outputs_count && port < RECV_SLOTS; port++) {
      clap_audio_buffer_t *out = &p->audio_outputs[port];
      if (!out->data32) continue;
      float *outL = out->data32[0];
      float *outR = out->channel_count > 1 ? out->data32[1] : NULL;
      recv_slot *s = &self->slots[port];
      if (atomic_load_explicit(&s->assigned, memory_order_acquire)) {
         uint32_t pad = recv_align(s, n, nowUs, usePhase, blockBeat0, tempo);
         uint32_t i;
         for (i = 0; i < pad; i++) {
            if (outL) outL[i] = 0.0f;
            if (outR) outR[i] = 0.0f;
         }
         recv_ring_pop(&s->ring, outL ? outL + pad : NULL, outR ? outR + pad : NULL, n - pad);
      } else {
         for (uint32_t i = 0; i < n; i++) {
            if (outL) outL[i] = 0.0f;
            if (outR) outR[i] = 0.0f;
         }
      }
   }
   return CLAP_PROCESS_CONTINUE;
}

// --- lifecycle ---

static bool CLAP_ABI recv_init(const clap_plugin_t *plugin) {
   (void)plugin;
   return true;
}
static void CLAP_ABI recv_destroy(const clap_plugin_t *plugin) {
   wail_recv *self = plugin->plugin_data;
   wail_mutex_destroy(&self->names_mu);
   free(self);
}
static bool CLAP_ABI recv_activate(const clap_plugin_t *plugin, double sr, uint32_t minf, uint32_t maxf) {
   (void)minf;
   (void)maxf;
   wail_recv *self = plugin->plugin_data;
   self->sample_rate = sr;
   atomic_store(&self->running, true);
   if (wail_thread_create(&self->ipc_thread, recv_ipc_thread, self) != 0) {
      atomic_store(&self->running, false);
      return false;
   }
   return true;
}
static void CLAP_ABI recv_deactivate(const clap_plugin_t *plugin) {
   wail_recv *self = plugin->plugin_data;
   if (atomic_exchange(&self->running, false)) wail_thread_join(self->ipc_thread);
}
static bool CLAP_ABI recv_start(const clap_plugin_t *p) {
   (void)p;
   return true;
}
static void CLAP_ABI recv_stop(const clap_plugin_t *p) { (void)p; }
static void CLAP_ABI recv_reset(const clap_plugin_t *p) { (void)p; }

static void CLAP_ABI recv_main_thread(const clap_plugin_t *plugin) {
   wail_recv *self = plugin->plugin_data;
   if (!atomic_exchange(&self->names_dirty, false)) return;
   const clap_host_audio_ports_t *hap = self->host->get_extension(self->host, CLAP_EXT_AUDIO_PORTS);
   if (hap && hap->rescan && hap->is_rescan_flag_supported &&
       hap->is_rescan_flag_supported(self->host, CLAP_AUDIO_PORTS_RESCAN_NAMES))
      hap->rescan(self->host, CLAP_AUDIO_PORTS_RESCAN_NAMES);
}

// --- audio-ports extension: 1 stereo input (ignored) + RECV_SLOTS stereo outputs ---

static uint32_t CLAP_ABI recv_ap_count(const clap_plugin_t *p, bool is_input) {
   (void)p;
   return is_input ? 1 : RECV_SLOTS;
}
static bool CLAP_ABI recv_ap_get(const clap_plugin_t *plugin, uint32_t idx, bool is_input, clap_audio_port_info_t *info) {
   wail_recv *self = plugin->plugin_data;
   if (is_input) {
      if (idx != 0) return false;
      info->id = 0;
      snprintf(info->name, sizeof(info->name), "Input");
      info->flags = CLAP_AUDIO_PORT_IS_MAIN;
      info->channel_count = RECV_CHANNELS;
      info->port_type = CLAP_PORT_STEREO;
      info->in_place_pair = CLAP_INVALID_ID;
      return true;
   }
   if (idx >= RECV_SLOTS) return false;
   info->id = idx;
   wail_mutex_lock(&self->names_mu);
   if (self->name[idx][0])
      snprintf(info->name, sizeof(info->name), "%s", self->name[idx]);
   else
      snprintf(info->name, sizeof(info->name), "WAIL %u", idx + 1);
   wail_mutex_unlock(&self->names_mu);
   info->flags = idx == 0 ? CLAP_AUDIO_PORT_IS_MAIN : 0;
   info->channel_count = RECV_CHANNELS;
   info->port_type = CLAP_PORT_STEREO;
   info->in_place_pair = CLAP_INVALID_ID;
   return true;
}
static const clap_plugin_audio_ports_t recv_audio_ports = {recv_ap_count, recv_ap_get};

// --- state extension ---
// Recv has no parameters today, but it must still implement CLAP_EXT_STATE:
// hosts like Bitwig refuse to save projects containing a plugin that "does not
// support saving its state". The blob is a version marker only.

#define RECV_STATE_MAGIC 0x57414C52u   // 'WALR'
#define RECV_STATE_VERSION 1u

static bool CLAP_ABI recv_state_save(const clap_plugin_t *plugin, const clap_ostream_t *stream) {
   (void)plugin;
   uint8_t buf[8];
   size_t  off = 0;
   wail_put_u32(buf, &off, RECV_STATE_MAGIC);
   wail_put_u32(buf, &off, RECV_STATE_VERSION);
   return wail_stream_write_all(stream, buf, off) != 0;
}

static bool CLAP_ABI recv_state_load(const clap_plugin_t *plugin, const clap_istream_t *stream) {
   (void)plugin;
   uint8_t buf[8];
   if (!wail_stream_read_all(stream, buf, sizeof(buf))) return false;
   if (wail_get_u32(buf) != RECV_STATE_MAGIC) return false;
   if (wail_get_u32(buf + 4) != RECV_STATE_VERSION) return false;
   return true;
}
static const clap_plugin_state_t recv_state = {recv_state_save, recv_state_load};

static const void *CLAP_ABI recv_get_extension(const clap_plugin_t *p, const char *id) {
   (void)p;
   if (!strcmp(id, CLAP_EXT_AUDIO_PORTS)) return &recv_audio_ports;
   if (!strcmp(id, CLAP_EXT_STATE)) return &recv_state;
   return NULL;
}

// --- descriptor / factory / entry ---

static const char *recv_features[] = {CLAP_PLUGIN_FEATURE_AUDIO_EFFECT, CLAP_PLUGIN_FEATURE_UTILITY, NULL};
static const clap_plugin_descriptor_t recv_desc = {
    .clap_version = CLAP_VERSION_INIT,
    .id = "software.wail.recv",
    .name = "WAIL Recv",
    .vendor = "WAIL",
    .url = "https://github.com/nicholasgasior/wail",
    .version = "0.1.0",
    .description = "Play remote WAIL streams into your DAW (thin PCM bridge from the WAIL app).",
    .features = recv_features,
};

static const clap_plugin_t *CLAP_ABI recv_create(const clap_plugin_factory_t *f, const clap_host_t *host, const char *id) {
   (void)f;
   if (strcmp(id, recv_desc.id) != 0) return NULL;
   wail_recv *self = calloc(1, sizeof(wail_recv));
   if (!self) return NULL;
   self->host = host;
   self->role_pref = WAIL_IPC_ROLE_RECV_V2; // old apps rejected → toggle to v1
   wail_mutex_init(&self->names_mu);
   self->plugin.desc = &recv_desc;
   self->plugin.plugin_data = self;
   self->plugin.init = recv_init;
   self->plugin.destroy = recv_destroy;
   self->plugin.activate = recv_activate;
   self->plugin.deactivate = recv_deactivate;
   self->plugin.start_processing = recv_start;
   self->plugin.stop_processing = recv_stop;
   self->plugin.reset = recv_reset;
   self->plugin.process = recv_process;
   self->plugin.get_extension = recv_get_extension;
   self->plugin.on_main_thread = recv_main_thread;
   return &self->plugin;
}

static uint32_t CLAP_ABI recv_factory_count(const clap_plugin_factory_t *f) {
   (void)f;
   return 1;
}
static const clap_plugin_descriptor_t *CLAP_ABI recv_factory_desc(const clap_plugin_factory_t *f, uint32_t idx) {
   (void)f;
   return idx == 0 ? &recv_desc : NULL;
}
static const clap_plugin_factory_t recv_factory = {recv_factory_count, recv_factory_desc, recv_create};

static bool CLAP_ABI entry_init(const char *path) {
   (void)path;
   return true;
}
static void CLAP_ABI entry_deinit(void) {}
static const void *CLAP_ABI entry_get_factory(const char *id) {
   return strcmp(id, CLAP_PLUGIN_FACTORY_ID) == 0 ? &recv_factory : NULL;
}

CLAP_EXPORT const clap_plugin_entry_t clap_entry = {
    .clap_version = CLAP_VERSION_INIT,
    .init = entry_init,
    .deinit = entry_deinit,
    .get_factory = entry_get_factory,
};
