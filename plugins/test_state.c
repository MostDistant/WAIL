// test_state — loads a built .clap bundle and exercises the clap.state
// extension: set a param → save → mutate → load → the param must be restored.
// Drives the plugin with a minimal fake host (no GUI, no audio thread). Usage:
//
//   test_state <path-to-plugin-binary> <plugin-id> [stream-index]
//
// <path-to-plugin-binary> is the actual shared object (CMake passes
// $<TARGET_FILE:...>, which handles the macOS bundle layout). The optional
// third argument selects the "Stream Index" roundtrip assertions (wail-send);
// without it only a bare save/load cycle is checked (wail-recv).
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "clap/clap.h"

#ifdef _WIN32
#include <windows.h>
static void *lib_open(const char *p) { return (void *)LoadLibraryA(p); }
static void *lib_sym(void *h, const char *n) { return (void *)GetProcAddress((HMODULE)h, n); }
#else
#include <dlfcn.h>
static void *lib_open(const char *p) { return dlopen(p, RTLD_NOW | RTLD_LOCAL); }
static void *lib_sym(void *h, const char *n) { return dlsym(h, n); }
#endif

#define CHECK(cond, msg)                                                                                               \
   do {                                                                                                                \
      if (!(cond)) {                                                                                                   \
         fprintf(stderr, "FAIL: %s (%s:%d)\n", msg, __FILE__, __LINE__);                                               \
         exit(1);                                                                                                      \
      }                                                                                                                \
   } while (0)

// --- memory streams ---

typedef struct {
   clap_ostream_t s;
   uint8_t        buf[4096];
   size_t         len;
} mem_out;

static int64_t CLAP_ABI mem_write(const clap_ostream_t *stream, const void *buffer, uint64_t size) {
   mem_out *m = (mem_out *)stream;
   if (m->len + size > sizeof(m->buf)) return -1;
   memcpy(m->buf + m->len, buffer, size);
   m->len += size;
   return (int64_t)size;
}

typedef struct {
   clap_istream_t s;
   uint8_t        buf[4096];
   size_t         len, pos;
} mem_in;

static int64_t CLAP_ABI mem_read(const clap_istream_t *stream, void *buffer, uint64_t size) {
   mem_in *m = (mem_in *)stream;
   size_t avail = m->len - m->pos;
   if (avail == 0) return 0; // EOF
   if (size > avail) size = avail;
   memcpy(buffer, m->buf + m->pos, size);
   m->pos += size;
   return (int64_t)size;
}

// --- one-shot input event list carrying a single PARAM_VALUE ---

typedef struct {
   clap_input_events_t        list;
   clap_event_param_value_t   ev;
} one_param_event;

static uint32_t CLAP_ABI one_ev_size(const clap_input_events_t *l) {
   (void)l;
   return 1;
}
static const clap_event_header_t *CLAP_ABI one_ev_get(const clap_input_events_t *l, uint32_t i) {
   if (i != 0) return NULL;
   return (const clap_event_header_t *)&((one_param_event *)l)->ev;
}

static void send_param_value(const clap_plugin_params_t *params, const clap_plugin_t *plug, double v) {
   one_param_event in = {0};
   in.list.ctx = NULL;
   in.list.size = one_ev_size;
   in.list.get = one_ev_get;
   in.ev.header.size = sizeof(in.ev);
   in.ev.header.time = 0;
   in.ev.header.space_id = CLAP_CORE_EVENT_SPACE_ID;
   in.ev.header.type = CLAP_EVENT_PARAM_VALUE;
   in.ev.header.flags = 0;
   in.ev.param_id = 0;
   in.ev.value = v;
   params->flush(plug, &in.list, NULL);
}

// --- fake host ---

static int g_rescan_calls;

static void CLAP_ABI host_params_rescan(const clap_host_t *host, clap_param_rescan_flags flags) {
   (void)host;
   if (flags & CLAP_PARAM_RESCAN_VALUES) g_rescan_calls++;
}
static void CLAP_ABI host_params_clear(const clap_host_t *h, clap_id i, clap_param_clear_flags f) {
   (void)h;
   (void)i;
   (void)f;
}
static void CLAP_ABI host_params_request_flush(const clap_host_t *h) { (void)h; }
static const clap_host_params_t g_host_params = {host_params_rescan, host_params_clear, host_params_request_flush};

static const void *CLAP_ABI host_get_extension(const clap_host_t *host, const char *id) {
   (void)host;
   if (!strcmp(id, CLAP_EXT_PARAMS)) return &g_host_params;
   return NULL;
}
static void CLAP_ABI host_request_restart(const clap_host_t *h) { (void)h; }
static void CLAP_ABI host_request_process(const clap_host_t *h) { (void)h; }
static void CLAP_ABI host_request_callback(const clap_host_t *h) { (void)h; }

static clap_host_t g_host = {
    .clap_version = CLAP_VERSION_INIT,
    .host_data = NULL,
    .name = "test-state-host",
    .vendor = "WAIL",
    .url = "https://github.com/MostDistant/WAIL",
    .version = "0",
    .get_extension = host_get_extension,
    .request_restart = host_request_restart,
    .request_process = host_request_process,
    .request_callback = host_request_callback,
};

int main(int argc, char **argv) {
   CHECK(argc >= 3, "usage: test_state <plugin-binary> <plugin-id> [stream-index]");
   const char *path = argv[1];
   const char *want_id = argv[2];
   int check_param = argc > 3;

   void *lib = lib_open(path);
   CHECK(lib != NULL, "load bundle");
   const clap_plugin_entry_t *entry = (const clap_plugin_entry_t *)lib_sym(lib, "clap_entry");
   CHECK(entry != NULL, "clap_entry symbol");
   CHECK(entry->init("/tmp"), "entry init");
   const clap_plugin_factory_t *fac =
       (const clap_plugin_factory_t *)entry->get_factory(CLAP_PLUGIN_FACTORY_ID);
   CHECK(fac != NULL, "plugin factory");

   const clap_plugin_t *plug = fac->create_plugin(fac, &g_host, want_id);
   CHECK(plug != NULL, "create plugin");
   CHECK(plug->init(plug), "plugin init");

   const clap_plugin_params_t *params = NULL;
   if (check_param) {
      params = (const clap_plugin_params_t *)plug->get_extension(plug, CLAP_EXT_PARAMS);
      CHECK(params != NULL, "send plugin must expose clap.params");
      double v = -1;
      CHECK(params->get_value(plug, 0, &v) && v == 0, "stream index defaults to 0");
   }

   // The extension under test.
   const clap_plugin_state_t *state =
       (const clap_plugin_state_t *)plug->get_extension(plug, CLAP_EXT_STATE);
   CHECK(state != NULL && state->save && state->load, "plugin must expose clap.state");

   // Set a non-default value, save, mutate, load — the saved value must win.
   if (check_param) {
      send_param_value(params, plug, 7);
      double v = -1;
      CHECK(params->get_value(plug, 0, &v) && v == 7, "param event sets stream index");
   }

   mem_out out = {.s = {.ctx = NULL, .write = mem_write}, .len = 0};
   CHECK(state->save(plug, &out.s), "save");
   CHECK(out.len > 0, "save wrote bytes");

   if (check_param) send_param_value(params, plug, 3); // diverge from saved state

   mem_in in = {.s = {.ctx = NULL, .read = mem_read}, .len = out.len, .pos = 0};
   memcpy(in.buf, out.buf, out.len);
   CHECK(state->load(plug, &in.s), "load");

   if (check_param) {
      double v = -1;
      CHECK(params->get_value(plug, 0, &v) && v == 7, "load restores the saved stream index");
      CHECK(g_rescan_calls > 0, "load asks the host to rescan param values");
   }

   // A truncated stream must fail cleanly (no crash, no half-applied state).
   mem_in bad = {.s = {.ctx = NULL, .read = mem_read}, .len = 2, .pos = 0};
   memcpy(bad.buf, out.buf, 2);
   CHECK(!state->load(plug, &bad.s), "truncated stream is rejected");
   if (check_param) {
      double v = -1;
      CHECK(params->get_value(plug, 0, &v) && v == 7, "rejected load leaves state untouched");
   }

   plug->destroy(plug);
   entry->deinit();
   printf("PASS: %s state roundtrip\n", want_id);
   return 0;
}
