# Link Audio Migration — Work Breakdown

The plan for turning WAIL into a Go Link Audio peer, per `docs/adr/0001`, `0002`, `0003` and `CONTEXT.md`. This is a **big-bang** migration on one branch: plugins, IPC, and the Rust workspace come out and the Link Audio engine goes in together, so the branch is **non-functional until step 3 (emit) lands**. The steps below are the *internal build order*, not separate PRs.

## Preconditions

- [ ] PR #342 (remove WebRTC/TURN leftovers) merged.
- [ ] PR #343 (bump `vendor/link` to final 4.0) merged.
- [ ] Confirm the MinGW `link_audio/Channels.hpp` `endpoint`/`interface` collision still exists at the `Link-4.0` tag and decide where our own C-side fix lives (it moves out of the abletonlink-go clone/patch and into our binding).

## Step 0 — cgo `abl_link` binding (sync + audio) vs `vendor/link`

The riskiest piece (cross-platform C build + linking). Do it first, sync-only, as a like-for-like replacement of `abletonlink-go`, so build risk is isolated from audio logic.

- [ ] New Go package wrapping `abl_link` (one handle: sync + audio) compiled against `vendor/link`. C sources + cgo directives; own the C++ build (CMake or direct) and the MinGW fix.
- [ ] Port `link_real.go` to the new binding (tempo, beat, phase, enable/disable, poller). Keep `link_stub.go` for `-tags linkstub`.
- [ ] Verify build + link on macOS, Windows (MinGW), Linux.
- [ ] Remove `abletonlink-go` from `go.mod`; drop the DatanoiseTV clone/patch from CI and the Homebrew formula.
- [ ] **Invariant to hold from here on:** the Link Audio capture callback (step 1) must be a *pure C* function pointer, never a Go `//export` (see ADR-0002).

## Step 1 — Link Audio capture

- [ ] Channel discovery: `setChannelsChangedCallback`; surface the available local channels (excluding WAIL's own peer's channels — no feedback loop).
- [ ] Pure-C source callback → `memcpy` into a C-owned preallocated ring; Go goroutine drains off-thread.
- [ ] Per-channel interval bucketing: map each buffer's `sessionBeatTime` via `beginBeats(sessionState, quantum)` → interval index (labeled with the shared room index from step 2); resample to 48k at the edge if the channel isn't 48k; no clock recovery.
- [ ] Opus-encode completed intervals → WAIF frames → the **existing** relay path (unchanged).
- [ ] LAN-loss metric from the per-buffer `count` sequence gaps.
- [ ] Explicit per-channel opt-in ("send-mixer"): only bridge channels the user ticks.

## Step 2 — Relay-authoritative interval clock

- [ ] `signaling-server`: track room tempo + interval config; own the room interval index; broadcast it (index + what clients need to label their local boundaries).
- [ ] Clients derive/label their own local interval boundaries with the shared index (RTT to the one server as the shared reference).
- [ ] WAIF `interval_index` now carries the *room* index → delete the per-peer `rewrite_waif_interval_index` remap.
- [ ] Confirm the joining→playing session model still holds without the plugin `plugin_connected` signal (metrics phase model changes when plugins go).

## Step 3 — Link Audio emit (branch becomes functional here)

- [ ] Receive WAIF from the relay → Opus-decode → per-channel **pending** buffer keyed on `(remote identity, stream index)`.
- [ ] Hold-until-boundary scheduler: release pending interval `N` at the local boundary labeled `N + D` (D configurable, default 1).
- [ ] Publish via `LinkAudioSink`, deep-queue top-up from Go (not a C pacing thread); stamp to the local beat window.
- [ ] Affinity: a reconnecting identity reclaims the same channel; channel name `"{peer display name} · {stream name}"`.
- [ ] Late/incomplete at `N+D` → play-partial + live-append; log at `warn` + per-direction/per-stream "interval incomplete" metric (distinct from LAN loss and decode failures).

## Step 4 — GUI

- [ ] Capture send-mixer (tick discovered local channels).
- [ ] Published-channel / remote-stream view; drop plugin/slot UI.
- [ ] Config surface for `D` (interval offset).

## Step 5 — Retire the old world

- [ ] Delete `wail-plugin-send`, `wail-plugin-recv`, `wail-plugin-test`, the nih_plug fork dependency.
- [ ] Delete the TCP IPC layer: `wail-app/ipc.go`, `IPCWriterPool`, `crates/wail-audio/src/ipc.rs`, `plugin_install.go`.
- [ ] Delete the **entire Rust workspace** (`crates/`, `xtask/` plugin tasks) once the Go engine replaces it.
- [ ] Reimplement the test client in Go against the app's own WAIF/relay code (retire `wail-test-client`).
- [ ] `release.yml` / `homebrew/wail.rb`: drop the abletonlink-go clone + MinGW patch on its copy; build against `vendor/link` via our binding.
- [ ] Update `CLAUDE.md` (project structure, build, architecture) and `docs/architecture.md` to the new single-language, Link-Audio shape.

## Cross-cutting

- **Observability (pillar 8):** LAN loss (capture `count` gaps), interval-incomplete (emit), decode failures — all per-direction/per-stream in the metrics/dashboard.
- **RT safety (pillar 7 + ADR-0002):** pure-C capture callback; no allocation/locking on the drain hot path; deep-queue emit.
- **Unchanged:** WAIF wire format, sync/JSON protocol shape, rooms, reconnect, relay bandwidth model.

## Open questions to resolve during build

- Go resampler choice for non-48k capture channels.
- Verify the `LinkAudioSink` internal queue depth (confirm hundreds-of-ms so deep-queue emit tolerance holds; if shallow, revisit the C-pacing-thread option).
- Exact client↔server clock/anchor protocol for the interval index (tick broadcast vs epoch + tempo derivation).
- Whether `D` is per-room (relay-config) or per-client, and how it's advertised.
