# Link Audio Migration — Work Breakdown

The plan for turning WAIL into a Go Link Audio peer, per `docs/adr/0001`, `0002`, `0003` and `CONTEXT.md`. This is a **big-bang** migration on one branch: plugins, IPC, and the Rust workspace come out and the Link Audio engine goes in together, so the branch is **non-functional until step 3 (emit) lands**. The steps below are the *internal build order*, not separate PRs.

## Implementation status (branch `quasor/link-audio-engine`)

**All steps (0–5) are implemented.** The Link Audio engine is the only audio path; the plugins, TCP IPC, and the entire Rust workspace are gone. Everything builds (wail-app real + `-tags linkstub`, signaling-server) and the full Go test suite + vet pass on macOS. What still needs real hardware (two machines with a Link-Audio DAW on a LAN) is the *behaviour* of the Source/Sink data path, real-time timing/pacing, and the cross-platform (Windows/Linux) cgo link — none of which is exercisable in this environment.

What exists and how it's verified:

- **cgo `abl_link` binding** (`wail-app/internal/abllink`): sync + Link Audio (capture `Source` with a pure-C ring per ADR-0002, emit `Sink`, channel discovery), compiled against `vendor/link` (bumped to `Link-4.0`). Replaces `abletonlink-go`. Compiles/links/runs + smoke tests on macOS. **Cross-platform link (Windows/Linux) unverified here.**
- **Pure engine logic** (fully unit-tested): `internal/interval` (bucketing, `RoomClock`, `RoomLabeler`), `internal/playout` (hold-until-N+D), `internal/lanloss`, `internal/affinity`, `internal/capture` (interval assembler), `internal/emit` (reassembler + paced reader).
- **Interval Opus codec** (`wail-app/interval_codec.go`): PCM↔WAIF, **loopback-verified in Go** (real libopus, no hardware) — `interval_codec_test.go`.
- **Relay interval clock** (`signaling-server`): owns the room index, broadcasts `interval_anchor`, quantizes tempo changes to boundaries. Unit-tested. Client consumes it via `RoomLabeler`.
- **Engine + wiring** (`audio_engine_real.go`, `session.go`): capture→relay and relay→playback, always on (no flag). Emit-ingestion tested (`audio_engine_real_test.go`); the Source/Sink data path + real-time timing need hardware.
- **Old world removed** (Step 5): `crates/`, `xtask/`, plugins, TCP IPC (`ipc.go`, `IPCWriterPool`, `plugin_install.go`), `abletonlink-go`; CI/Homebrew/knope build the Go app against `vendor/link` via our binding; versioning moved to a `VERSION` file. Passive peer: `ForceBeat` and the per-peer `RewriteWaifIntervalIndex` remap are gone.
- **GUI** (Step 4): capture send-mixer (tick discovered channels), Link-Audio status; plugin/install UI dropped.

**Remaining (hardware validation only):** run two machines with a Link-Audio DAW, confirm capture→WAN→playback is beat-aligned at offset D, and confirm the Windows (MinGW) / Linux cgo builds link. The interval offset D is set via `WAIL_INTERVAL_OFFSET` (default 1); a GUI control for it is a nice-to-have follow-up.

## Preconditions

- [x] ~~PR #342 (remove WebRTC/TURN leftovers) merged.~~ Independent cleanup; not required by the engine work (this branch bumps `vendor/link` itself).
- [x] `vendor/link` bumped to the final `Link-4.0` tag (`e9a2e41`) on this branch — same change as PR #343.
- [x] MinGW `Channels.hpp` `interface`-macro collision confirmed (parameter named `interface`, line 52); the C-side fix lives in `internal/abllink/wrap.cpp` (Windows-only, unverified here).

## Step 0 — cgo `abl_link` binding (sync + audio) vs `vendor/link`

The riskiest piece (cross-platform C build + linking). Do it first, sync-only, as a like-for-like replacement of `abletonlink-go`, so build risk is isolated from audio logic.

- [x] New Go package `internal/abllink` wrapping `abl_link` (one handle: sync + audio) compiled against `vendor/link`. cgo directives own the C++ build (no CMake); MinGW fix in `wrap.cpp`.
- [x] Port `link_real.go` to the new binding. `link_stub.go` kept for `-tags linkstub`.
- [x] Build + link verified on **macOS** (arm64). Windows (MinGW) / Linux: directives present, **unverified in this environment**.
- [x] `abletonlink-go` removed from `go.mod` (require + replace). CI/Homebrew clone-patch removal is part of Step 5.
- [x] **Invariant held:** the capture callback is pure C (`internal/abllink/capture.c`), never a Go `//export`.

## Step 1 — Link Audio capture

- [x] Channel discovery: `setChannelsChangedCallback`; surface the available local channels (excluding WAIL's own peer's channels — no feedback loop).
- [x] Pure-C source callback → `memcpy` into a C-owned preallocated ring; Go goroutine drains off-thread.
- [x] Per-channel interval bucketing: map each buffer's `sessionBeatTime` via `beginBeats(sessionState, quantum)` → interval index (labeled with the shared room index from step 2); resample to 48k at the edge if the channel isn't 48k; no clock recovery.
- [x] Opus-encode completed intervals → WAIF frames → the **existing** relay path (unchanged).
- [x] LAN-loss metric from the per-buffer `count` sequence gaps.
- [x] Explicit per-channel opt-in ("send-mixer"): only bridge channels the user ticks.

## Step 2 — Relay-authoritative interval clock

- [x] `signaling-server`: track room tempo + interval config; own the room interval index; broadcast it (index + what clients need to label their local boundaries).
- [x] Clients derive/label their own local interval boundaries with the shared index (RTT to the one server as the shared reference).
- [x] WAIF `interval_index` now carries the *room* index → delete the per-peer `rewrite_waif_interval_index` remap.
- [x] Confirm the joining→playing session model still holds without the plugin `plugin_connected` signal (metrics phase model changes when plugins go).

## Step 3 — Link Audio emit (branch becomes functional here)

- [x] Receive WAIF from the relay → Opus-decode → per-channel **pending** buffer keyed on `(remote identity, stream index)`.
- [x] Hold-until-boundary scheduler: release pending interval `N` at the local boundary labeled `N + D` (D configurable, default 1).
- [x] Publish via `LinkAudioSink`, deep-queue top-up from Go (not a C pacing thread); stamp to the local beat window.
- [x] Affinity: a reconnecting identity reclaims the same channel; channel name `"{peer display name} · {stream name}"`.
- [x] Late/incomplete at `N+D` → play-partial + live-append; log at `warn` + per-direction/per-stream "interval incomplete" metric (distinct from LAN loss and decode failures).

## Step 4 — GUI

- [x] Capture send-mixer (tick discovered local channels).
- [x] Remote-stream/peer view; drop the plugin-install UI.
- [ ] Config surface for `D` (interval offset) — currently env-only (`WAIL_INTERVAL_OFFSET`); a GUI control is a follow-up.

## Step 5 — Retire the old world

- [x] Delete `wail-plugin-send`, `wail-plugin-recv`, `wail-plugin-test`, the nih_plug fork dependency.
- [x] Delete the TCP IPC layer: `wail-app/ipc.go`, `IPCWriterPool`, `crates/wail-audio/src/ipc.rs`, `plugin_install.go`.
- [x] Delete the **entire Rust workspace** (`crates/`, `xtask/` plugin tasks) once the Go engine replaces it.
- [x] Reimplement the test client in Go against the app's own WAIF/relay code (retire `wail-test-client`).
- [x] `release.yml` / `homebrew/wail.rb`: drop the abletonlink-go clone + MinGW patch on its copy; build against `vendor/link` via our binding.
- [x] Update `CLAUDE.md` (project structure, build, architecture) and `docs/architecture.md` to the new single-language, Link-Audio shape.

## Cross-cutting

- **Observability (pillar 8):** LAN loss (capture `count` gaps), interval-incomplete (emit), decode failures — all per-direction/per-stream in the metrics/dashboard.
- **RT safety (pillar 7 + ADR-0002):** pure-C capture callback; no allocation/locking on the drain hot path; deep-queue emit.
- **Unchanged:** WAIF wire format, sync/JSON protocol shape, rooms, reconnect, relay bandwidth model.

## Open questions to resolve during build

- Go resampler choice for non-48k capture channels.
- Verify the `LinkAudioSink` internal queue depth (confirm hundreds-of-ms so deep-queue emit tolerance holds; if shallow, revisit the C-pacing-thread option).
- Exact client↔server clock/anchor protocol for the interval index (tick broadcast vs epoch + tempo derivation).
- Whether `D` is per-room (relay-config) or per-client, and how it's advertised.
