# Trade-off Log

Deferred decisions and remaining code quality items. Each entry has enough context to review and adjust without re-reading the code.

## Completed

| ID | Fix | Commit |
|----|-----|--------|
| C1 | `unwrap()` in signaling client → match + log + continue | 470c1fa |
| C2 | Mutex poison in plugin `initialize()` → return false to DAW | 470c1fa |
| C3 | Silent Opus init failure → `warn!` log when encoder/decoder is None | 470c1fa |
| C5 | Division by zero in interval tracking → clamp bars≥1, quantum≥ε | 470c1fa |
| W4 | Volume param labeled "dB" but is linear 0–1 → removed misleading unit | f4c1f83 |
| W7 | Integer overflow in clock offset → saturating arithmetic + negative RTT guard | f4c1f83 |
| W8 | `Vec::remove(0)` in clock sync → `VecDeque` + `pop_front()` | f4c1f83 |
| W9 | Dead code warnings → `#[allow(dead_code)]` with comments, removed unused imports/mut | f4c1f83 |
| W13 | Unused `WailTask` enum → removed, `type BackgroundTask = ()` | f4c1f83 |
| I8 | Redundant `use serde_json;` in signaling server → removed | f4c1f83 |
| W5 | `let _ =` send failures → tiered logging: `warn!` for critical paths, `debug!` for hot paths | 67a02c2 |
| W12 | DataChannel send() silent when None → `debug!` log when channel not ready | 67a02c2 |
| W17 | `mem::replace` receiver swap → `Option<Receiver>` with `.take()` methods | 67a02c2 |
| W2 | Plugin hardcoded 128kbps bitrate → passes `bitrate_kbps` param through | 67a02c2 |
| W3 | Plugin hardcoded 2 channels → derives from `audio_io_layout` | 67a02c2 |
| I1 | No `Default` for `ClockSync` → added `impl Default` | 67a02c2 |
| I2 | No `Default` for `IpcRecvBuffer` → added `impl Default` | 67a02c2 |
| I3 | Magic number `10` for snapshot interval → `SNAPSHOT_INTERVAL_TICKS` constant | 67a02c2 |
| W1 | Duplicate AudioBridge → deleted old bridge, plugin uses `wail_audio::AudioBridge` | 085a16e |
| W14 | Audio IPC not wired → TCP IPC between plugin and app, bidirectional audio intervals | 085a16e |
| W6 | Unbounded audio channels → bounded(64) with drop-on-full for 3 audio channels; sync/signaling/ICE left unbounded | 36272e4 |

---

## Deferred — Infrastructure (revisit when deploying)

### W6. Unbounded channels (sync/signaling/ICE — 6 remaining instances)
**Status:** Partially fixed — audio channels bounded, sync/signaling left unbounded
**Files:** `crates/wail-net/src/signaling.rs:33-34`, `crates/wail-net/src/peer.rs:67,100,162`, `crates/wail-core/src/link.rs:120-121`
**Problem:** Remaining sync/signaling channels are unbounded. Messages are tiny JSON structs at low frequency.
**Decision:** Low risk — leave until scale demands it. Audio channels (the real risk) are now bounded(64) with `try_send` + drop-on-full.

### W10. No graceful shutdown
**Status:** Deferred — infrastructure concern
**File:** `crates/wail-app/src/main.rs`
**Problem:** Binary doesn't handle SIGINT/SIGTERM. Process just dies on Ctrl+C.
**Fix when ready:** Add `tokio::signal::ctrl_c()` branch in `select!` loop.

### W11. No reconnection logic
**Status:** Completed
**File:** `crates/wail-net/src/lib.rs`, `wail-app/session.go`
**Problem:** Signaling server disconnect and WebRTC peer failures killed the session permanently.
**Resolution:** Implemented automatic reconnection for both:
- **WebRTC peers:** `MeshEvent::PeerFailed` detection via connection state callbacks, `re_initiate()` with exponential backoff (2s–16s, max 5 attempts), UI events (`peer:reconnecting`).
- **Signaling server:** Reconnect loop with exponential backoff (1s–30s, unlimited attempts), re-fetches ICE servers, replaces PeerMesh.
- **Tests:** `peer_failure_emits_event`, `peer_reconnects_after_close`, `new_offer_replaces_stale_connection` in `crates/wail-net/tests/network.rs`.

### W16. Signaling server has no rate limiting
**Status:** Deferred — infrastructure concern
**File:** `signaling-server/main.go`
**Problem:** No rate limiting on the WebSocket signaling server.
**Fix when ready:** Add rate limiting via middleware or per-connection message throttling.

---

## Deferred — Feature Work

### W15. Clock offset computation removed
**Status:** Done — dead code removed
**File:** `crates/wail-core/src/clock.rs`
**Rationale:** Link timestamps (`link.clock_micros()`) and ClockSync timestamps (`Instant::now()`) are different clock domains. Offset computation was dead code — it was never applied to anything. `ClockSync` now tracks RTT only (`VecDeque<i64>` of RTT samples per peer), using a median over the last 8 samples. RTT is available via `rtt_us(peer_id)` for diagnostics.

---

## Skipped

### C4. Signaling server has no rate-limiting
**Status:** Partially addressed — room passwords + capacity check added
**File:** `signaling-server/main.go`
**Problem:** No rate limiting, no per-connection message throttling.
**Rationale:** Room passwords prevent unauthorized joins. 32-slot capacity check prevents room overflow. Rate limiting to revisit when needed.

---

## Design Decisions

### Link Audio is the only audio interface (retire Send/Recv plugins)
**Status:** Decided — see `docs/adr/0001-link-audio-is-the-only-audio-interface.md` (ADRs in `docs/adr/` are now the canonical home for architectural decisions)
**Decision:** WAIL captures and plays local audio exclusively via Ableton Link Audio (Link 4.0); the CLAP/VST3 plugins and TCP IPC protocol are to be retired. Lossless capture narrows to the WAN leg; the LAN hop is best-effort with loss detection surfaced in metrics.

### Linear crossfade at interval boundaries
**Status:** Done
**File:** `crates/wail-audio/src/ring.rs`
**Decision:** Switched from equal-power (sin/cos) to linear (t / 1−t) crossfade at interval boundaries. Equal-power preserves constant power for uncorrelated signals but causes a √2 ≈ +3dB amplitude bump for correlated signals (sustained notes, test tones, drones). Linear guarantees new_w + old_w = 1.0 at every point — no amplitude variation regardless of signal content. The −3dB power dip for uncorrelated signals over the ~2.7ms window is inaudible. Also fixed stereo-aware frame iteration so L/R pairs get identical weights. Matches NINJAM's reference crossfade implementation.

---

### WebRTC → WebSocket relay migration
**Status:** Done
**Files:** `crates/wail-net/`, `signaling-server/main.go`, `wail-app/session.go`
**Decision:** Replaced all WebRTC DataChannels with server-relayed WebSocket messages. All sync (JSON text) and audio (binary) data now flows through the Go signaling server, which broadcasts to all room peers (SFU-style). Removed `webrtc`/`webrtc-ice` crate dependencies and deleted `peer.rs`.
**Rationale:** Dramatically simpler architecture — no ICE/STUN/TURN negotiation, no per-peer connection state, no tie-breaking logic. Eliminates Metered TURN dependency and credentials management. Compile times and binary size significantly reduced.
**Trade-offs:** (1) TCP head-of-line blocking — a lost packet delays all subsequent frames, unlike WebRTC's UDP/SCTP. For audio at ~50 frames/sec this could cause occasional latency spikes. (2) Server bandwidth scales quadratically — N peers × (N−1) streams relayed through the server. At 128kbps Opus with 4 peers: ~1.5 Mbps outbound. (3) One extra network hop (peer→server→peer) vs direct P2P, though for users behind NATs this may be comparable to TURN relay.

---

### TempoChangeDetector extraction
**Status:** Done
**File:** `crates/wail-core/src/link.rs`
**Decision:** Extracted tempo-change detection logic (threshold check + echo guard state machine) from `LinkBridge` into a separate `pub(crate) TempoChangeDetector` struct. `LinkBridge` delegates to it. The detector accepts `Instant` as a parameter for deterministic testing without `AblLink` (C FFI + CMake). Integration testing of the full `LinkBridge` → `AblLink` path is deferred to e2e tests.

---

## Polish (low priority)

| ID | Item | File | Status |
|----|------|------|--------|
| I4 | ~~Single STUN server, no TURN~~ | Removed — WebRTC replaced by WebSocket relay | Resolved |
| I5 | `now_us()` cast u128→i64 overflows after 292 years | `crates/wail-core/src/clock.rs:36` | Open |
| I6 | Median uses upper-median for even-length arrays | `crates/wail-core/src/clock.rs:87` | Open |
| I7 | Echo guard 150ms window suppresses legit fast tempo changes | `crates/wail-core/src/link.rs:89-94` | Open |
| I9 | DAW aux output ports show "Slot 1–31" instead of actual peer display names | `crates/wail-plugin-recv/src/lib.rs` | Fixed (CLAP only) |

### I9. Dynamic peer names in DAW aux outputs
**Status:** Fixed for CLAP hosts — VST3 has no equivalent API
**File:** `crates/wail-plugin-recv/src/lib.rs`, nih_plug fork at `MostDistant/nih-plug@feat/dynamic-audio-port-names`
**Solution:** Forked nih_plug to add `ProcessContext::set_aux_output_name()` + `rescan_audio_port_names()` which call CLAP's `host.audio_ports->rescan(CLAP_AUDIO_PORTS_RESCAN_NAMES)`. Added `IPC_TAG_PEER_NAME` message type to forward display names from the Tauri session to the recv plugin. When a peer sends Hello with a display name, the session broadcasts it via IPC, the plugin updates the dynamic port name, and triggers a host rescan.
**Limitation:** VST3 hosts will still show static "Slot 1–31" names — VST3 has no bus rename API.

---

## Link Audio migration (branch `quasor/link-audio-engine`)

### Transitional flag vs. strict big-bang
**Status:** Decided (transitional)
**File:** `wail-app/session.go`, `docs/link-audio-migration-plan.md`
**Decision:** ADR-0002 calls for a big-bang (plugins/IPC/Rust out, Link Audio in, together). The Link Audio audio path cannot be functionally tested in this environment (needs real Link peers + a DAW on a LAN), so the engine landed behind a `WAIL_LINK_AUDIO=1` flag with the plugin/IPC/Rust path still the default. Retiring the old world (Step 5) before the new path is hardware-validated would leave the app with only an unverified audio path. Promote to default and do Step 5 after validation.

### Non-48k capture resampling
**Status:** Open (basic implementation)
**File:** `wail-app/audio_engine_real.go` (`resampleLinearInterleaved`)
**Decision:** Capture channels not at 48 kHz are resampled with linear interpolation — adequate for a jam on-ramp, but a low-pass/polyphase resampler would reduce aliasing. Migration-plan open question §68.

### Self-channel exclusion is best-effort
**Status:** Resolved (issue #352)
**File:** `wail-app/internal/affinity/ownchannels.go`, `wail-app/audio_engine_real.go` (`reconcileChannels`)
**Decision:** Peer-name-only matching hid a third-party publisher (Bitwig VoidLinkAudioSend defaulting its peer name to the machine name) in a live test. The `abl_link` C API still doesn't expose a sink's channel id, so `affinity.OwnChannels` records every sink name WAIL mints and learns each sink's id from the first discovery snapshot pairing a minted name with our peer name; exclusion is by learned id thereafter (rename-proof). Residual theoretical false-skip: a same-LAN publisher matching both our peer name and a minted name.

### Send pacing rate and relay burst limits
**Status:** Superseded by streaming capture (kept as defense in depth)
**File:** `wail-app/internal/pace/pace.go`, `wail-app/internal/capture/assembler.go`, `signaling-server/main.go`
**Decision:** Capture now streams each 20ms window as it fills (like the old plugins / NINJAM), so frames leave at real-time cadence during the interval and arrive ~a full interval before the D=1 playout boundary — no burst exists in normal operation. The pacer (2× real time, one frame per 10ms) stays wired as defense in depth for abnormal batches (interval-close flushes after a capture stall). The relay keeps the flat per-stream bucket (rate 100/s, burst 2500) rather than an interval-aware limit — simpler, and now far above steady-state.

### Emitted capture windows are immutable
**Status:** Decided
**File:** `wail-app/internal/capture/assembler.go` (`AddWindows`)
**Decision:** Streaming capture emits a window once coverage passes its end; a buffer that lands behind the emitted boundary is trimmed/dropped and counted (`DroppedBackfill`). Link Audio buffers arrive in temporal order from the C ring, so backfill only occurs on pathological reordering — accepted in exchange for real-time transmission. The relay keeps a flat per-stream token bucket (rate 100/s, burst 2500) rather than an interval-aware limit derived from bars/quantum/BPM — simpler, and the burst covers a full interval even at slow tempos (1200 frames at 40 BPM / 4 bars). Revisit if intervals ever exceed ~25s.

### Residual splices are hard cuts (no crosslap) and drift accrues as latency
**Status:** Mostly resolved (2026-07 audio-quality pass); crosslap on the few remaining splice points still open
**File:** `wail-app/internal/capture/assembler.go`, `wail-app/internal/emit/{feeder,plc}.go`, `wail-app/audio_engine_real.go`
**Decision:** The quality pass landed the substantive fixes: bounded drift is now absorbed by the capture **micro-slew** (≤4 frames smeared over a 64-frame tail resample past a 10ms deadband — no splice, replaces the eventual 250ms re-anchor for pure drift), receive-side sequence gaps are masked by **Opus PLC** (decode-order synthesis, ≤120ms per gap, real frames still win), and the emit side keeps an ~80ms **cushion** ahead of the playhead so scheduler stalls no longer reach the ear (shortfalls counted as sink underruns). Still hard splices, deliberately: capture re-anchors after genuine discontinuities (LAN loss, >250ms divergence — rare, counted, logged) and zero-padded interval tails after capture stops. A 1–5ms crossfade at those two points is the remaining polish; deferred until the new counters show they occur in practice. NINJAM forum precedent for the drift disease and its cure (place audio at its true sample time, not the rounded grid) noted in the 2026-07 investigation. Fractional interval lengths (e.g. 44.1k/117BPM → 180923.077 samples) still imply ±1-sample playout splices at non-integer tempos; inaudible so far. 2026-07-21: one large boundary splice this entry missed is now fixed — the receiver played the final WAIF window's zero padding at tempos where the interval isn't a multiple of 960 frames (~15ms of silence + overlapping beat stamps at every boundary, an audible click at e.g. 124.59 BPM); playout now stops at the exact interval length (`intervalPlayoutFrames`).

### Cross-platform binding link unverified
**Status:** Open
**File:** `wail-app/internal/abllink/{abllink.go,wrap.cpp}`
**Decision:** The cgo directives + MinGW `interface`-macro fix are in place, but only the macOS (arm64) build/link/run is verified in this environment. Windows (MinGW) and Linux must be confirmed in CI.

### Feeder does not retry rejected sink writes
**Status:** Open (now observable)
**File:** `wail-app/internal/emit/feeder.go`, `wail-app/audio_engine_real.go` (`topUpSinks`)
**Decision:** `Sink.WriteInterleaved` can refuse a chunk (no subscribed listener, or the SDK's 128-buffer queue momentarily full), and the feeder has already advanced its cursor past it — the chunk is never re-sent. The `abl_link` C API cannot distinguish the two causes, and refusals are continuous and benign while nobody is subscribed, so retry/rewind semantics were deferred. Instead the success→failure edge is counted (`EmitSinkWriteRejected`) and logged as a warning. Revisit with rewind-on-reject if the counter fires in practice while a listener is subscribed.
