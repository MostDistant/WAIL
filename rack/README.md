# Most Distant — VCV Rack plugin

Ableton Link bridge modules for [VCV Rack 2](https://vcvrack.com), living in the WAIL
repo so they can reuse WAIL's proven Ableton Link + Link Audio C wrapper
(`../plugins/linkbridge_link.{h,cpp}`, ADR-0007) and the pinned `../vendor/link`
(Link-4.0) submodule instead of re-vendoring the SDK.

## Modules

| Module | What it does |
| --- | --- |
| **Link Clock** | Joins the local Ableton Link session and turns its shared timeline into CV — a beat **Clock**, a bar **Reset**, and a **Run** gate driven by Link start/stop transport. Knobs: pulses-per-beat, quantum (beats/bar). |
| **Link Audio Receive** | Subscribes to a remote **Link Audio** channel and plays it into Rack (stereo out). |
| **Link Audio Send** | Publishes a Rack stereo input as a **Link Audio** channel other peers (WAIL, another Most Distant, …) can hear. |

## Status (Milestone 1)

M1 is the **scaffold + build wiring**, not the full DSP:

- All three modules build, load, and are **real Link peers** — the Ableton Link session
  is created per module instance (on the main thread, per the `lb_` API threading
  contract). **Link Clock** already derives clock/reset/run from the Link timeline.
- **Link Audio Receive/Send** join the session and (Send) announce a channel, but the
  PCM path is stubbed. **Milestone 2** is the real-time audio: channel discovery +
  selection, `lb_source`/`lb_sink` pop/commit, and 48 kHz ↔ Rack-engine-rate
  conversion (`rack::dsp::SampleRateConverter`). See the `// M2:` notes in each source.
- **Link Clock** captures the Link timeline throttled by a `ClockDivider` because
  `lb_capture()` allocates; M2 should move to a preallocated realtime-safe capture
  (WAIL ADR-0002) and interpolate the beat for sample-accurate edges.

## Build & test (macOS — Apple Silicon)

This plugin was scaffolded and its Ableton Link translation unit was compile-verified on
Linux, but the full Rack build/run happens on your Mac (the Rack GUI can't run in the
cloud container, and `vcvrack.com` is blocked by the container's egress policy, so the
SDK can't be downloaded there — do these steps locally):

```sh
# 1. Toolchain: Xcode Command Line Tools
xcode-select --install

# 2. Rack SDK (match the Rack version you run; latest is the 2.6.x line)
cd ~/Downloads
curl -LO https://vcvrack.com/downloads/Rack-SDK-2.6.6-mac-arm64.zip   # confirm exact version on the downloads page
unzip Rack-SDK-2.6.6-mac-arm64.zip
export RACK_DIR=~/Downloads/Rack-SDK

# 3. Make sure the Link submodule is present (once, in the WAIL repo)
git submodule update --init --recursive vendor/link

# 4. Build + install into Rack's user plugins folder, then launch Rack
cd <WAIL repo>/rack
make            # builds plugin.dylib
make install    # copies to ~/Library/Application Support/Rack2/plugins-mac-arm64/
```

Then launch Rack and add the **Most Distant** modules from the browser. To exercise Link,
run another Link peer on the LAN — Ableton Live, or WAIL itself (see the repo's
`scripts/pair-debug.sh` / test-tone tooling). With a peer playing, **Link Clock**'s
peer/playing lights light and it emits a clock.

- `make dist` packages a distributable `.vcvplugin`.
- Intel Macs: use the `-mac-x64` SDK; the Makefile's `ARCH_OS`/`ARCH_CPU` handle the rest.

## How the Link SDK is wired in

`Makefile` adds `../plugins/linkbridge_link.cpp` to `SOURCES` via `VPATH` (it `#include`s
`vendor/link`'s `abl_link.cpp`, i.e. the whole header-only Link 4.0 + Link Audio SDK — one
extra TU), sets the Link include paths and per-OS `LINK_PLATFORM_*` defines mirroring
`../plugins/CMakeLists.txt`, and forces `-std=c++17` (appended after `plugin.mk`, so it
wins over Rack's default `-std=c++11`). On Windows/MinGW the vendored `Channels.hpp`
`interface`-param rename is also required (untested here — see WAIL `DEVELOPMENT.md`).

## License

MIT — see [`LICENSE`](LICENSE).
