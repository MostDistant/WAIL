// Smoke test for the Link Bridge spike (ADR-0007 gate 1): host the spike
// plugin via clap-trap, pump blocks, and confirm it creates a Link peer and
// writes its heartbeat log. Session-membership proof (peers > 0) needs a
// second Link peer on the LAN — that's the Bitwig/Live manual step; here we
// prove the bundle loads, activates, enables Link, and processes without
// crashing.

#include <cmath>
#include <cstdio>
#include <cstring>
#include <string>

#include "linkbridge_link.h"
#include "plugin_host.h"
#include "test_framework.h"

TEST(linkbridge_spike_hosts_and_logs) {
   // Fresh log so we can attribute lines to this run.
   std::remove("/tmp/linkbridge-spike.log");

   wailtest::ClapInstance inst;
   std::string err;
   CHECK_MSG(inst.load(WAIL_LINKBRIDGE_SPIKE_PATH, "software.linkbridge.spike", 0, 48000.0, 256, 256, &err),
             err.c_str());
   if (!inst.plugin) return;

   // Pump ~3s of blocks, paced: Link session discovery fires on a ~1s
   // announcement interval, so an unpaced burst (ms of wall time) would
   // finish before any peer can be seen. With a second peer on the LAN (e.g.
   // linktempo), the heartbeat should report peers=1 and the converged tempo.
   std::vector<float> buf(256, 0.0f);
   float *ch[2] = {buf.data(), buf.data()};
   clap_audio_buffer_t out{};
   out.channel_count = 2;
   out.data32 = ch;
   for (int i = 0; i < 600; i++) {
      clap_process_t p{};
      p.steady_time = -1;
      p.frames_count = 256;
      p.audio_outputs_count = 1;
      p.audio_outputs = &out;
      inst.plugin->process(inst.plugin, &p);
      wail_sleep_ms(5);
   }

   inst.teardown(); // deactivate: flushes + closes the log

   FILE *f = fopen("/tmp/linkbridge-spike.log", "r");
   CHECK_MSG(f != nullptr, "spike wrote no log — Link peer creation or activate failed");
   if (!f) return;
   char line[512];
   bool sawActivate = false, sawPeers = false;
   while (fgets(line, sizeof(line), f)) {
      std::string s(line);
      if (s.find("spike activated") != std::string::npos) sawActivate = true;
      if (s.find("peers=") != std::string::npos) sawPeers = true;
   }
   fclose(f);
   CHECK(sawActivate);
   CHECK_MSG(sawPeers, "no peers= heartbeat — Link peer not running");
   // Session-membership proof when a second peer is on the LAN: WAIL_LB_EXPECT_PEERS
   // is set by the manual/CI gate-1 run (linktempo alongside); unset in the
   // default suite where the LAN may be empty.
   if (getenv("WAIL_LB_EXPECT_PEERS")) {
      char tail[512] = {0};
      f = fopen("/tmp/linkbridge-spike.log", "r");
      if (f) {
         while (fgets(line, sizeof(line), f)) strncpy(tail, line, sizeof(tail) - 1);
         fclose(f);
      }
      CHECK_MSG(strstr(tail, "peers=0") == nullptr, "expected peers>0 with a second peer on the LAN");
   }
}

// Pub/sub roundtrip (Link Bridge Send): host the send plugin via clap-trap,
// feed a 440Hz sine, and a second Link peer in this process subscribes to the
// published channel and measures what arrives — the whole Link Audio send
// path, no DAW, no WAIL app.
TEST(linkbridge_send_pubsub_roundtrip) {
   wailtest::ClapInstance inst;
   std::string err;
   CHECK_MSG(inst.load(WAIL_LINKBRIDGE_SEND_PATH, "software.linkbridge.send", 0, 48000.0, 256, 256, &err),
             err.c_str());
   if (!inst.plugin) return;

   // Subscriber side: our own Link peer in the test process.
   lb_link *sub = lb_create(120.0);
   lb_enable(sub, true);
   lb_enable_audio(sub, true);

   const uint32_t kBlock = 256;
   std::vector<float> inL(kBlock), inR(kBlock), outL(kBlock), outR(kBlock);
   float *inCh[2] = {inL.data(), inR.data()};
   float *outCh[2] = {outL.data(), outR.data()};
   clap_audio_buffer_t inBuf{}, outBuf{};
   inBuf.channel_count = 2;
   inBuf.data32 = inCh;
   outBuf.channel_count = 2;
   outBuf.data32 = outCh;

   auto pump = [&]() {
      static uint64_t frame = 0;
      for (uint32_t i = 0; i < kBlock; i++) {
         float v = 0.5f * sinf(2.0f * 3.14159265f * 440.0f * (float)(frame + i) / 48000.0f);
         inL[i] = v;
         inR[i] = v;
      }
      frame += kBlock;
      clap_process_t p{};
      p.steady_time = -1;
      p.frames_count = kBlock;
      p.audio_inputs_count = 1;
      p.audio_inputs = &inBuf;
      p.audio_outputs_count = 1;
      p.audio_outputs = &outBuf;
      inst.plugin->process(inst.plugin, &p);
   };

   // Wait for the channel to appear, pumping the plugin meanwhile.
   uint64_t chanId = 0;
   {
      auto t0 = std::chrono::steady_clock::now();
      while (!chanId && std::chrono::steady_clock::now() - t0 < std::chrono::seconds(10)) {
         pump();
         lb_channel_info chans[LB_MAX_CHANNELS];
         size_t n = lb_channels(sub, chans, LB_MAX_CHANNELS);
         for (size_t i = 0; i < n; i++)
            if (std::string(chans[i].name).find("Link Bridge Send") != std::string::npos)
               chanId = chans[i].id_u64;
         wail_sleep_ms(20);
      }
   }
   CHECK_MSG(chanId != 0, "Link Bridge Send channel never appeared on the LAN");
   if (!chanId) {
      inst.teardown();
      lb_destroy(sub);
      return;
   }

   lb_source *src = lb_source_create(sub, chanId);

   // Collect ~1.5s of audio.
   std::vector<int16_t> got;
   {
      auto t0 = std::chrono::steady_clock::now();
      lb_source_buffer b;
      while (got.size() < 48000 * 3 / 2 &&
             std::chrono::steady_clock::now() - t0 < std::chrono::seconds(10)) {
         pump();
         while (lb_source_pop(src, &b))
            got.insert(got.end(), b.samples, b.samples + b.num_frames * 2);
         wail_sleep_ms(5);
      }
   }
   CHECK_MSG(got.size() >= 48000,
             "received too little audio (" + std::to_string(got.size()) + " frames-pairs)");

   // Measure: RMS and zero-crossing frequency on the left channel.
   if (got.size() >= 48000) {
      double sq = 0;
      int crossings = 0;
      size_t frames = got.size() / 2;
      for (size_t i = 0; i < frames; i++) {
         double l = got[2 * i] / 32768.0;
         sq += l * l;
         if (i > 0 && ((got[2 * i - 2] < 0) != (got[2 * i] < 0))) crossings++;
      }
      double rms = sqrt(sq / frames);
      double seconds = (double)frames / 48000.0;
      double freq = crossings / 2.0 / seconds;
      CHECK_MSG(rms > 0.1, "received audio too quiet (rms=" + std::to_string(rms) + ") — send path broken");
      CHECK_MSG(freq > 380 && freq < 500, "frequency off: got " + std::to_string(freq) + " Hz, want ~440");
   }

   lb_source_destroy(src);
   inst.teardown();
   lb_destroy(sub);
}
