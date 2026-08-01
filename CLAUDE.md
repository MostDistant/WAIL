# WAIL - WebSocket Audio Interchange for Link

## What is this?

WAIL synchronizes Ableton Link sessions across the internet using a WebSocket relay server. Musicians on different networks can sync tempo, phase, and interval boundaries as if they were on the same LAN. WAIL participates as an Ableton **Link Audio** peer: it subscribes to your local Link Audio channels, captures NINJAM-style intervals, Opus-encodes them, and ships them over the relay; on the receiving side it decodes remote intervals, holds them until the interval boundary, and republishes them as Link Audio channels. There are no plugins to install — any Link-Audio-capable app (e.g. Ableton Live 12.3+) works with WAIL.

WAIL is a single-language project: two Go modules, `wail-app/` (the desktop app) and `signaling-server/` (the relay).

## Project Structure

```
wail-app/                Go/Wails desktop app: session orchestration, Ableton Link
│                        + Link Audio, Opus codec, relay client, GUI. Needs cgo.
├── main.go               Entry point, Wails window setup, CLI flags (headless, --wav)
├── app.go                Frontend-callable methods (JoinRoom, Disconnect, SetTestTone, …)
├── session.go            Session state machine (goroutine-based select loop)
├── signaling.go          WebSocket signaling/relay client + PeerMesh
├── peers.go              PeerRegistry
├── link_real.go          Ableton Link sync bridge (via internal/abllink; !linkstub)
├── link_stub.go          Link stub for testing (-tags=linkstub)
├── link_types.go         Link types, poller, echo guard, tempo detector
├── audio_engine.go       AudioEngine interface (Link Audio capture + emit path)
├── audio_engine_real.go  Capture + emit engine (//go:build !linkstub)
├── audio_engine_stub.go  No-op AudioEngine under -tags linkstub (no audio path)
├── interval_codec.go     Interval Opus↔WAIF codec (encode/decode + PLC, loopback-tested)
├── capture_dump.go       Debug GUI toggle: dump capture audio pre/post-Opus to WAV
├── clock.go              NTP-style RTT/clock sync
├── protocol.go           SyncMessage + SignalMessage types (incl. interval_anchor)
├── wire.go               WAIF binary wire format
├── test_tone.go          Test tone generator (Opus sine wave) — GUI/headless injection
├── wav_sender.go         Headless WAV file sender (--wav)
├── metronome_sender.go   In-app sender that broadcasts the WAIL Metronome click to the room
├── packet_loss.go        WAN relay loss detection (WAIF frame sequence)
├── recorder.go           Local session recording (WAIF frames to disk)
├── events.go             Frontend event types
├── stream_names.go       Persistent per-stream name storage
├── filelog.go            Rotating file logger
├── wslog.go              WebSocket log broadcaster
├── honeybadger.go        Honeybadger crash reporting
├── internal/
│   ├── abllink/          cgo binding to Ableton Link's abl_link C API (sync + Link
│   │                     Audio), compiled against vendor/link. capture.c holds the
│   │                     pure-C capture callback + lock-free ring (ADR-0002: the
│   │                     realtime callback is never a Go callback); sink.go/source.go
│   │                     publish/subscribe Link Audio channels.
│   ├── interval/         Interval/room-clock math: local↔room index mapping,
│   │                     RoomClock, RoomLabeler (ADR-0003)
│   ├── align/            Grid steer (ADR-0006): entry conformance, gated grid
│   │                     slew, snapshot-tempo arbitration, committed-tempo record
│   ├── playout/          Hold-until-N+D playout scheduler (interval offset D, default 1)
│   ├── lanloss/          Link Audio count-gap loss detection (LAN capture hop)
│   ├── affinity/         (identity, stream) → stable published Link Audio channel
│   ├── capture/          Interval assembler: sample-contiguous placement + micro-slew
│   │                     drift correction (buckets capture buffers into intervals)
│   └── emit/             Reassembler (PLC-aware) + cushioned sink feeder (playback side)
└── frontend/             Bundled web UI (HTML/JS/CSS)

signaling-server/         Go WebSocket relay server (deployed to fly.io)
├── main.go               Relay + room management (SQLite)
├── roomclock.go          Relay-authoritative room interval clock (ADR-0003)
├── interval_clock.go     interval_anchor broadcast
├── labelwatch.go         Label watchdog: heals peers whose room-label offset froze
│                         wrong (WAIF label vs room index → unicast fresh anchor)
├── cmd/wail-metrics/     CLI metrics client
├── cmd/wail-logtail/     Tails a room's peer-shared logs live (joins as an observer)
└── cmd/wail-logstore/    Backfills relay logs from Fly's logs API (~7 days) into a
                          local SQLite DB, optionally alongside a room's peer logs —
                          one timeline for both sides. See DEVELOPMENT.md → "Log store"

plugins/                  WAIL Send / WAIL Receive CLAP plugins for DAWs without Link Audio (ADR-0007).
│                         Each instance is its own LAN Link Audio peer; the app is unchanged.
├── wail_send.c           Publishes the track as a Link Audio channel (named from track-info)
├── wail_recv.c           Subscribes to room-published "WAIL · " channels → 16 stereo ports
├── wail_link.{h,cpp}     C-facing Link session/audio API (mirrors internal/abllink/wrap.cpp)
├── wail_thread.h         Thread/mutex/sleep shim (Win32 API vs pthreads)
├── tests/                DAW-less harness: clap-trap host + a second in-process Link peer
│                         (ctest via -DWAIL_PLUGIN_TESTS=ON) + minidaw E2E driver
│                         (scripts/minidaw-e2e.sh: recv plugin ⇄ real app ⇄ relay)
└── CMakeLists.txt        Builds both .clap bundles

vendor/
├── link/                 Ableton Link 4.0 SDK (git submodule, pinned to Link-4.0)
└── clap/                 CLAP SDK headers (git submodule, pinned to 1.2.10) for the bridge plugins
```

## Build

Requires: Go 1.26+, a C++ toolchain (cgo, for the Link Audio binding in `internal/abllink`), and libopus + pkg-config (Opus via cgo). CMake is **not** required.

```sh
git submodule update --init --recursive vendor/link   # fetch Link SDK + its asio submodule
git submodule update --init vendor/clap                # CLAP headers (for the bridge plugins)

cd wail-app && go build                                # build the app (needs cgo)
cd wail-app && go test ./...                           # run app + internal package tests

# Build/test WITHOUT the Link SDK (Link becomes a stub; no audio path).
# ALWAYS -o /dev/null for the stub compile check: a bare `go build -tags linkstub`
# overwrites ./wail-app with a DEAF binary (no Link, no audio) that looks identical.
cd wail-app && go build -tags linkstub -o /dev/null
cd wail-app && go test -tags linkstub ./...

# Signaling server
cd signaling-server && go build ./... && go test ./...

# WAIL Send / WAIL Receive CLAP plugins (ADR-0007) — optional, for DAWs without Link Audio;
# needs vendor/clap and vendor/link
cmake -S plugins -B build/plugins && cmake --build build/plugins

# Plugin integration tests (DAW-less: clap-trap host + a second Link peer; fetches clap-trap)
cmake -S plugins -B build/plugins -DWAIL_PLUGIN_TESTS=ON && cmake --build build/plugins
ctest --test-dir build/plugins --output-on-failure
```

Note: Go needs GOCACHE write access; if you build inside a sandbox you may need to disable it.

## Key Dependencies

### Go (wail-app)
- `wailsapp/wails/v3` - Desktop app framework (webview)
- `gorilla/websocket` - WebSocket client (signaling + audio relay)
- `internal/abllink` - our own cgo binding to Ableton Link's `abl_link` C API (sync + Link Audio), compiled directly against `vendor/link`. Replaced the external `abletonlink-go` dependency.
- `hraban/opus.v2` - Opus codec (cgo, libopus)
- `google/uuid`, `go-audio/wav` - IDs and headless WAV loading

### Go (signaling-server)
- `gorilla/websocket` - WebSocket relay + room management; SQLite for room storage

## Architecture

### Sync Flow
Each WAIL peer:
1. Joins local Ableton Link session (LAN multicast)
2. Connects to WebSocket relay server to join a room (public or password-protected)
3. Sync messages (tempo, phase, clock) are relayed through the server to all room peers
4. Polls Link at 50Hz, broadcasts tempo/phase changes
5. Applies remote tempo changes to local Link session
6. The relay owns the authoritative room interval clock and broadcasts an `interval_anchor`; each peer maps its local interval index to the shared room index (ADR-0003)

### Audio Flow (Link Audio)
WAIL is an Ableton Link Audio peer. There are no plugins and no IPC — the whole audio path runs inside `wail-app`.

**Capture (send side):**
1. Subscribe to local Link Audio channels (`LinkAudioSource`). The realtime capture callback is pure C (`internal/abllink/capture.c`) and pushes buffers into a lock-free ring; a Go goroutine drains it off-thread (ADR-0002 invariant: never a Go callback on the audio thread).
2. `internal/capture` buckets buffers into fixed-length NINJAM intervals (local index), emitting each 20ms window as its audio arrives (the interval is a playout concept, not a transmission one).
3. `interval_codec.go` Opus-encodes each window into a WAIF frame as it fills.
4. Frames stream over the WebSocket relay (binary) to all room peers in real time during the interval.

**Playback (recv side):**
1. Receive WAIF frames from the relay.
2. `interval_codec.go` Opus-decodes; `internal/emit` reassembles frames into interval PCM.
3. `internal/playout` holds each interval until the local boundary labeled N+D (interval offset D, default 1).
4. `internal/emit` paces the interval into a Link Audio sink (`LinkAudioSink`); `internal/affinity` keeps a reconnecting peer's streams on stable channels.
5. WAIL republishes remote streams as Link Audio channels — any Link-Audio-capable app plays them.

Latency = the interval offset D (default 1 interval), by design like NINJAM.

Two WebSocket message types via the relay server:
- **sync** (text): JSON messages relayed to all room peers (tempo, beat, phase, clock sync, `interval_anchor`)
- **audio** (binary): WAIF wire-format frames broadcast to all room peers (Opus-encoded intervals)

```
Link Audio (local channels)     → [capture] → interval → Opus → WAIF → WS relay → server → all peers
Link Audio (published channels) ← [emit/sink] ← hold N+D ← reassemble ← Opus ← WAIF ← WS relay ← remote peer
```

### Wire Format (WAIF)
Streaming binary format in `wail-app/wire.go`: one WAIF frame per 20ms Opus packet.
- 25-byte header: magic "WAIF", flags (stereo, final), stream_id, interval_index, frame_number, frame_seq, opus_len — followed by Opus data.
- The final frame of an interval appends 28 bytes (sample_rate, total_frames, bpm, quantum, bars) so the receiver can reconstruct the whole interval.

### Interval Model (Go engine)
- Capture assembles fixed-length intervals from Link Audio buffers (`internal/capture`); gaps read as silence and are surfaced as LAN-loss metrics (`internal/lanloss`).
- Playback reassembles decoded frames per room interval index (`internal/emit`), and the playout scheduler releases each interval one boundary late (offset D) into a Link Audio sink (`internal/playout`).
- The relay owns the authoritative room interval clock and broadcasts an `interval_anchor`; `internal/interval` maps each peer's local index to the shared room index (ADR-0003).

## Testing

Run tests with `go test`.

**Before opening any PR**, run the full test suite (`go test ./...` and `go test -tags linkstub ./...`) **and** `./scripts/tier2-e2e.sh` (step mode, the default — it verifies NINJAM-like interval placement end-to-end, not just audio integrity). No exceptions, even for changes that look unrelated to the audio path.

```sh
cd wail-app && go test ./...                    # app + internal package tests
cd wail-app && go test ./internal/interval      # a single package
cd wail-app && go test -tags linkstub ./...     # without the Link SDK (no audio path)
cd signaling-server && go test ./...            # relay server
```

Building or testing the audio path needs cgo (a C++ toolchain + libopus) and GOCACHE write access; in a sandbox you may need to disable it. `-tags linkstub` swaps Link for a stub so the app and its pure logic packages build without the SDK.

`go test` is all in-process. To exercise the real Link Audio Sink/Source path + relay round trip end-to-end on one machine (no DAW), run `./scripts/tier2-e2e.sh` (local relay + WAV-sweep sender + receiver + `linkaudio-probe`; exit 0 = PASS). See DEVELOPMENT.md → "Tier 2 audio E2E". For the WAIL Send/Receive path (ADR-0007), `./scripts/minidaw-e2e.sh` does the equivalent through the plugin: a clap-trap-hosted wail-recv wired to a real headless app over a local relay, checking the looped-back metronome click lands on the session grid within ±5ms (exit 0 = PASS).

### Two-machine debugging (pair-debug)

When a bug needs **two real machines with DAWs** (e.g. sync/audio issues that only appear across networks), use `scripts/pair-debug.sh` — do NOT hand-roll ssh/scp/headless invocations; the harness already handles build, deploy, lifecycle, readiness, and log collection:

```sh
scripts/pair-debug.sh start [--relay prod|local] [--room X] [--bpm N] [--password PW] \
                            [--local-test-tone] [--remote-test-tone] [--local-wav F] [--remote-wav F]
scripts/pair-debug.sh logs     # stream both sides, [local]/[remote] prefixed; Ctrl-C stops watching only
scripts/pair-debug.sh status   # room, relay, PID liveness both sides
scripts/pair-debug.sh stop     # graceful SIGTERM + archive all logs to debug-runs/<ts>/
```

- Drives a headless WAIL locally (instance 90, name `studio`) and one on **andrews-laptop** (`100.105.127.32`, Tailscale SSH, instance 91, name `laptop`). Both join a fresh `debug-<ts>` room; each machine's DAW talks to its local WAIL via Link Audio.
- Default relay is production (`wss://wail-relay.fly.dev`); `--relay local` spins up the studio relay and repoints both sides over Tailscale (use when isolating relay-vs-app).
- `start` **gates on both sides logging a successful room join** and tears down loudly on failure — a green `READY` means the pair is actually connected.
- `stop` archives `local|remote.stdout.log` + `local|remote.wail.log` into `debug-runs/<ts>/` (gitignored; `debug-runs/latest` symlink) — debug from those artifacts.
- Safety: kills only recorded PIDs after cmdline verification (never `pkill`); instances 90/91 keep clear of any GUI WAIL. `start` refuses to run over an unstopped session without `--force`.
- `--local-test-tone` / `--remote-test-tone` give a DAW-less sanity check (tone streams over the relay and is republished as a Link Audio channel on the far side).
- Remote prereqs (already done for andrews-laptop): Remote Login + authorized key, `brew install opus opusfile`. Env overrides: `PAIR_REMOTE`, `PAIR_LOCAL_INSTANCE`, `PAIR_REMOTE_INSTANCE`, `PAIR_RELAY_PORT`, `PAIR_JOIN_TIMEOUT`.

**Skip tests for docs-only changes.** If a PR only modifies `.md` files (or other non-code docs), do not run the test suite — building the audio path requires the Link SDK/cgo and is slow. Tests are not needed when no code paths change.

## Code Conventions

- Goroutines + channels for cross-task communication
- Structured logging via `log`/`slog`
- Protocol messages are JSON with a `type` discriminator (tagged unions)
- Audio messages use the WAIF streaming wire format over the WebSocket relay
- Echo guard pattern: suppress re-broadcast for 150ms after applying remote changes
- Pure engine logic lives in `wail-app/internal/{interval,playout,lanloss,affinity,capture,emit}` — no cgo, no networking, fully unit-testable; the cgo Link Audio layer (`internal/abllink`) wraps it
- TDD: write tests first, especially for the interval/codec logic

## Versioning and Releases

Managed by [knope](https://github.com/knope-dev/knope) via `knope.toml`. Both Go modules share one repo-wide version.

**Versioned files** (kept in sync automatically): `VERSION`

### Recording changes

Conventional commit messages (`feat:`, `fix:`, `feat!:`) are the **sole** mechanism for changelog entries. Knope's `PrepareRelease` step processes **both** conventional commits and changeset files independently — using both for the same change produces duplicate changelog entries.

**Do NOT create a changeset file for changes that already use a conventional commit prefix.** Changeset files are a fallback only for `chore:` commits (infrastructure, CI, docs) that need a changelog entry but won't be picked up by conventional commit parsing.

If you need a changeset for a `chore:` commit:

```sh
knope document-change
```

**Changeset frontmatter format:** The YAML frontmatter must use `default: <type>` (e.g., `default: patch`). Do NOT use `type: <type>` or package names — knope silently ignores unrecognized package keys. Example:
```markdown
---
default: patch
---

Description of the change.
```

### Release pipeline (automated via GitHub Actions)

Releases are fully automated — no manual `knope` commands needed:

1. **Push to `main`** → `auto-release.yml` runs `knope prepare-release`, which consumes conventional commits (and `.changeset/` files if present as a fallback), bumps versions, updates `CHANGELOG.md`, and opens/updates a PR from the `release` branch → `main`.
2. **Merge the release PR** → `release-on-merge.yml` runs `knope release` (creates GitHub release + git tag) and dispatches artifact builds.
3. **`release.yml`** builds platform artifacts (Windows/Linux app binaries + zip/tar archives, plus the Homebrew source tarball that serves as the macOS channel) and uploads them to the GitHub release. A failed platform build does not block the release: `create-release` packages and uploads whatever artifacts exist (the run still reports failure so a missing platform is visible).

### Rules for agents

- **Use conventional commits for user-facing work** (`feat:`, `fix:`, `feat!:`). Do NOT also create a changeset file — knope processes both sources and this creates duplicate changelog entries.
- **Never manually edit the version number** in `VERSION` — knope handles this.
- **Never manually create git tags** for releases — GitHub Actions handles tagging.
- **Never run `knope release` or `knope prepare-release` locally** — GitHub Actions runs both automatically.
- **Use the correct conventional commit prefix.** New features MUST use `feat:`, bug fixes MUST use `fix:`, breaking changes MUST use `feat!:` or `fix!:`. Never use `fix:` for a new feature — this causes knope to bump only the patch version instead of minor. Similarly, never use unprefixed or `chore:` commits for user-facing changes — knope ignores them entirely. Get the prefix right; it directly controls the version bump.
- **Semver is now standard (post-1.0).** `feat:` / `default: minor` → minor bump, `fix:` / `default: patch` → patch bump, `feat!:` / `default: major` → major bump. No pre-1.0 shifting applies.
- **Every push to `main` produces a release — there is no release-free path.** `auto-release.yml` runs on every main push; if no commit since the last tag matches `feat|fix|refactor|perf` (and no `.changeset/` files exist), its "Ensure changeset exists" step auto-creates a fallback patch changeset from the latest commit message. So `test:`/`chore:`/`docs:`-only pushes are **patch releases**, not ignored — v3.9.7 shipped with only test infrastructure (#411). (knope itself ignores `test:` commits; the fallback changeset is the mechanism, which is why the changelog gets exactly one entry, not a duplicate.) Batch infra/docs/test changes to keep release noise down.
- **Keep docs in sync.** For each PR, check whether `README.md` and `docs/architecture.md` need updates to reflect the changes. User-facing features should update README; architectural changes (wire format, Link Audio engine, internal package structure, new design decisions) should update `docs/architecture.md`.

## Common Tasks

- **Add a new sync message**: Add a variant to `SyncMessage` in `wail-app/protocol.go`, handle it in the `wail-app/session.go` select loop
- **Change Link polling rate**: `linkPollInterval` in `wail-app/link_types.go`
- **Change Opus bitrate**: `engineBitrateKbps` in `wail-app/audio_engine_real.go` (passed to `NewIntervalEncoder` in `interval_codec.go`)
- **Change the interval offset D**: `WAIL_INTERVAL_OFFSET` env var (default 1), read in `wail-app/session.go` and applied via `playout.New` in `audio_engine_real.go`
- **Change the emit cushion**: `WAIL_EMIT_CUSHION_MS` env var or the Debug-tab slider (default 100, clamped 100–500), read in `wail-app/audio_engine_real.go`; it adds directly to a Link Audio subscriber's reported buffering
- **Modify wire format**: `wail-app/wire.go` (bump the flags/format)


## Trade-off Preferences

When encountering code quality trade-offs, follow these principles (derived from owner decisions):

### Error handling
- **Never panic in production paths.** Replace `unwrap()` with match/`?`/log-and-continue. Mutex poison → handle gracefully (return error to host, not crash).
- **Make failures observable.** Silent `.ok()` that discards errors must log at `warn!` level. If something can fail and the caller won't notice, add a log line.
- **Defensive clamping over error propagation** for internal numeric inputs. Bad values (zero divisors, negative durations) → clamp to safe minimums. Don't bubble `Result` for things that should just work.
- **TDD safety-critical fixes.** Division-by-zero, overflow, NaN — write the failing test first, then fix.

### Scope and priorities
- **Batch obvious fixes, discuss complex ones.** If a fix has no real trade-off (dead code, misleading labels, redundant imports), just do it. Only pause for decisions that involve architectural choices or behavioral changes.
- **Fix code, don't add process.** Prefer actual code changes over adding TODOs, lint suppressions, or documentation-only fixes. Exception: `#[allow(dead_code)]` is fine for fields that are structurally needed but not yet read.

### Trade-off log
All deferred decisions and remaining code quality items are tracked in `tradeoffs.md` at the repo root. When making a trade-off decision during development, record it there with the rationale.

## Direction: Link Audio Is the Primary Audio Interface (ADR-0001, amended by ADR-0007)

Ableton Link 4.0 (final, May 2026) introduces Link Audio — real-time uncompressed PCM streaming between Link peers on a LAN (unicast UDP, fire-and-forget). The API (LinkAudio.hpp) provides:
- `LinkAudioSink`: publish audio channels to the network
- `LinkAudioSource`: subscribe to remote audio channels
- Channel discovery via `channels()` and `setChannelsChangedCallback()`

Decided direction (see `CONTEXT.md` pillars and `docs/adr/0001`): WAIL interacts with local audio primarily as a Link peer — capture subscribes to local Link Audio channels, playback publishes remote streams as Link Audio channels one interval late. The original Rust Send/Recv plugins, their TCP IPC, and the entire Rust workspace were retired. **Amended by ADR-0007:** a first-party CLAP bridge (WAIL Send / WAIL Receive, in `plugins/`) is back as an *optional* path for DAWs without Link Audio — but each instance is simply another LAN Link Audio peer, so the app is unchanged and all codec/interval/relay logic stays in Go. ADR-0005's raw-PCM-over-loopback-IPC bridge was superseded and removed.

`vendor/link` is pinned to the final `Link-4.0` tag. Research: `docs/link-4-research.md`, `docs/link-audio-research.md`.

### Migration status (branch `quasor/link-audio-engine`)

All steps of `docs/link-audio-migration-plan.md` are implemented: the Link Audio engine is the only audio path (no flag), and the plugins, TCP IPC, and Rust workspace are gone. New Go pieces:

- `wail-app/internal/abllink` — cgo `abl_link` binding (sync + Link Audio; pure-C capture ring), against `vendor/link` (Link-4.0). Replaced the external `abletonlink-go`.
- `wail-app/internal/{interval,playout,lanloss,affinity,capture,emit}` — pure, unit-tested engine logic (interval/room clock, hold-until-N+D scheduler, LAN loss, channel affinity, interval assembly/reassembly + paced playout).
- `wail-app/interval_codec.go` — interval Opus↔WAIF codec (loopback-tested).
- `wail-app/audio_engine_real.go` — the capture + emit engine (`//go:build !linkstub`; no-op stub otherwise).
- `signaling-server` — relay-authoritative room interval clock + `interval_anchor` broadcast.

Remaining is hardware validation only: run two machines with a Link-Audio DAW to exercise the Source/Sink data path + real-time timing, and confirm the Windows (MinGW) / Linux cgo builds link. The interval offset D is env-configurable (`WAIL_INTERVAL_OFFSET`, default 1); a GUI control for it is a follow-up.
