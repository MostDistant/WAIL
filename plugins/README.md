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

## Build

Requires a C11 compiler, CMake ≥ 3.15, and the vendored CLAP headers:

```sh
git submodule update --init vendor/clap
cmake -S plugins -B build/plugins -DCMAKE_BUILD_TYPE=Release
cmake --build build/plugins
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

## Status

Compile-, link-, and bundle-verified in CI. End-to-end audio behavior (loading in a
CLAP host and confirming a live room) must be validated on a machine with a
Link-Audio-less DAW (e.g. Reaper, Bitwig) running alongside the WAIL app.
