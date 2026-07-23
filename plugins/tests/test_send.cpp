// Integration tests for wail-send.clap: host it via clap-trap, play the WAIL
// app's role on the loopback IPC socket (IpcTestServer), and verify the RawPCM
// stream the plugin produces — handshake, framing, sample-exact PCM, transport
// flag, stream-index param — plus passthrough and no-server resilience.
#include <cstdint>
#include <cstring>
#include <functional>
#include <memory>
#include <string>
#include <vector>

#include <clap/ext/track-info.h>

#include <clap-trap/clap-trap.h>

#include "ipc_test_server.h"
#include "plugin_host.h"
#include "test_framework.h"

#ifndef WAIL_SEND_CLAP_PATH
#error "WAIL_SEND_CLAP_PATH must be defined (CMake sets it to the built plugin)"
#endif

using namespace clap_trap;

namespace {

constexpr double kSampleRate = 48000.0;
constexpr uint32_t kBlock = 256;

// Deterministic per-sample patterns, distinct per channel, in [-1, 1). The
// plugin memcpy's captured floats, so received PCM must match bit-exactly.
float patternL(uint64_t i) {
   return (float)((int)((i * 2654435761u) % 20000u) - 10000) / 10000.0f;
}
float patternR(uint64_t i) {
   return (float)((int)((i * 1103515245u + 12345u) % 16000u) - 8000) / 8000.0f;
}

// SendFixture wraps SendHost with pattern-filled inputs and test assertions.
struct SendFixture {
   wailtest::SendHost h;
   const clap_plugin_t *plugin = nullptr;

   bool setup(int ipcPort, const std::function<void(TestHost *)> &configureHost = nullptr) {
      std::string err;
      if (!h.setup(WAIL_SEND_CLAP_PATH, ipcPort, kSampleRate, kBlock, &err, configureHost)) {
         if (!err.empty()) ::wailtest::fail(__LINE__, err);
         return false;
      }
      plugin = h.inst.plugin;
      return true;
   }

   // processBlock fills the inputs with the pattern starting at firstSample,
   // runs one process() call, and leaves the outputs in h.outL/h.outR.
   void processBlock(bool playing, uint64_t firstSample, const clap_input_events_t *ev = nullptr) {
      for (uint32_t i = 0; i < kBlock; i++) {
         h.inL[i] = patternL(firstSample + i);
         h.inR[i] = patternR(firstSample + i);
      }
      h.processBlock(playing, ev);
   }
};

// --- track-info test host state ---
// The clap_host_track_info_t::get callback has no user-data pointer, so the
// test host's track name lives in this global (tests run sequentially).
std::string g_trackName;

bool CLAP_ABI testTrackInfoGet(const clap_host_t *, clap_track_info_t *info) {
   memset(info, 0, sizeof(*info));
   if (g_trackName.empty()) return true; // success, but no HAS_TRACK_NAME flag
   info->flags = CLAP_TRACK_INFO_HAS_TRACK_NAME;
   snprintf(info->name, sizeof(info->name), "%s", g_trackName.c_str());
   return true;
}
const clap_host_track_info_t g_hostTrackInfo = {testTrackInfoGet};

void checkPassthrough(const SendFixture &fx, uint32_t block) {
   for (uint32_t i = 0; i < kBlock; i++) {
      if (fx.h.outL[i] != fx.h.inL[i] || fx.h.outR[i] != fx.h.inR[i]) {
         ::wailtest::fail(__LINE__, "passthrough mismatch in block " + std::to_string(block));
         return;
      }
   }
}

} // namespace

TEST(send_handshake_and_pcm) {
   wailtest::IpcTestServer server;
   int port = server.start();
   CHECK(port > 0);
   if (!port) return;

   SendFixture fx;
   CHECK(fx.setup(port));
   if (!fx.plugin) return;

   CHECK_MSG(server.waitConnected(5000), "plugin did not connect");
   CHECK(server.role() == WAIL_IPC_ROLE_SEND);
   CHECK(server.streamIndex() == 0);

   const uint32_t blocks = 32;
   for (uint32_t b = 0; b < blocks; b++) {
      fx.processBlock(true, (uint64_t)b * kBlock);
      checkPassthrough(fx, b);
   }

   CHECK_MSG(server.waitFrameCount(blocks, 5000), "timed out waiting for RawPCM frames");
   // The stock TestHost serves no clap.track-info: the plugin must stay silent
   // about track names (the app keeps its "Plugin Send N" placeholder).
   CHECK_MSG(!server.hasTag(WAIL_TAG_TRACKNAME),
             "host without track-info: unexpected TrackName frame");
   auto frames = server.frames();
   CHECK(frames.size() >= blocks);
   const uint32_t n = (uint32_t)(frames.size() < blocks ? frames.size() : blocks);
   for (uint32_t b = 0; b < n; b++) {
      wailtest::RawPCM m;
      CHECK_MSG(wailtest::decodeRawPCM(frames[b], m), "frame " + std::to_string(b) + " did not decode");
      if (m.pcm.empty()) continue;
      CHECK(m.streamIndex == 0);
      CHECK(m.flags == WAIL_RAW_FLAG_PLAYING); // float32 payload + playing
      CHECK(m.channels == 2);
      CHECK(m.sampleRate == (uint32_t)kSampleRate);
      CHECK(m.frameCounter == (uint64_t)b * kBlock); // sample-contiguous
      CHECK(m.pcm.size() == kBlock * 2);
      if (m.pcm.size() != kBlock * 2) continue;
      for (uint32_t i = 0; i < kBlock; i++) {
         if (m.pcm[2 * i] != patternL((uint64_t)b * kBlock + i) ||
             m.pcm[2 * i + 1] != patternR((uint64_t)b * kBlock + i)) {
            ::wailtest::fail(__LINE__, "PCM mismatch in block " + std::to_string(b));
            break;
         }
      }
   }
}

TEST(send_transport_playing_flag) {
   wailtest::IpcTestServer server;
   int port = server.start();
   CHECK(port > 0);
   if (!port) return;

   SendFixture fx;
   CHECK(fx.setup(port));
   if (!fx.plugin) return;
   CHECK(server.waitConnected(5000));

   const uint32_t blocks = 8;
   for (uint32_t b = 0; b < blocks; b++)
      fx.processBlock(b % 2 == 0, (uint64_t)b * kBlock); // alternate rolling/stopped

   CHECK(server.waitFrameCount(blocks, 5000));
   auto frames = server.frames();
   const uint32_t n = (uint32_t)(frames.size() < blocks ? frames.size() : blocks);
   for (uint32_t b = 0; b < n; b++) {
      wailtest::RawPCM m;
      if (!wailtest::decodeRawPCM(frames[b], m)) {
         CHECK_MSG(false, "frame " + std::to_string(b) + " did not decode");
         continue;
      }
      bool playing = (m.flags & WAIL_RAW_FLAG_PLAYING) != 0;
      CHECK_MSG(playing == (b % 2 == 0),
                "playing flag mismatch in block " + std::to_string(b));
   }
}

TEST(send_stream_index_param) {
   wailtest::IpcTestServer server;
   int port = server.start();
   CHECK(port > 0);
   if (!port) return;

   SendFixture fx;
   CHECK(fx.setup(port));
   if (!fx.plugin) return;
   CHECK(server.waitConnected(5000));
   CHECK(server.streamIndex() == 0); // default at connect time

   const uint32_t pre = 8, post = 8;
   for (uint32_t b = 0; b < pre; b++) fx.processBlock(true, (uint64_t)b * kBlock);
   CHECK(server.waitFrameCount(pre, 5000));
   wail_sleep_ms(100); // let the ring fully drain: stream index is stamped at drain
                       // time, so it must be quiescent when the param changes

   SimpleInputEvents ev;
   ev.addParamValue(0, 0, 4.0); // time 0, param id 0 ("Stream Index"), value 4
   fx.processBlock(true, (uint64_t)pre * kBlock, ev.get());
   for (uint32_t b = pre + 1; b <= pre + post; b++) fx.processBlock(true, (uint64_t)b * kBlock);

   CHECK(server.waitFrameCount(pre + 1 + post, 5000));
   auto frames = server.frames();
   const uint32_t total = pre + 1 + post;
   const uint32_t n = (uint32_t)(frames.size() < total ? frames.size() : total);
   for (uint32_t b = 0; b < n; b++) {
      wailtest::RawPCM m;
      if (!wailtest::decodeRawPCM(frames[b], m)) continue;
      if (b < pre)
         CHECK_MSG(m.streamIndex == 0, "pre-param frame " + std::to_string(b) + " has wrong stream index");
      else if (b > pre)
         CHECK_MSG(m.streamIndex == 4, "post-param frame " + std::to_string(b) + " has wrong stream index");
      // frame `pre` carries the param event itself: the event is applied after
      // that block's ring push and the drain stamp races it — accept either.
   }
}

TEST(send_track_name_from_host) {
   wailtest::IpcTestServer server;
   int port = server.start();
   CHECK(port > 0);
   if (!port) return;

   // Host serves clap.track-info/1 with a mutable track name.
   g_trackName = "Bass DI";
   SendFixture fx;
   CHECK(fx.setup(port, [](TestHost *h) {
      h->setExtensionCallback([](const char *id) -> const void * {
         if (!strcmp(id, CLAP_EXT_TRACK_INFO) || !strcmp(id, CLAP_EXT_TRACK_INFO_COMPAT))
            return (const void *)&g_hostTrackInfo;
         return nullptr;
      });
   }));
   if (!fx.plugin) return;

   CHECK_MSG(server.waitConnected(5000), "plugin did not connect");
   CHECK_MSG(server.waitTrackName("Bass DI", 5000), "no TrackName frame after connect");

   // Rename the track and notify through the plugin's track-info extension: a
   // fresh TrackName frame must follow without reconnecting.
   g_trackName = "Bass DI (renamed)";
   auto *pti = (const clap_plugin_track_info_t *)fx.plugin->get_extension(fx.plugin, CLAP_EXT_TRACK_INFO);
   CHECK_MSG(pti && pti->changed, "plugin does not expose clap.track-info");
   if (!pti || !pti->changed) return;
   pti->changed(fx.plugin);
   CHECK_MSG(server.waitTrackName("Bass DI (renamed)", 5000), "no TrackName frame after rename");
}

TEST(send_no_server_resilience) {
   // Nothing listening: the IPC thread reconnect-loops, the ring fills (64
   // slots), blocks drop — process() must stay glitch-free and pass audio through.
   int port = wailtest::unusedPort();
   CHECK(port > 0);
   if (!port) return;

   SendFixture fx;
   CHECK(fx.setup(port));
   if (!fx.plugin) return;

   for (uint32_t b = 0; b < 200; b++) fx.processBlock(true, (uint64_t)b * kBlock);
   checkPassthrough(fx, 199);
   // fixture teardown deactivates: joins the IPC thread out of its reconnect sleep.
}
