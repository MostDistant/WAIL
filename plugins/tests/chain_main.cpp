// wail-plugin-chain — E2E driver for scripts/plugin-e2e.sh.
//
// Hosts the real wail-send + wail-recv CLAP plugins (via clap-trap), wired to two
// real headless WAIL apps over loopback IPC:
//
//   sweep → wail-send → sender app → Opus/WAIF → relay → receiver app → wail-recv → stats
//
// It feeds a log frequency sweep into send and drives BOTH plugins' process() in
// real time (48 kHz / 256-frame blocks, like a DAW callback — the app's
// ipcCaptureSource anchors the plugin frame counter to the Link clock once and
// extrapolates by sample count, so bursting would smear the interval grid).
// What comes back on recv port 0 is reported as per-second stat lines (RMS +
// zero-crossing frequency, mirroring cmd/linkaudio-probe); the script judges the
// verdict.
//
// Usage: wail-plugin-chain --send PATH --recv PATH --send-port P --recv-port P
//                          [--seconds 30] [--tail-seconds 12] [--warmup-ms 1500]
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>

#include "plugin_host.h"

namespace {

constexpr double kRate = 48000.0;
constexpr uint32_t kBlock = 256;
constexpr double kPi = 3.14159265358979323846;

struct Args {
   std::string sendPath, recvPath;
   int sendPort = 0, recvPort = 0;
   double seconds = 30.0;
   double tailSeconds = 12.0; // keep pumping after the sweep: received audio lags by ~2 intervals
   int warmupMs = 1500;
};

bool parseArgs(int argc, char **argv, Args &a) {
   for (int i = 1; i < argc; i++) {
      std::string k = argv[i];
      auto next = [&](const char **out) -> bool {
         if (i + 1 >= argc) return false;
         *out = argv[++i];
         return true;
      };
      const char *v = nullptr;
      if (k == "--send" && next(&v)) a.sendPath = v;
      else if (k == "--recv" && next(&v)) a.recvPath = v;
      else if (k == "--send-port" && next(&v)) a.sendPort = atoi(v);
      else if (k == "--recv-port" && next(&v)) a.recvPort = atoi(v);
      else if (k == "--seconds" && next(&v)) a.seconds = atof(v);
      else if (k == "--tail-seconds" && next(&v)) a.tailSeconds = atof(v);
      else if (k == "--warmup-ms" && next(&v)) a.warmupMs = atoi(v);
      else {
         std::fprintf(stderr, "unknown/incomplete arg: %s\n", k.c_str());
         return false;
      }
   }
   if (a.sendPath.empty() || a.recvPath.empty() || a.sendPort <= 0 || a.recvPort <= 0) {
      std::fprintf(stderr, "required: --send PATH --recv PATH --send-port P --recv-port P\n");
      return false;
   }
   return true;
}

} // namespace

int main(int argc, char **argv) {
   Args args;
   if (!parseArgs(argc, argv, args)) return 2;

   wailtest::SendHost send;
   wailtest::RecvHost recv;
   std::string err;
   if (!send.setup(args.sendPath.c_str(), args.sendPort, kRate, kBlock, &err)) {
      std::fprintf(stderr, "send setup failed: %s\n", err.c_str());
      return 1;
   }
   // WAIL_IPC_ADDR is process-global and each plugin's IPC thread resolves it
   // once, asynchronously, at thread start. Let the send plugin's thread resolve
   // it before recv's setup overwrites it — otherwise the send plugin can dial
   // the recv app (500ms matches the plugins' own reconnect tick, far beyond any
   // scheduling jitter; the env read is the thread's first action).
   std::this_thread::sleep_for(std::chrono::milliseconds(500));
   if (!recv.setup(args.recvPath.c_str(), args.recvPort, kRate, kBlock, &err)) {
      std::fprintf(stderr, "recv setup failed: %s\n", err.c_str());
      return 1;
   }

   std::fprintf(stderr, "chain: plugins up (send→:%d, recv→:%d), warmup %dms\n",
                args.sendPort, args.recvPort, args.warmupMs);
   std::this_thread::sleep_for(std::chrono::milliseconds(args.warmupMs));

   const double f0 = 80.0, f1 = 12000.0;
   const uint64_t totalFrames = (uint64_t)((args.seconds + args.tailSeconds) * kRate);
   double phase = 0.0;

   // Per-second receive stats (mirrors linkaudio-probe's rms + zero-crossing freq).
   double sumSq = 0.0;
   uint64_t winFrames = 0, zeroCross = 0, others = 0;
   int prevSign = 1, sec = 0;
   bool namePrinted = false;

   auto t0 = std::chrono::steady_clock::now();
   for (uint64_t frame = 0; frame < totalFrames; frame += kBlock) {
      for (uint32_t i = 0; i < kBlock; i++) {
         double ti = (double)(frame + i) / kRate;
         float v = 0.0f;
         if (ti < args.seconds) {
            double f = f0 * std::pow(f1 / f0, ti / args.seconds); // log sweep
            phase += 2.0 * kPi * f / kRate;
            v = (float)(0.5 * std::sin(phase));
         }
         send.inL[i] = v;
         send.inR[i] = v;
      }
      send.processBlock(true);
      recv.processBlock();

      for (uint32_t i = 0; i < kBlock; i++) {
         float s = recv.outL[0][i];
         sumSq += (double)s * (double)s;
         int sign = s >= 0.0f ? 1 : -1;
         if (s != 0.0f && sign != prevSign) zeroCross++;
         if (s != 0.0f) prevSign = sign;
         if (!namePrinted && (s != 0.0f || recv.outR[0][i] != 0.0f)) {
            namePrinted = true;
            recv.inst.plugin->on_main_thread(recv.inst.plugin);
            std::fprintf(stderr, "chain-recv: port0 name=\"%s\" (first audio)\n",
                         recv.portName(0).c_str());
         }
      }
      for (int p = 1; p < wailtest::RecvHost::kPorts; p++)
         for (uint32_t i = 0; i < kBlock; i++)
            if (recv.outL[p][i] != 0.0f || recv.outR[p][i] != 0.0f) others++;
      winFrames += kBlock;

      if (winFrames >= (uint64_t)kRate) {
         double rms = std::sqrt(sumSq / (double)winFrames) * 32768.0;
         double freq = ((double)zeroCross / 2.0) / ((double)winFrames / kRate);
         std::fprintf(stderr, "chain-recv: sec=%d rms=%d ~%dHz others=%llu\n",
                      sec, (int)rms, (int)freq, (unsigned long long)others);
         sumSq = 0.0;
         winFrames = 0;
         zeroCross = 0;
         others = 0;
         sec++;
      }

      // Real-time pacing: one 256-frame block per 5.33ms, deadline-anchored.
      auto target = t0 + std::chrono::duration_cast<std::chrono::steady_clock::duration>(
                             std::chrono::duration<double>((double)(frame + kBlock) / kRate));
      std::this_thread::sleep_until(target);
   }

   std::fprintf(stderr, "chain: sweep complete, tearing down\n");
   return 0;
}
