// Shared CLAP hosting helpers for the WAIL plugin test binaries: load a plugin
// via clap-trap, wire audio buffers, drive process(). Used by the test runner
// (test_send.cpp / test_recv.cpp fixtures) and the E2E chain driver
// (chain_main.cpp). No test-framework dependencies — errors are returned, not
// asserted.
#pragma once

#include <chrono>
#include <cstdint>
#include <cstdio>
#include <memory>
#include <string>
#include <vector>

#include <clap-trap/clap-trap.h>

#include "wail_ipc.h" // wail_sleep_ms

namespace wailtest {

// setIpcAddrEnv points a subsequently-activated plugin at the given loopback port
// (each plugin's IPC thread resolves WAIL_IPC_ADDR once, at activate time — so set
// it before every setup() when hosting several plugins in one process).
inline void setIpcAddrEnv(int port) {
   char buf[64];
   std::snprintf(buf, sizeof(buf), "127.0.0.1:%d", port);
#ifdef _WIN32
   _putenv_s("WAIL_IPC_ADDR", buf);
#else
   setenv("WAIL_IPC_ADDR", buf, 1);
#endif
}

// ClapInstance owns one loaded+activated plugin (loader lifetime, host, lifecycle).
struct ClapInstance {
   std::unique_ptr<clap_trap::PluginLoader> loader;
   std::unique_ptr<clap_trap::TestHost> host;
   const clap_plugin_t *plugin = nullptr;

   // load points the plugin at ipcPort, then loads/activates/starts it.
   // Returns true on success; err receives a message on failure.
   bool load(const char *path, const char *pluginID, int ipcPort, double sampleRate,
             uint32_t minFrames, uint32_t maxFrames, std::string *err) {
      setIpcAddrEnv(ipcPort);
      loader = clap_trap::PluginLoader::load(path);
      if (!loader || !loader->factory()) {
         if (err) *err = "load failed: " + (loader ? loader->getError() : std::string("null loader"));
         return false;
      }
      const clap_plugin_factory_t *factory = loader->factory();
      host = std::make_unique<clap_trap::TestHost>();
      plugin = factory->create_plugin(factory, host->clapHost(), pluginID);
      if (!plugin) {
         if (err) *err = std::string("create_plugin failed for ") + pluginID;
         return false;
      }
      if (!plugin->init(plugin) || !plugin->activate(plugin, sampleRate, minFrames, maxFrames) ||
          !plugin->start_processing(plugin)) {
         if (err) *err = "init/activate/start_processing failed";
         return false;
      }
      return true;
   }

   void teardown() {
      if (!plugin) return;
      plugin->stop_processing(plugin);
      plugin->deactivate(plugin);
      plugin->destroy(plugin);
      plugin = nullptr;
      loader.reset();
   }
   ~ClapInstance() { teardown(); }
};

// SendHost hosts wail-send with one stereo input + one stereo output.
// The caller fills inL/inR, calls processBlock, and reads outL/outR.
struct SendHost {
   ClapInstance inst;
   std::vector<float> inL, inR, outL, outR;

   bool setup(const char *path, int ipcPort, double sampleRate, uint32_t block, std::string *err) {
      if (!inst.load(path, "software.wail.send", ipcPort, sampleRate, block, block, err))
         return false;
      inL.assign(block, 0.0f);
      inR.assign(block, 0.0f);
      outL.assign(block, 0.0f);
      outR.assign(block, 0.0f);
      inCh_[0] = inL.data();
      inCh_[1] = inR.data();
      outCh_[0] = outL.data();
      outCh_[1] = outR.data();
      inBuf_.channel_count = 2;
      inBuf_.data32 = inCh_;
      outBuf_.channel_count = 2;
      outBuf_.data32 = outCh_;
      transport_.header.size = sizeof(transport_);
      transport_.header.time = 0;
      transport_.header.space_id = CLAP_CORE_EVENT_SPACE_ID;
      transport_.header.type = CLAP_EVENT_TRANSPORT;
      transport_.header.flags = 0;
      return true;
   }

   clap_process_status processBlock(bool playing, const clap_input_events_t *ev = nullptr) {
      transport_.flags = playing ? CLAP_TRANSPORT_IS_PLAYING : 0;
      clap_process_t p{};
      p.steady_time = -1;
      p.frames_count = (uint32_t)inL.size();
      p.transport = &transport_;
      p.audio_inputs_count = 1;
      p.audio_inputs = &inBuf_;
      p.audio_outputs_count = 1;
      p.audio_outputs = &outBuf_;
      p.in_events = ev ? ev : emptyIn_.get();
      p.out_events = discardOut_.get();
      return inst.plugin->process(inst.plugin, &p);
   }

   void teardown() { inst.teardown(); }

private:
   float *inCh_[2] = {nullptr, nullptr};
   float *outCh_[2] = {nullptr, nullptr};
   clap_audio_buffer_t inBuf_{};
   clap_audio_buffer_t outBuf_{};
   clap_event_transport_t transport_{};
   clap_trap::EmptyInputEvents emptyIn_;
   clap_trap::DiscardOutputEvents discardOut_;
};

// RecvHost hosts wail-recv with its 16 stereo output ports (input ignored).
struct RecvHost {
   static constexpr int kPorts = 16; // RECV_SLOTS

   ClapInstance inst;
   std::vector<float> outL[kPorts], outR[kPorts];

   bool setup(const char *path, int ipcPort, double sampleRate, uint32_t block, std::string *err) {
      if (!inst.load(path, "software.wail.recv", ipcPort, sampleRate, block, block, err))
         return false;
      for (int p = 0; p < kPorts; p++) {
         outL[p].assign(block, 0.0f);
         outR[p].assign(block, 0.0f);
         ch_[p][0] = outL[p].data();
         ch_[p][1] = outR[p].data();
         outs_[p] = clap_audio_buffer_t{};
         outs_[p].channel_count = 2;
         outs_[p].data32 = ch_[p];
      }
      return true;
   }

   clap_process_status processBlock() {
      clap_process_t p{};
      p.steady_time = -1;
      p.frames_count = (uint32_t)outL[0].size();
      p.transport = nullptr; // recv never reads it
      p.audio_inputs_count = 0;
      p.audio_outputs_count = kPorts;
      p.audio_outputs = outs_;
      p.in_events = emptyIn_.get();
      p.out_events = discardOut_.get();
      return inst.plugin->process(inst.plugin, &p);
   }

   std::string portName(uint32_t idx) {
      auto *ap = (const clap_plugin_audio_ports_t *)inst.plugin->get_extension(inst.plugin, CLAP_EXT_AUDIO_PORTS);
      clap_audio_port_info_t info{};
      if (!ap || !ap->get(inst.plugin, idx, false, &info)) return {};
      return info.name;
   }

   // waitNameChange polls until port idx's name differs from prev (or timeout) —
   // observes IPC-thread-driven renames without a race.
   bool waitNameChange(uint32_t idx, const std::string &prev, int timeoutMs) {
      auto t0 = std::chrono::steady_clock::now();
      while (std::chrono::steady_clock::now() - t0 < std::chrono::milliseconds(timeoutMs)) {
         if (portName(idx) != prev) return true;
         wail_sleep_ms(5);
      }
      return false;
   }

   void teardown() { inst.teardown(); }

private:
   float *ch_[kPorts][2];
   clap_audio_buffer_t outs_[kPorts];
   clap_trap::EmptyInputEvents emptyIn_;
   clap_trap::DiscardOutputEvents discardOut_;
};

} // namespace wailtest
