# WAIL — WebRTC Audio Interchange for Link

WAIL synchronizes [Ableton Link](https://www.ableton.com/link/) sessions across the internet using WebRTC DataChannels. Musicians on different networks can sync tempo, phase, and interval boundaries as if they were on the same LAN. Intervalic audio (NINJAM-style) is captured, Opus-encoded, and transmitted over WebRTC DataChannels. A CLAP/VST3 plugin provides DAW integration.

## How it works

Each WAIL peer joins a local Ableton Link session and connects to a lightweight WebSocket signaling server to discover other peers. Peers then establish direct WebRTC connections with two DataChannels each:

- **sync** — JSON text messages for tempo, beat, phase, and clock synchronization
- **audio** — binary wire-format messages carrying Opus-encoded audio intervals

Audio uses a NINJAM-style double-buffer pattern: the plugin records the current interval from the DAW, and at the interval boundary the completed recording is Opus-encoded and sent to all peers. Remote intervals are decoded, mixed, and played back one interval behind — latency equals exactly one interval by design.

```
DAW A → [CLAP Plugin] → record → Opus encode → DataChannel → remote peer
                       ← play  ← Opus decode ← DataChannel ← remote peer
```

## Project structure

```
crates/
├── wail-core/        Core sync library (no networking)
├── wail-audio/       Audio encoding and intervalic ring buffer
├── wail-net/         WebRTC peer mesh and signaling client
├── wail-plugin/      CLAP/VST3 plugin (nih-plug)
├── wail-app/         CLI binary
└── wail-signaling/   WebSocket signaling server
```

## Build

Requires: **Rust 1.75+**, CMake 3.14+, a C++ compiler, and libopus-dev.

```sh
git submodule update --init --recursive   # fetch Ableton Link SDK
cargo build                               # build workspace
```

### Plugin

Install the bundler once, then use `cargo xtask`:

```sh
cargo install --git https://github.com/robbert-vdh/nih-plug.git cargo-nih-plug

cargo xtask build-plugin        # build CLAP + VST3 bundles → target/bundled/
cargo xtask install-plugin      # build and install to system plugin directories
cargo xtask install-plugin --no-build  # install already-built bundles
```

Plugin directories:
- **macOS** — `~/Library/Audio/Plug-Ins/{CLAP,VST3}/`
- **Linux** — `~/.clap/` and `~/.vst3/`
- **Windows** — `%COMMONPROGRAMFILES%\{CLAP,VST3}\`

### Running locally

```sh
cargo xtask run-signaling       # start signaling server on :9090

# two terminals — different IPC ports so they don't collide
cargo xtask run-peer                        # peer A (BPM 120, IPC 9191)
cargo xtask run-peer --bpm 96 --ipc-port 9192  # peer B
```

All `run-peer` flags map directly to `wail-app join` options (see `--help`).
Defaults: `--room test --server ws://localhost:9090 --bars 4 --quantum 4`.

## Testing

```sh
cargo test                    # all tests (~90 unit + integration)
cargo test -p wail-core       # core library tests
cargo test -p wail-audio      # audio codec, ring buffer, wire format
cargo test -p wail-net        # networking + WebRTC integration tests
```

## License

MIT
