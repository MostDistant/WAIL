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
