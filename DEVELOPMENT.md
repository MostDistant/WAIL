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
└── interval_clock.go    Room tempo/BPI state + interval_anchor broadcast (joiner seeding, ADR-0009)

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

### Debug room + offset analysis (latency hunting, no DAW needed)

For measuring a peer's rhythmic phase offset against the room grid (the
"GerenM felt 90ms off" class of problem):

- **GUI**: turn on **Developer mode** in settings, then press **Debug Room**
  on the join screen. It joins the shared `wail-debug` room and arms all
  diagnostics: the WAIL Metronome broadcast (a grid-rendered reference click),
  server-echo loopback, and peer log sharing (the room collates everyone's
  logs into each client).
- **`-debug`**: the same arming from the command line, GUI or headless —
  `wail-app -debug`. This is the one to hand a tester: it needs no room name
  and no settings change, and every debug session then carries the same
  diagnostics, so captures are comparable across machines.
  - `-debug` **implies developer mode** for that run, so the Debug tab (stream
    offsets, cushion) is there without talking anyone through a checkbox. The
    saved preference is left alone — relaunching without the flag restores it —
    and unticking Developer mode in settings still wins for the session.
  - Add `-room NAME` for a private debug room instead of the shared one
    (`wail-app -debug -room debug-geren`) when you don't want testers landing
    on top of each other's audio.
  - Collect with `wail-logstore -room wail-debug` (see **Log store** below)
    while the tester is connected.
- **Individually**: `-metronome-broadcast`, `-loopback` if you want one
  diagnostic rather than the set.

> `-debug` and the Debug Room button share the room's log stream, which
> carries Link Audio channel names — i.e. the tester's DAW track names — to
> everyone in the room. Both paths say so in the log when they arm. Worth
> mentioning to testers before pointing them at the shared room.
- **In-app readout (DAW-less)**: the Debug tab's *Stream offsets* section
  shows each remote stream's measured phase offset vs the room grid in ms
  (`internal/offset`, computed from labeled WAIF frames — exact, no envelope
  matching). |offset| > 25ms is highlighted. A peer performing late because
  their monitoring path is latent shows it directly.
- **Probe**: `linkaudio-probe -offset-ref "Metronome"` cross-correlates
  each channel's envelope against the reference metronome (best for
  same-period rhythmic content), and `-offset-dump <dir>` writes per-channel
  RMS envelopes as CSVs for offline analysis.

### Log store (after-the-fact session forensics)

Fly keeps relay logs for about seven days, but `flyctl logs --no-tail` returns
only a short buffer — so by the time a jam is over and someone reports "it
sounded crinkly around 10:42", the evidence is usually already out of reach.
`wail-logstore` pulls the relay's logs from Fly's *paginated* logs API, which
does reach back across the whole retention window, into a local SQLite DB:

```sh
cd signaling-server && go build -o wail-logstore ./cmd/wail-logstore

./wail-logstore -db wail-logs.db                    # backfill 7 days of relay logs
./wail-logstore -db wail-logs.db -since 24h -follow # a day, then keep pulling
./wail-logstore -room synthseeker -password PW      # relay + that room's peer logs
```

The Fly token comes from `-token`, `$FLY_API_TOKEN`, or `flyctl auth token`.
Re-running is idempotent (rows dedupe on time+source+origin+message), so it is
safe to point at the same DB repeatedly, and overlapping windows cost nothing.

Adding `-room` also records the room's peer-shared logs into the same table, so
both sides land on one timeline and correlating a relay event against a peer's
audio log is a query:

```sql
-- what the relay was doing either side of a peer's capture glitch
SELECT ts, source, origin, message FROM logs
WHERE ts_ns BETWEEN :t - 60e9 AND :t + 60e9 ORDER BY ts_ns;
```

Two things worth knowing. Peer logs **cannot** be backfilled — they only exist
while someone is in the room, so `-room` has to be running during the jam,
whereas relay logs can be collected days later. And peer rows are stamped on
arrival at the recorder, because the relay does not carry a peer's wall clock
and peer clocks disagree anyway; one receiver's arrival order is the only
timeline all sources can share.

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

`plugins/tests/` hosts the built WAIL Send / WAIL Receive plugins via
[clap-trap](https://github.com/dfl/clap-trap) alongside a second in-process Link
peer, verifying publish/subscribe and the room-channel filter without a DAW or
the Go app:

```sh
cmake -S plugins -B build/plugins -DWAIL_PLUGIN_TESTS=ON   # fetches clap-trap
cmake --build build/plugins
ctest --test-dir build/plugins --output-on-failure
```

### Mini-DAW E2E (WAIL Receive ⇄ real app ⇄ relay, no DAW)

`scripts/minidaw-e2e.sh` goes one step further: the same clap-trap harness hosts
`wail-recv` as a mini-DAW, wired to a **real headless WAIL app** in a
local relay room. The app broadcasts its metronome with `-loopback`, so the click
comes back through the relay and is republished as a `WAIL · …` channel; the
harness measures each click's onset against the session grid and PASSes when the
median offset is within `MDE2E_THRESHOLD_MS` (default 5 ms). Like tier2, it builds
the app with cgo and runs in real time (~1 min); manual, not CI.

```sh
scripts/minidaw-e2e.sh
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
  content block is playing. On top of the integrity checks this verifies **NINJAM-like
  interval placement** end-to-end: content captured in room interval `N` must play
  on the receiver during room interval `N+D`. Ground truth is wall-clock
  correlated (absolute grid indices are per-peer — ADR-0003): the sender logs a
  `(room, content-seconds)` marker per boundary (`[wav-sender] boundary room=…
  content=…s`), the receiver's `>>> INTERVAL` log marks which room interval
  started playing when, and each receiver interval's **majority-vote capture
  room** (per-second tone classification voting by content range; skewed or
  mixed-tone seconds lose the vote, so no edge margins are needed) is asserted
  `== playingRoom − D`.
- **`sweep`** — the original rising log sweep: a PASS means non-silent,
  **lossless** audio whose estimated frequency **climbs with the sweep** (proof
  it's intact and in order).

Tunables: `TIER2_PORT`, `TIER2_BPM`, `TIER2_SWEEP_DUR`, `TIER2_PROBE_SECS`,
`TIER2_ROOM`, `TIER2_MODE`, `TIER2_BPI`, `TIER2_D`. Note the room may not
actually run at `TIER2_BPM`: WAIL joins the LAN's Link session, and if that
session already has peers at a different tempo, Link convergence pulls WAIL to
the *LAN* tempo (pillar 4: the local Link session is authoritative) and the room
follows it. The step mode's placement check is exact for any room tempo.

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

## Beta channel

Main is the beta channel. Every merge to `main` with a `feat:`/`fix:` (or a
changeset) is cut as a prerelease `v4.2.0-beta.N` on the long-lived `beta`
branch and published as a GitHub **prerelease** with the same platform artifacts
as stable. Stable is a separate, deliberate promotion: merging the standing
`release` PR ships `v4.2.0`. See `docs/adr/0008-beta-channel.md` for the full
model; this section is how a tester installs and runs a beta.

Betas are a small, invite-only circle — they share the **production relay and
real rooms** with stable users, so keep them among people you've asked. A beta
build self-identifies: the version in the app header and Debug tab reads
`v4.2.0-beta.N` (zero-indexed — the first beta of a cycle is `beta.0`).

The numbering is checked without cutting a release by
`scripts/verify-beta-versioning.sh` (drives the real `prepare-beta` against a
throwaway repo; `verify-release-config.yml` runs it in CI on release-config
changes). It needs `knope` on `PATH`; grab the binary from
[knope releases](https://github.com/knope-dev/knope/releases) to run it locally.

### macOS

Betas ship a **separate `wail-beta` formula** in the same tap, so stable `wail`
stays installed and you flip between them with `brew link` — no rebuild either
way. Both build the same binary from source (Go + cgo Link + CLAP plugins, a few
minutes, needs the Xcode command-line tools).

```sh
brew install MostDistant/wail/wail-beta
```

If stable `wail` is already installed, that command finishes the build and then
prints a "Could not symlink bin/wail" error — **expected**, because both
formulae install `bin/wail` and only one can be linked at a time. Switch which
one is on your `PATH`:

```sh
brew unlink wail && brew link --overwrite wail-beta   # run the beta
brew unlink wail-beta && brew link --overwrite wail   # back to stable, instantly
```

`brew upgrade wail-beta` rebuilds the newest beta from source. Both kegs stay on
disk, so falling back to stable mid-session is a `link`, not a reinstall.

Beta prereleases also ship the same `wail-macos-arm64-<version>.dmg` as stable
(Apple Silicon, ad-hoc signed — right-click → Open on first launch). Dragging
the beta WAIL.app into Applications replaces any stable .app install; keep it
somewhere else (e.g. `~/Applications`) to run both.

### Windows

Download the `wail-windows-x64-<version>.zip` asset from the beta's prerelease on
the Releases page, unzip it anywhere (alongside a stable copy is fine — they're
just folders), and run `bin\wail.exe`. Same unsigned-binary SmartScreen prompt as
stable ("More info" → "Run anyway").

### Data and identity

A beta shares the stable data dir (`~/.wail`): same identity, stream names, and
capture selections, so you're testing your real setup and the room sees your
usual peer identity. (`-instance N` still gives an isolated `~/.wail-N` if you
want one.)

> **Plugin caveat (until the follow-up ships):** the CLAP bridge plugins
> auto-install only *if missing*, so a machine that already has stable's plugins
> keeps running them under a beta app — beta plugin changes won't reach a DAW
> that already has the bundles. A forced-reinstall control (a Debug-tab button /
> `-install-plugins` flag / `wail-beta-install-plugins`) lands with the app PR;
> until then, to test beta plugins, delete the `wail-send.clap` / `wail-recv.clap`
> bundles from your CLAP folder and relaunch the beta so it reinstalls its own.

### Crash evidence (follow-up)

The app PR removes the crash reporter and starts writing a panic stack to
`~/.wail/logs/crash.log`. Until then, the rotating `wail.log` and the room-shared
log are what to collect — "send me your logs" means those.

