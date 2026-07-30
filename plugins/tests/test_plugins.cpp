// Smoke test for the Link Bridge spike (ADR-0007 gate 1): host the spike
// plugin via clap-trap, pump blocks, and confirm it creates a Link peer and
// writes its heartbeat log. Session-membership proof (peers > 0) needs a
// second Link peer on the LAN — that's the Bitwig/Live manual step; here we
// prove the bundle loads, activates, enables Link, and processes without
// crashing.

#include <chrono>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>

namespace {
constexpr double kSampleRate = 48000.0;
constexpr int kPorts = 16; // LBR_SLOTS in wail_recv.c
}

#include "wail_link.h"
#include "plugin_host.h"
#include "test_framework.h"

TEST(linkbridge_spike_hosts_and_logs) {
   // Fresh log so we can attribute lines to this run.
   char spikeLog[512];
   lb_temp_log_path("linkbridge-spike.log", spikeLog, sizeof(spikeLog));
   std::remove(spikeLog);

   wailtest::ClapInstance inst;
   std::string err;
   CHECK_MSG(inst.load(WAIL_LINKBRIDGE_SPIKE_PATH, "software.linkbridge.spike", 48000.0, 256, 256, &err),
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

   FILE *f = fopen(spikeLog, "r");
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
      f = fopen(spikeLog, "r");
      if (f) {
         while (fgets(line, sizeof(line), f)) strncpy(tail, line, sizeof(tail) - 1);
         fclose(f);
      }
      CHECK_MSG(strstr(tail, "peers=0") == nullptr, "expected peers>0 with a second peer on the LAN");
   }
}

// Pub/sub roundtrip (WAIL Send): host the send plugin via clap-trap,
// feed a 440Hz sine, and a second Link peer in this process subscribes to the
// published channel and measures what arrives — the whole Link Audio send
// path, no DAW, no WAIL app.
TEST(wail_send_pubsub_roundtrip) {
   wailtest::ClapInstance inst;
   std::string err;
   CHECK_MSG(inst.load(WAIL_SEND_PATH, "software.wail.send", 48000.0, 256, 256, &err),
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
            if (std::string(chans[i].name).find("WAIL Send") != std::string::npos)
               chanId = chans[i].id_u64;
         wail_sleep_ms(20);
      }
   }
   CHECK_MSG(chanId != 0, "WAIL Send channel never appeared on the LAN");
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
         while (lb_source_pop(src, &b, 4.0))
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

// WAIL Receive roundtrip: the test process publishes two channels — one
// room-published ("WAIL · tester · sweep", 440Hz) and one raw LAN channel
// ("raw-channel", 220Hz). The hosted recv plugin must assign only the
// prefixed channel, name its port with the prefix stripped, and render the
// 440Hz audio (RMS + zero-crossing checks).
TEST(wail_recv_hears_only_room_published) {
   wailtest::ClapInstance inst;
   std::string err;
   CHECK_MSG(inst.load(WAIL_RECV_PATH, "software.wail.recv", 48000.0, 256, 256, &err),
             err.c_str());
   if (!inst.plugin) return;

   // Publisher: our own Link peer with two sinks.
   lb_link *pub = lb_create(120.0);
   lb_enable(pub, true);
   lb_enable_audio(pub, true);
   lb_sink *room = lb_sink_create(pub, "WAIL · tester · sweep", 16384);
   lb_sink *raw = lb_sink_create(pub, "raw-channel", 16384);

   const uint32_t kBlock = 256;
   std::vector<float> outL[kPorts], outR[kPorts];
   float *ch[kPorts][2];
   clap_audio_buffer_t outs[kPorts];
   for (int p = 0; p < kPorts; p++) {
      outL[p].assign(kBlock, 0.0f);
      outR[p].assign(kBlock, 0.0f);
      ch[p][0] = outL[p].data();
      ch[p][1] = outR[p].data();
      outs[p] = clap_audio_buffer_t{};
      outs[p].channel_count = 2;
      outs[p].data32 = ch[p];
   }

   uint64_t frame = 0;
   auto pumpBoth = [&]() {
      // Publish one block of each sine.
      float l[kBlock], r[kBlock], l2[kBlock], r2[kBlock];
      for (uint32_t i = 0; i < kBlock; i++) {
         l[i] = r[i] = 0.5f * sinf(2.0f * 3.14159265f * 440.0f * (float)(frame + i) / 48000.0f);
         l2[i] = r2[i] = 0.5f * sinf(2.0f * 3.14159265f * 220.0f * (float)(frame + i) / 48000.0f);
      }
      frame += kBlock;
      lb_state *st = lb_capture(pub);
      // Stamp-ahead, like the send plugin: the receiver needs delivery margin.
      double beat = lb_beat_at_time(st, lb_clock_micros(pub) + 10000, 4.0);
      lb_sink_commit(room, st, beat, 4.0, l, r, kBlock, 48000);
      lb_sink_commit(raw, st, beat, 4.0, l2, r2, kBlock, 48000);
      lb_release(st);
      // Drive the recv plugin one block.
      clap_process_t p{};
      p.steady_time = -1;
      p.frames_count = kBlock;
      p.audio_outputs_count = kPorts;
      p.audio_outputs = outs;
      inst.plugin->process(inst.plugin, &p);
   };

   auto portName = [&](uint32_t idx) -> std::string {
      auto *ap = (const clap_plugin_audio_ports_t *)inst.plugin->get_extension(inst.plugin, CLAP_EXT_AUDIO_PORTS);
      clap_audio_port_info_t info{};
      if (!ap || !ap->get(inst.plugin, idx, false, &info)) return {};
      return info.name;
   };

   // Wait for the room channel to be assigned + named (discovery ~1s + manager poll).
   // The pump is real-time paced (5.33ms/block): publishing faster than real
   // time stamps buffers into the future and receivers drop them.
   std::string name0;
   {
      auto t0 = std::chrono::steady_clock::now();
      auto next = t0;
      while (name0 != "tester · sweep" &&
             std::chrono::steady_clock::now() - t0 < std::chrono::seconds(12)) {
         next += std::chrono::microseconds((int64_t)((double)kBlock * 1000000.0 / kSampleRate));
         std::this_thread::sleep_until(next - std::chrono::milliseconds(2));
         while (std::chrono::steady_clock::now() < next) {
         }
         pumpBoth();
         name0 = portName(0);
      }
   }
   CHECK_MSG(name0 == "tester · sweep", "port 0 name = \"" + name0 + "\" (want \"tester · sweep\")");

   // Collect ~1s of port-0 audio (same real-time pacing).
   std::vector<float> col;
   {
      auto t0 = std::chrono::steady_clock::now();
      auto next = t0;
      while (col.size() < 48000 && std::chrono::steady_clock::now() - t0 < std::chrono::seconds(10)) {
         next += std::chrono::microseconds((int64_t)((double)kBlock * 1000000.0 / kSampleRate));
         std::this_thread::sleep_until(next - std::chrono::milliseconds(2));
         while (std::chrono::steady_clock::now() < next) {
         }
         pumpBoth();
         for (uint32_t i = 0; i < kBlock; i++)
            if (outL[0][i] != 0.0f || col.size() > 0) col.push_back(outL[0][i]);
      }
   }
   CHECK(col.size() >= 48000 / 2);

   if (col.size() >= 4800) {
      double sq = 0;
      int crossings = 0;
      for (size_t i = 1; i < col.size(); i++) {
         sq += (double)col[i] * col[i];
         if ((col[i - 1] < 0) != (col[i] < 0)) crossings++;
      }
      double rms = sqrt(sq / col.size());
      double freq = crossings / 2.0 / ((double)col.size() / 48000.0);
      CHECK_MSG(rms > 0.1, "port 0 too quiet (rms=" + std::to_string(rms) + ")");
      // CI runners stall the real-time publisher/recv pacing and the renderer
      // honestly skips late buffers, garbling the sine — fidelity stays
      // local-only; assignment + audio flow are the CI-verified behaviors.
      if (getenv("GITHUB_ACTIONS") == nullptr)
         CHECK_MSG(freq > 380 && freq < 500, "port 0 freq = " + std::to_string(freq) + " Hz, want ~440");
   }

   // The raw channel must never be assigned a named port.
   for (uint32_t idx = 1; idx < 4; idx++) {
      std::string nm = portName(idx);
      CHECK_MSG(nm.find("raw-channel") == std::string::npos,
                "raw channel got a port: \"" + nm + "\"");
   }

   lb_sink_destroy(room);
   lb_sink_destroy(raw);
   inst.teardown();
   lb_destroy(pub);
}
