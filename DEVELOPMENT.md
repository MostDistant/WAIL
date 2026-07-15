# Development

## How It Works

Each WAIL peer joins a local Ableton Link session and connects to a lightweight WebSocket relay server to join a room. All sync and audio data flows through the relay to every peer in the room:

- **sync** — JSON text messages for tempo, beat, phase, clock, and the relay's `interval_anchor` (the authoritative room interval clock)
- **audio** — binary WAIF frames carrying Opus-encoded audio intervals

WAIL is an Ableton Link Audio peer, so the whole audio path runs inside `wail-app` — no plugins, no IPC. Capture subscribes to your local Link Audio channels, buckets the samples into NINJAM-style intervals, Opus-encodes them, and relays them. Playback decodes remote intervals, holds each one until the interval boundary (offset D, default 1), and republishes it as a Link Audio channel — latency equals the offset by design.

```
Link Audio (local channels)     → capture → interval → Opus → WAIF → WS relay → remote peer
Link Audio (published channels) ← emit/sink ← hold N+D ← reassemble ← Opus ← WAIF ← WS relay ← remote peer
```

## Project Structure

```
wail-app/                Go/Wails desktop app: session orchestration, Link + Link
│                        Audio, Opus codec, relay client, GUI. Needs cgo.
├── audio_engine*.go     Link Audio capture + emit engine (stub under -tags linkstub)
├── interval_codec.go    Interval Opus↔WAIF codec
├── link_*.go            Ableton Link sync bridge + poller
├── session.go           Session state machine
├── signaling.go         WebSocket signaling/relay client
├── wire.go              WAIF binary wire format
└── internal/
    ├── abllink/         cgo binding to Ableton Link (abl_link C API: sync + Link
    │                    Audio); capture.c holds the pure-C realtime callback + ring
    ├── interval/        Interval/room-clock math (RoomClock, RoomLabeler)
    ├── playout/         Hold-until-N+D playout scheduler
    ├── lanloss/         Link Audio count-gap loss detection
    ├── affinity/        (identity, stream) → stable published channel
    ├── capture/         Interval assembler
    └── emit/            Reassembler + paced sink reader

signaling-server/        Go WebSocket relay (SQLite, deployed to fly.io)
├── main.go              Relay + room management
├── roomclock.go         Relay-authoritative room interval clock
└── interval_clock.go    interval_anchor broadcast

vendor/
└── link/                Ableton Link 4.0 SDK (git submodule, pinned to Link-4.0)
```

## Build from Source

Requires: **Go 1.26+**, a C++ toolchain (cgo, for the Link Audio binding in `internal/abllink`), and libopus. CMake is **not** required.

**Linux build dependencies (Debian/Ubuntu):**

```sh
sudo apt-get install libopus-dev pkg-config g++
```

```sh
git submodule update --init --recursive vendor/link   # fetch Link SDK + its asio submodule

cd wail-app && go build                                # build the app (needs cgo)
cd signaling-server && go build ./...                  # build the relay server
```

Building without the Link SDK (Link becomes a stub, no audio path):

```sh
cd wail-app && go build -tags linkstub
```

**Windows (MinGW) only:** MinGW's COM headers `#define interface`, which collides with a parameter named `interface` in Link's `link_audio/Channels.hpp`. Before building on Windows, rename it:

```sh
sed -i 's/\binterface\b/iface/g' vendor/link/include/ableton/link_audio/Channels.hpp
```

(The Windows CI jobs do this automatically; macOS/Linux don't need it.)

### Desktop App (dev mode)

```sh
bin/dev            # fetch the Link submodule + run the Go/Wails app in dev mode
bin/dev-headless   # same, but headless (CLI) mode
```

## Testing

```sh
cd wail-app && go test ./...                    # app + internal package tests
cd wail-app && go test ./internal/interval      # a single package
cd wail-app && go test -tags linkstub ./...     # without the Link SDK (no audio path)
cd signaling-server && go test ./...            # relay server
```

Building or testing the audio path needs cgo (a C++ toolchain + libopus) and GOCACHE write access; in a sandbox you may need to disable it.
