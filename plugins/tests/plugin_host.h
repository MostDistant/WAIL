// Shared CLAP hosting helpers for the WAIL plugin test binaries: load a plugin
// via clap-trap and drive its lifecycle. Used by the test runner
// (test_plugins.cpp) and the mini-DAW driver (minidaw_main.cpp). No
// test-framework dependencies — errors are returned, not asserted.
#pragma once

#include <chrono>
#include <cstdint>
#include <cstdio>
#include <functional>
#include <memory>
#include <string>
#include <vector>

#include <clap-trap/clap-trap.h>

#include "wail_thread.h" // wail_sleep_ms

namespace wailtest {

// ClapInstance owns one loaded+activated plugin (loader lifetime, host, lifecycle).
struct ClapInstance {
   std::unique_ptr<clap_trap::PluginLoader> loader;
   std::unique_ptr<clap_trap::TestHost> host;
   const clap_plugin_t *plugin = nullptr;

   // load loads/activates/starts the plugin. configureHost (optional) runs
   // after the TestHost is created but before create_plugin/init, so a test can
   // serve host extensions (e.g. clap.audio-ports) via setExtensionCallback.
   // Returns true on success; err receives a message on failure.
   bool load(const char *path, const char *pluginID, double sampleRate,
             uint32_t minFrames, uint32_t maxFrames, std::string *err,
             const std::function<void(clap_trap::TestHost *)> &configureHost = nullptr) {
      loader = clap_trap::PluginLoader::load(path);
      if (!loader || !loader->factory()) {
         if (err) *err = "load failed: " + (loader ? loader->getError() : std::string("null loader"));
         return false;
      }
      const clap_plugin_factory_t *factory = loader->factory();
      host = std::make_unique<clap_trap::TestHost>();
      if (configureHost) configureHost(host.get());
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

} // namespace wailtest
