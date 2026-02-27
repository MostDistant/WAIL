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
├── wail-plugin/      CLAP/VST3 plugin (nih-plug, built separately)
├── wail-app/         CLI binary
└── wail-signaling/   WebSocket signaling server
```

## Build

Requires: **Rust 1.75+**, CMake 3.14+, a C++ compiler, and libopus-dev.

```sh
git submodule update --init --recursive   # fetch Ableton Link SDK
cargo build                               # build workspace
cargo run -p wail-signaling               # start signaling server on :9090
cargo run -p wail-app -- join --room test --server ws://localhost:9090
```

Build the CLAP/VST3 plugin (separate from the workspace):

```sh
cargo build -p wail-plugin --release
```

## Testing

```sh
cargo test                    # all tests (~90 unit + integration)
cargo test -p wail-core       # core library tests
cargo test -p wail-audio      # audio codec, ring buffer, wire format
cargo test -p wail-net        # networking + WebRTC integration tests
```

## License

MIT
