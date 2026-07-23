# WAIL CLAP plugins

Thin CLAP bridge plugins for DAWs that don't speak Ableton Link Audio (ADR-0005).
They are **raw-PCM transports only** — the WAIL app owns all Opus/WAIF/interval/relay
logic. Each plugin talks to the running WAIL app over loopback TCP (`127.0.0.1:9191`,
or `WAIL_IPC_ADDR`).

- **`wail-send`** — insert on a track/bus. Passes audio through and taps a copy to
  WAIL. One CLAP param, *Stream Index* (0–15), lets you run several instances as
  separate streams (drums, synth, …).
- **`wail-recv`** — 16 stereo output ports, one per remote stream. Ports are named
  live (`{peer} · {stream}`) as audio arrives.

Both implement `clap.state` (Stream Index persists in project files for send; recv
saves a version marker), so hosts can save projects/presets containing them.

## Build

Requires a C11 compiler, CMake ≥ 3.15, and the vendored CLAP headers:

```sh
git submodule update --init vendor/clap
cmake -S plugins -B build/plugins -DCMAKE_BUILD_TYPE=Release
cmake --build build/plugins
ctest --test-dir build/plugins --output-on-failure   # clap.state roundtrip tests
```

Outputs `wail-send.clap` and `wail-recv.clap` in the build dir:
- **macOS** — a `.clap` bundle (`Contents/MacOS/<name>` + `Info.plist`).
- **Linux / Windows** — a single `.clap` shared object / DLL.

No `libopus` or other companion libraries: the plugin has no codec, so the Windows
`.clap` is a lone DLL (unlike the retired plugin era).

## Realtime discipline (ADR-0002)

`process()` only does lock-free SPSC ring reads/writes + memcpy — never a syscall,
lock, or allocation. All socket I/O runs on a dedicated IPC thread that owns the
connection and reconnects on drop.

## Testing

DAW-less integration tests live in `tests/` (opt-in; fetches
[clap-trap](https://github.com/dfl/clap-trap) at configure time):

```sh
cmake -S plugins -B build/plugins -DWAIL_PLUGIN_TESTS=ON
cmake --build build/plugins
ctest --test-dir build/plugins --output-on-failure   # add -C Release for multi-config
```

`wail-plugin-tests` hosts each built plugin via clap-trap's `TestHost` and plays the
WAIL app's role on the loopback IPC socket (`tests/ipc_test_server.h` mirrors
`wail-app/ipc.go`), so the plugin ⇄ app contract is exercised without a DAW or the
Go app: RawPCM framing/PCM exactness, transport flag, Stream Index param, stream →
port routing, port naming + rescan, StreamGone, mono duplication, underrun silence,
slot exhaustion, and no-server resilience. `test_state` covers the clap.state
roundtrip. Both run in CI.

`wail-plugin-chain` (same build) is the driver for `scripts/plugin-e2e.sh`: it hosts
both plugins wired to two real headless WAIL apps over a local relay and sweeps
audio through the full Opus/WAIF/playout path — the DAW-less end-to-end check for
the bridge (manual, ~1 min, like `scripts/tier2-e2e.sh`).

## Status

Compile-, link-, bundle-, and IPC-behavior-verified in CI (see Testing). A live-room
sanity check in a real Link-Audio-less DAW (e.g. Reaper, Bitwig) is still worth doing
once on each platform, but day-to-day plugin changes are covered without one.
