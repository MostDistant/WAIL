// Smoke test for the Link Bridge spike (ADR-0007 gate 1): host the spike
// plugin via clap-trap, pump blocks, and confirm it creates a Link peer and
// writes its heartbeat log. Session-membership proof (peers > 0) needs a
// second Link peer on the LAN — that's the Bitwig/Live manual step; here we
// prove the bundle loads, activates, enables Link, and processes without
// crashing.

#include <cstdio>
#include <cstring>
#include <string>

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
