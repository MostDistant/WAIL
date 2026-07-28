# WAIL CLAP plugins

The **Link Bridge** plugins (ADR-0007), for DAWs that are native Link *sync* peers but
lack Link Audio (Live pre-12.3, Bitwig-class hosts). Each instance joins the LAN Link
session as its own audio-only peer and speaks Link Audio directly, so the WAIL app
needs no changes and no room intelligence (codec, intervals, relay, room clock) ever
enters a plugin.

- **`wail-linkbridge-send`** — publishes the track as a Link Audio channel named from
  the DAW track name (host `clap.track-info`).
- **`wail-linkbridge-recv`** — 16 stereo ports subscribed to the room-published
  `WAIL · {peer} · {stream}` channels, auto-named as peers arrive and renamed live
  (CLAP `RESCAN_NAMES`) when the app relabels a stream.

Neither has any configuration to persist, but both implement `clap.state` with a
version marker — Bitwig refuses to save a project containing a plugin that can't.

## Build

Requires a C11/C++17 compiler, CMake ≥ 3.15, the vendored CLAP headers, and the
vendored Link SDK:

```sh
git submodule update --init vendor/clap
git submodule update --init --recursive vendor/link
cmake -S plugins -B build/plugins -DCMAKE_BUILD_TYPE=Release
cmake --build build/plugins
```

Outputs the two product bundles in the build dir — `wail-linkbridge-send.clap` and
`wail-linkbridge-recv.clap` (the `transport-probe` and `linkbridge-spike` targets
build alongside but are dev tools and are never shipped):
- **macOS** — a `.clap` bundle (`Contents/MacOS/<name>` + `Info.plist`).
- **Linux / Windows** — a single `.clap` shared object / DLL.

No `libopus` or other companion libraries: the plugin has no codec, so the Windows
`.clap` is a lone DLL.

## Realtime discipline (ADR-0002)

`process()` only does lock-free SPSC ring reads/writes + memcpy — never a syscall,
lock, or allocation. Channel discovery, subscription, and port renaming run on a
dedicated manager thread that polls the Link session off the audio thread.

## Testing

DAW-less integration tests live in `tests/` (opt-in; fetches
[clap-trap](https://github.com/dfl/clap-trap) at configure time):

```sh
cmake -S plugins -B build/plugins -DWAIL_PLUGIN_TESTS=ON
cmake --build build/plugins
ctest --test-dir build/plugins --output-on-failure   # add -C Release for multi-config
```

`wail-plugin-tests` hosts each built plugin via clap-trap's `TestHost` alongside a
second in-process Link peer, so publish/subscribe behaviour is exercised without a
DAW: the spike hosts and logs, Send's channel round-trips to a subscriber, and Recv
subscribes only to room-published (`WAIL · `-prefixed) channels and renders their
audio, follows an in-place channel rename onto the same port, resubscribes after a
host bypass/re-enable, and round-trips `clap.state`. Runs in CI.

`minidaw` (same build) is the driver for `scripts/minidaw-e2e.sh`: it hosts
`wail-linkbridge-recv` against a real headless WAIL app over a local relay and checks
audio through the full Opus/WAIF/playout path — the DAW-less end-to-end check for the
bridge (manual, ~1 min, like `scripts/tier2-e2e.sh`).

## Status

Compile-, link-, bundle-, and Link-Audio-behaviour-verified in CI (see Testing). A
live-room sanity check in a real Link-Audio-less DAW (e.g. Reaper, Bitwig) is still
worth doing once on each platform, but day-to-day plugin changes are covered without
one.
