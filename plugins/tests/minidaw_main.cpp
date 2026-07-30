// minidaw — a miniature DAW for system simulation (no real DAW involved).
//
// One process = one Link peer + one hosted CLAP plugin + a Link-faithful
// transport (rolling, song_pos from session beats, session tempo) + a
// real-time block pump. Mode "recv": host WAIL Receive, wait for a
// room-published ("WAIL · ") channel to be assigned a port, then measure the
// phase offset of arriving click onsets against the local session grid.
//
// This is the deterministic form of the field ritual (remote click vs the
// DAW's grid): the room metronome is grid-rendered by the sender, so any
// measured sub-beat offset is the receive chain's error — publish path,
// bridge filter, stamp alignment, output pipeline.
//
// Usage:
//   minidaw recv --plugin <path-to-wail-recv> [--seconds 20]
//            [--threshold-ms 5] [--name-contains Metronome] [--verbose]
//
// Exit 0 = PASS (|median onset offset| <= threshold), 1 = FAIL/timeout.

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

#include "wail_link.h"
#include "plugin_host.h"

namespace {

constexpr double kRate = 48000.0;
constexpr uint32_t kBlock = 256;

struct Args {
   std::string plugin;
   std::string nameContains = "Metronome";
   int seconds = 20;
   double thresholdMs = 5.0;
   bool verbose = false;
};

Args parseArgs(int argc, char **argv) {
   Args a;
   for (int i = 1; i < argc; i++) {
      std::string s = argv[i];
      auto next = [&](const char *what) -> std::string {
         if (++i >= argc) {
            fprintf(stderr, "missing value for %s\n", what);
            exit(2);
         }
         return argv[i];
      };
      if (s == "recv") continue;
      else if (s == "--plugin") a.plugin = next("--plugin");
      else if (s == "--seconds") a.seconds = atoi(next("--seconds").c_str());
      else if (s == "--threshold-ms") a.thresholdMs = atof(next("--threshold-ms").c_str());
      else if (s == "--name-contains") a.nameContains = next("--name-contains");
      else if (s == "--verbose") a.verbose = true;
      else {
         fprintf(stderr, "unknown arg: %s\n", s.c_str());
         exit(2);
      }
   }
   if (a.plugin.empty()) {
      fprintf(stderr, "usage: minidaw recv --plugin <path> [--seconds N] [--threshold-ms X] [--name-contains S] [--verbose]\n");
      exit(2);
   }
   return a;
}

// Link-fed transport: exactly what Bitwig serves plugins (confirmed by the
// transport probe) — rolling, beats timeline, tempo, fixed-point position.
struct LinkTransport {
   lb_link *link = nullptr;
   clap_event_transport_t tr{};

   void fill(clap_process_t *p, lb_state *st, int64_t clockMicros) {
      double beat = lb_beat_at_time(st, clockMicros, 4.0);
      double tempo = lb_tempo(link);
      tr.header.size = sizeof(tr);
      tr.header.space_id = CLAP_CORE_EVENT_SPACE_ID;
      tr.header.type = CLAP_EVENT_TRANSPORT;
      tr.flags = CLAP_TRANSPORT_IS_PLAYING | CLAP_TRANSPORT_HAS_BEATS_TIMELINE |
                 CLAP_TRANSPORT_HAS_TEMPO;
      tr.song_pos_beats = (clap_beattime)llround(beat * (double)CLAP_BEATTIME_FACTOR);
      tr.tempo = tempo > 0 ? tempo : 120.0;
      p->transport = &tr;
   }
};

// Onset: a block whose RMS jumps well above the running floor. When one is
// found, the precise onset sample is located within the block — block-
// quantized detection would add up to a whole block of jitter (at 240bpm a
// beat is 46.875 blocks, producing an 8-beat sawtooth artifact that swamps
// the sub-5ms signal we're measuring).
struct OnsetDetector {
   double floor = 1e-4;
   bool armed = true;

   // Returns true on onset; onsetIndex gets the sample index within the block
   // (0 = block start) where the click actually begins.
   bool block(const float *l, uint32_t n, uint32_t *onsetIndex, double *rmsOut) {
      double sq = 0;
      for (uint32_t i = 0; i < n; i++) sq += (double)l[i] * l[i];
      double rms = sqrt(sq / n);
      *rmsOut = rms;
      bool onset = armed && rms > floor * 6 && rms > 0.01;
      if (onset) {
         armed = false;
         double thresh = floor * 6 > 0.005 ? floor * 6 : 0.005;
         *onsetIndex = 0;
         for (uint32_t i = 0; i < n; i++) {
            if (fabsf(l[i]) > (float)thresh) {
               *onsetIndex = i;
               break;
            }
         }
      }
      if (rms < floor * 2) armed = true;
      floor = 0.98 * floor + 0.02 * rms;
      return onset;
   }
};

} // namespace

int main(int argc, char **argv) {
   Args args = parseArgs(argc, argv);

   lb_link *link = lb_create(120.0);
   lb_enable(link, true);
   lb_enable_audio(link, true);

   wailtest::ClapInstance inst;
   std::string err;
   if (!inst.load(args.plugin.c_str(), "software.wail.recv", kRate, kBlock, kBlock, &err)) {
      fprintf(stderr, "load failed: %s\n", err.c_str());
      return 1;
   }

   constexpr int kPorts = 16;
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

   auto portName = [&](uint32_t idx) -> std::string {
      auto *ap = (const clap_plugin_audio_ports_t *)inst.plugin->get_extension(inst.plugin, CLAP_EXT_AUDIO_PORTS);
      clap_audio_port_info_t info{};
      if (!ap || !ap->get(inst.plugin, idx, false, &info)) return {};
      return info.name;
   };

   LinkTransport transport;
   transport.link = link;
   OnsetDetector onset;

   int targetPort = -1;
   std::vector<double> offsetsMs;
   auto t0 = std::chrono::steady_clock::now();
   auto deadline = t0 + std::chrono::seconds(args.seconds);
   auto next = t0;

   while (std::chrono::steady_clock::now() < deadline) {
      next += std::chrono::microseconds((int64_t)(kBlock * 1000000.0 / kRate));
      std::this_thread::sleep_until(next - std::chrono::milliseconds(2));
      while (std::chrono::steady_clock::now() < next) {
      }

      lb_state *st = lb_capture(link);
      int64_t clock = lb_clock_micros(link);
      double beatNow = lb_beat_at_time(st, clock, 4.0);
      double tempo = lb_tempo(link);

      clap_process_t p{};
      p.steady_time = -1;
      p.frames_count = kBlock;
      transport.fill(&p, st, clock);
      p.audio_outputs_count = kPorts;
      p.audio_outputs = outs;
      inst.plugin->process(inst.plugin, &p);
      lb_release(st);

      // Discover the assigned port by name.
      if (targetPort < 0) {
         for (uint32_t i = 0; i < kPorts; i++) {
            std::string nm = portName(i);
            if (nm.find(args.nameContains) != std::string::npos) {
               targetPort = (int)i;
               printf("[minidaw] target channel on port %u: \"%s\"\n", i, nm.c_str());
               break;
            }
         }
         continue;
      }

      uint32_t onsetIdx = 0;
      double rms;
      if (onset.block(outL[targetPort].data(), kBlock, &onsetIdx, &rms)) {
         // Precise onset position: block start beat + sample offset in beats.
         double onsetBeat = beatNow + (double)onsetIdx / kRate * (tempo > 0 ? tempo : 120.0) / 60.0;
         double frac = onsetBeat - floor(onsetBeat + 0.5);
         double ms = frac * 60000.0 / (tempo > 0 ? tempo : 120.0);
         offsetsMs.push_back(ms);
         if (args.verbose)
            printf("[minidaw] onset at beat %.4f → offset %+.2f ms (rms %.3f)\n", onsetBeat, ms, rms);
      }
   }

   inst.teardown();
   lb_destroy(link);

   if (targetPort < 0) {
      printf("[minidaw] FAIL: no \"WAIL · \" channel containing \"%s\" appeared within %ds\n",
             args.nameContains.c_str(), args.seconds);
      return 1;
   }
   if (offsetsMs.size() < 4) {
      printf("[minidaw] FAIL: only %zu onsets detected — stream silent or too quiet\n", offsetsMs.size());
      return 1;
   }
   std::sort(offsetsMs.begin(), offsetsMs.end());
   double median = offsetsMs[offsetsMs.size() / 2];
   double lo = offsetsMs[offsetsMs.size() / 10];
   double hi = offsetsMs[offsetsMs.size() - 1 - offsetsMs.size() / 10];
   printf("[minidaw] onsets=%zu  median offset=%+.2f ms  p10=%+.2f  p90=%+.2f  (threshold ±%.1f ms)\n",
          offsetsMs.size(), median, lo, hi, args.thresholdMs);
   bool pass = fabs(median) <= args.thresholdMs;
   printf("[minidaw] %s\n", pass ? "PASS" : "FAIL");
   return pass ? 0 : 1;
}
