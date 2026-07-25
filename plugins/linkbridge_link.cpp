// linkbridge_link.cpp — Link SDK compilation unit + C API impl for the Link
// Bridge plugins (ADR-0007). Mirrors the Go app's abllink/wrap.cpp: pulls in
// the vendored abl_link C wrapper (which instantiates the header-only SDK) and
// exposes the small C surface linkbridge_link.h declares.
//
// Windows/MinGW gets the same accommodations as the app's cgo build (NOMINMAX,
// winsock2 before windows.h); the `interface` param collision in Link's
// Channels.hpp is handled at build time like the app does (see DEVELOPMENT.md).
#if defined(_WIN32)
#define NOMINMAX
#include <winsock2.h>
#include <windows.h>
#endif

#include "abl_link.cpp"
#include "linkbridge_link.h"

#include <atomic>
#include <cmath>
#include <cstring>
#include <vector>

struct lb_link {
  abl_link h;
};
struct lb_state {
  abl_link_session_state s;
};

lb_link *lb_create(double tempo) {
  lb_link *l = new lb_link{};
  l->h = abl_link_create(tempo);
  return l;
}

void lb_destroy(lb_link *l) {
  if (!l) return;
  abl_link_destroy(l->h);
  delete l;
}

void lb_enable(lb_link *l, bool on) { abl_link_enable(l->h, on); }

void lb_enable_audio(lb_link *l, bool on) { abl_link_audio_enable_link_audio(l->h, on); }

uint64_t lb_num_peers(lb_link *l) { return abl_link_num_peers(l->h); }

double lb_tempo(lb_link *l) {
  abl_link_session_state ss = abl_link_create_session_state();
  abl_link_capture_app_session_state(l->h, ss);
  double t = abl_link_tempo(ss);
  abl_link_destroy_session_state(ss);
  return t;
}

lb_state *lb_capture(lb_link *l) {
  lb_state *s = new lb_state{};
  s->s = abl_link_create_session_state();
  abl_link_capture_audio_session_state(l->h, s->s);
  return s;
}

void lb_release(lb_state *s) {
  if (!s) return;
  abl_link_destroy_session_state(s->s);
  delete s;
}

double lb_beat_at_time(lb_state *s, int64_t micros, double quantum) {
  return abl_link_beat_at_time(s->s, micros, quantum);
}

int64_t lb_time_at_beat(lb_state *s, double beat, double quantum) {
  return abl_link_time_at_beat(s->s, beat, quantum);
}

int64_t lb_clock_micros(lb_link *l) { return abl_link_clock_micros(l->h); }

// --- sink ---

struct lb_sink {
  abl_link_audio_sink h;
};

lb_sink *lb_sink_create(lb_link *l, const char *name, size_t max_samples) {
  lb_sink *s = new lb_sink{};
  s->h = abl_link_audio_sink_create(l->h, name, max_samples);
  return s;
}

void lb_sink_destroy(lb_sink *s) {
  if (!s) return;
  abl_link_audio_sink_destroy(s->h);
  delete s;
}

void lb_sink_set_name(lb_sink *s, const char *name) {
  abl_link_audio_sink_set_name(s->h, name);
}

static int16_t lb_f2i16(float x) {
  if (x > 1.0f) x = 1.0f;
  if (x < -1.0f) x = -1.0f;
  long v = lrintf(x * 32767.0f);
  if (v > 32767) v = 32767;
  if (v < -32768) v = -32768;
  return (int16_t)v;
}

bool lb_sink_commit(lb_sink *s, lb_state *st, double beats_at_begin, double quantum,
                    const float *l, const float *r, size_t num_frames, uint32_t sample_rate) {
  abl_link_audio_sink_buffer_handle h = abl_link_audio_sink_retain_buffer(s->h);
  if (!abl_link_audio_sink_buffer_is_valid(&h)) {
    abl_link_audio_sink_buffer_release(&h);
    return false;
  }
  size_t nf = num_frames;
  if (nf * 2 > h.max_num_samples) nf = h.max_num_samples / 2;
  for (size_t i = 0; i < nf; i++) {
    float lv = l ? l[i] : 0.0f;
    float rv = r ? r[i] : lv;
    h.samples[2 * i] = lb_f2i16(lv);
    h.samples[2 * i + 1] = lb_f2i16(rv);
  }
  return abl_link_audio_sink_buffer_commit(&h, st->s, beats_at_begin, quantum, nf, 2, sample_rate);
}

// --- channels + source ---

size_t lb_channels(lb_link *l, lb_channel_info *out, size_t max) {
  if (max > LB_MAX_CHANNELS) max = LB_MAX_CHANNELS;
  abl_link_audio_channel_list list = abl_link_audio_get_channels(l->h);
  size_t n = 0;
  for (size_t i = 0; i < list.count && n < max; i++) {
    const abl_link_audio_channel &c = list.channels[i];
    uint64_t id;
    memcpy(&id, c.id.bytes, 8);
    out[n].id_u64 = id;
    snprintf(out[n].name, sizeof(out[n].name), "%s", c.name ? c.name : "");
    snprintf(out[n].peer_name, sizeof(out[n].peer_name), "%s", c.peer_name ? c.peer_name : "");
    n++;
  }
  abl_link_audio_free_channel_list(list);
  return n;
}

namespace {

// SPSC ring of received buffers: the SDK callback (Link-managed thread)
// produces, lb_source_pop consumes. Data ring of frames + a parallel chunk
// ring with per-buffer info, exactly the ADR-0002 capture-ring shape.
constexpr uint32_t kRingFrames = 1 << 17; // ~1.36s stereo @48k
constexpr uint32_t kRingChunks = 512;

struct lb_chunk {
  uint32_t frame_start;
  size_t num_frames;
  size_t num_channels;
  uint32_t sample_rate;
  uint64_t count;
  double session_beat_time;
  double tempo;
  abl_link_audio_session_id session_id; // begin-beats mapping checks it vs ours
};

} // namespace

struct lb_source {
  abl_link_audio_source h;
  abl_link link;
  std::atomic<uint32_t> data_head{0}, data_tail{0}; // frame counters
  std::atomic<uint32_t> chunk_head{0}, chunk_tail{0};
  std::vector<int16_t> data = std::vector<int16_t>(kRingFrames * 2);
  lb_chunk chunks[kRingChunks];
  std::vector<int16_t> pop_buf;
  std::atomic<uint64_t> dropped{0};
};

static void lb_source_cb(const abl_link_audio_source_buffer *buf, void *context) {
  lb_source *s = static_cast<lb_source *>(context);
  uint32_t tail = s->data_tail.load(std::memory_order_relaxed);
  uint32_t head = s->data_head.load(std::memory_order_acquire);
  uint32_t space = kRingFrames - (tail - head);
  size_t nf = buf->info.num_frames;
  if (nf > space) {
    s->dropped.fetch_add(nf, std::memory_order_relaxed);
    return;
  }
  uint32_t ct = s->chunk_tail.load(std::memory_order_relaxed);
  uint32_t ch = s->chunk_head.load(std::memory_order_acquire);
  if (ct - ch == kRingChunks) {
    s->dropped.fetch_add(nf, std::memory_order_relaxed);
    return;
  }
  for (size_t i = 0; i < nf; i++) {
    uint32_t idx = (tail + i) % kRingFrames;
    int16_t lv, rv;
    if (buf->info.num_channels > 1) {
      lv = buf->samples[2 * i];
      rv = buf->samples[2 * i + 1];
    } else {
      lv = rv = buf->samples[i];
    }
    s->data[2 * idx] = lv;
    s->data[2 * idx + 1] = rv;
  }
  lb_chunk &c = s->chunks[ct % kRingChunks];
  c.frame_start = tail;
  c.num_frames = nf;
  c.num_channels = buf->info.num_channels;
  c.sample_rate = buf->info.sample_rate;
  c.count = buf->info.count;
  c.session_beat_time = buf->info.session_beat_time;
  c.tempo = buf->info.tempo;
  c.session_id = buf->info.session_id;
  s->data_tail.store(tail + (uint32_t)nf, std::memory_order_release);
  s->chunk_tail.store(ct + 1, std::memory_order_release);
}

lb_source *lb_source_create(lb_link *l, uint64_t channel_id_u64) {
  lb_source *s = new lb_source{};
  s->link = l->h;
  abl_link_audio_channel_id id;
  memcpy(id.bytes, &channel_id_u64, 8);
  s->h = abl_link_audio_source_create(l->h, id, lb_source_cb, s);
  return s;
}

void lb_source_destroy(lb_source *s) {
  if (!s) return;
  abl_link_audio_source_destroy(s->h);
  delete s;
}

bool lb_source_pop(lb_source *s, lb_source_buffer *out, double quantum) {
  uint32_t ch = s->chunk_head.load(std::memory_order_relaxed);
  uint32_t ct = s->chunk_tail.load(std::memory_order_acquire);
  if (ch == ct) return false;
  const lb_chunk &c = s->chunks[ch % kRingChunks];
  // Map the buffer's begin onto our session state (cross-session buffers
  // don't map — begin_beat 0 tells the consumer to skip them).
  double beginBeat = 0;
  {
    abl_link_session_state ss = abl_link_create_session_state();
    abl_link_capture_audio_session_state(s->link, ss);
    abl_link_audio_source_buffer_info info;
    info.num_channels = c.num_channels;
    info.num_frames = c.num_frames;
    info.sample_rate = c.sample_rate;
    info.count = c.count;
    info.session_beat_time = c.session_beat_time;
    info.tempo = c.tempo;
    info.session_id = c.session_id;
    abl_link_audio_source_buffer_info_begin_beats(&info, ss, quantum, &beginBeat);
    abl_link_destroy_session_state(ss);
  }
  s->pop_buf.resize(c.num_frames * 2);
  for (size_t i = 0; i < c.num_frames; i++) {
    uint32_t idx = (c.frame_start + i) % kRingFrames;
    s->pop_buf[2 * i] = s->data[2 * idx];
    s->pop_buf[2 * i + 1] = s->data[2 * idx + 1];
  }
  s->chunk_head.store(ch + 1, std::memory_order_release);
  s->data_head.store(c.frame_start + (uint32_t)c.num_frames, std::memory_order_release);
  out->samples = s->pop_buf.data();
  out->num_frames = c.num_frames;
  out->num_channels = c.num_channels;
  out->sample_rate = c.sample_rate;
  out->count = c.count;
  out->session_beat_time = c.session_beat_time;
  out->tempo = c.tempo;
  out->begin_beat = beginBeat;
  return true;
}
