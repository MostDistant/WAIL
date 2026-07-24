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

### CLAP plugin integration tests (no DAW)

`plugins/tests/` hosts the built `wail-send`/`wail-recv` bridges via
[clap-trap](https://github.com/dfl/clap-trap) and plays the app's role on the
loopback IPC socket, verifying the wire contract (`wail-app/ipc.go`) end to end
without a DAW or the Go app:

```sh
cmake -S plugins -B build/plugins -DWAIL_PLUGIN_TESTS=ON   # fetches clap-trap
cmake --build build/plugins
ctest --test-dir build/plugins --output-on-failure
```

### Plugin-chain E2E (plugins ⇄ real apps ⇄ relay, no DAW)

`scripts/plugin-e2e.sh` goes one step further: the same clap-trap harness hosts
`wail-send`/`wail-recv`, but wired to **two real headless WAIL apps** in a local
relay room (one app per IPC port via `-instance`). A log sweep is pumped into
wail-send in real time; what returns on wail-recv port 0 is measured per second
(RMS + zero-crossing frequency, same scheme as `linkaudio-probe`). Exit 0 = PASS.
Like tier2, it builds the app with cgo and runs in real time (~1 min); manual, not CI.

```sh
scripts/plugin-e2e.sh            # PLUGE2E_KEEP=1 keeps app/chain logs on exit
```

### Tier 2 audio E2E (real Link Audio path, no DAW)

`go test` runs entirely in-process — it never exercises the real Link Audio
Sink/Source UDP path, the relay round trip, or real-time timing. `scripts/tier2-e2e.sh`
covers that gap on a single machine, no DAW required:

```sh
./scripts/tier2-e2e.sh          # exit 0 = PASS, 1 = FAIL
```

It stands up a **local relay**, runs two headless WAIL instances against it — a
`SweepSender` that injects a test WAV, and a `Receiver` that pulls the stream from
the relay and republishes it as a real Link Audio channel — then runs
`linkaudio-probe` (a DAW-free Link Audio consumer) to subscribe to that channel and
measure what actually arrives.

Two modes (`TIER2_MODE`):

- **`step` (default)** — the WAV is stepped tones: one constant frequency per
  interval-length block (`gen-sweep -block`), so received audio identifies *which*
  content block is playing. On top of the integrity checks this verifies **NINJAM
  interval placement** end-to-end: content captured in room interval `N` must play
  on the receiver during room interval `N+D`. Ground truth is wall-clock
  correlated (absolute grid indices are per-peer — ADR-0003): the sender logs a
  `(room, content-seconds)` marker per boundary (`[wav-sender] boundary room=…
  content=…s`), the receiver's `>>> INTERVAL` log marks which room interval
  started playing when, and every received second is asserted
  `captureRoom == playingRoom − D`.
- **`sweep`** — the original rising log sweep: a PASS means non-silent,
  **lossless** audio whose estimated frequency **climbs with the sweep** (proof
  it's intact and in order).

Tunables: `TIER2_PORT`, `TIER2_BPM`, `TIER2_SWEEP_DUR`, `TIER2_PROBE_SECS`,
`TIER2_ROOM`, `TIER2_MODE`, `TIER2_BPI`, `TIER2_D`. Note the room may not
actually run at `TIER2_BPM` — the relay anchor can race the founder's seed at
join, and entry conformance then adopts the anchor tempo; the step mode's
placement check is exact for any room tempo.

The instances point at the local relay via the `WAIL_SIGNAL_URL` env var (e.g.
`WAIL_SIGNAL_URL=ws://localhost:8899`), which overrides the default production
relay — useful on its own for running headless against a self-hosted relay.

For a full hardware run, point a Link-Audio DAW (Ableton Live 12.3+) at two
machines instead of the probe; the emit path is identical.

A single-instance variant uses the server-echo loopback: run one headless WAIL
with `-wav <file> -loopback` against a local relay and probe the republished
`(loopback)` channel — the full encode → relay → decode → playout round trip in
one process. `cmd/gen-complex` generates dense music-like test material (detuned
pad, bass, percussive transients) that stresses the codec path harder than the
sweep; `TestComplexProgramRoundTrip` runs the same material through the streaming
encode/decode path in-process and asserts it comes back gap-free.
