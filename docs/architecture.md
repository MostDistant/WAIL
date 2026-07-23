# WAIL Architecture

## Overview

WAIL bridges Ableton Link sessions across the internet via a WebSocket relay server. Musicians on different networks sync tempo, phase, and interval boundaries as if they were on the same LAN. Audio is captured per interval (NINJAM-style), Opus-encoded, and transmitted as binary WebSocket frames through the server. WAIL is an Ableton **Link Audio** peer (ADR-0001): capture subscribes to local Link Audio channels, and playback republishes remote streams as Link Audio channels one interval late. There are no plugins and no IPC — the entire audio path runs inside the Go `wail-app` process (ADR-0002).

## System Diagram

```
┌──────────────────────────────────────┐                    ┌──────────────────────────────────────┐
│  Peer A Machine                      │                    │  Peer B Machine                      │
│                                      │                    │                                      │
│  ┌──────────────────────────────┐    │                    │    ┌──────────────────────────────┐  │
│  │  DAW / Link-Audio app        │    │                    │    │  DAW / Link-Audio app        │  │
│  │  (Ableton Live 12.3+, etc.)  │    │                    │    │  (Ableton Live 12.3+, etc.)  │  │
│  └──────────┬───────────────────┘    │                    │    └──────────┬───────────────────┘  │
│             │ Link Audio (LAN, UDP)  │                    │               │ Link Audio (LAN, UDP)│
│  ┌──────────┴───────────────────┐    │                    │    ┌──────────┴───────────────────┐  │
│  │  WAIL App                    │◄───┼─── WS Relay ───────┼───►│  WAIL App                    │  │
│  │  ├─ Link bridge (50Hz poll)  │    │  "sync" (JSON)     │    │  ├─ Link bridge (50Hz poll)  │  │
│  │  ├─ Link Audio capture       │    │  "audio" (binary)  │    │  ├─ Link Audio capture       │  │
│  │  └─ Link Audio emit (sink)   │    │                    │    │  └─ Link Audio emit (sink)   │  │
│  └──────────┬───────────────────┘    │                    │    └──────────┬───────────────────┘  │
│             │ Link (LAN multicast)   │                    │               │ Link (LAN multicast) │
│  ┌──────────┴───────────────────┐    │                    │    ┌──────────┴───────────────────┐  │
│  │  Ableton Live / Link app     │    │                    │    │  Ableton Live / Link app     │  │
│  └──────────────────────────────┘    │                    │    └──────────────────────────────┘  │
└──────────────────────────────────────┘                    └──────────────────────────────────────┘
                    │ WebSocket                                              │ WebSocket
                    │                                                        │
                    │              ┌──────────────────┐                      │
                    └─────────────►│  Relay Server    │◄─────────────────────┘
                                   │  (Go + SQLite)   │
                                   └──────────────────┘
```

The DAW and WAIL App are both Link peers on the same LAN: WAIL captures the app's Link Audio channels and publishes remote peers back as Link Audio channels the app can play.

## Dependency Graph

```
wail-app/ (Go/Wails desktop app — session orchestration, Link Audio engine, recording)
├── wailsapp/wails/v3 (desktop webview framework)
├── gorilla/websocket (WebSocket relay client)
├── internal/abllink (our own cgo binding to Ableton Link's abl_link C API:
│                     sync + Link Audio, compiled against vendor/link)
├── hraban/opus.v2 (Opus codec, cgo libopus)
├── go-audio/wav (headless WAV loading)
└── internal engine packages (pure, unit-tested):
    ├── interval (interval/room-clock math — RoomClock, RoomLabeler)
    ├── capture  (interval assembler)
    ├── emit     (reassembler + paced sink reader)
    ├── playout  (hold-until-N+D scheduler)
    ├── lanloss  (Link Audio count-gap loss)
    └── affinity ((identity, stream) → stable published channel)

signaling-server/ (Go WebSocket relay server, deployed to fly.io)
├── roomclock.go / interval_clock.go (relay-authoritative room interval clock + interval_anchor)
└── cmd/wail-metrics (CLI metrics client)
```

## The NINJAM Model

WAIL uses the NINJAM approach to intervalic audio. The core idea:

1. **Record** one full interval of local audio (e.g., 4 bars of 4/4 at 120 BPM = 8 seconds)
2. At the interval boundary, **transmit** the completed interval to all peers
3. Peers **play back** the received interval during the _next_ interval
4. Everyone hears everyone else delayed by exactly one interval

This means:
- **Latency = 1 interval** (e.g., 8 seconds at 4 bars / 120 BPM). This is by design, not a bug.
- **Sync is perfect** — all audio aligns to the same beat grid
- **Internet latency doesn't matter** as long as delivery completes within 1 interval
- Musicians adapt by playing "ahead" — the same mental model as NINJAM

### Why This Works for WAN

Traditional real-time audio requires <20ms round-trip latency. That's impossible over the internet. NINJAM sidesteps the problem: by accepting 1-interval latency, you can jam with anyone in the world. The music "works" because each interval is beat-aligned — you hear what the other person played last time, and play your response this time.

### The Double-Buffer

WAIL keeps the NINJAM double-buffer in pure Go: capture accumulates the current interval while playback emits the previous one, per remote stream.

```
Interval N:   [CAPTURE local Link Audio] ──→ on boundary ──→ Opus-encode + transmit
              [EMIT remote audio for room interval N-D]

Interval N+1: [CAPTURE local Link Audio] ──→ on boundary ──→ Opus-encode + transmit
              [EMIT remote audio for room interval N+1-D]
```

- **Capture** (`internal/capture`): buffers are placed **sample-contiguously** — consecutive Link Audio buffers land back to back, and beat stamps only anchor the stream (first buffer, after a real discontinuity, or stamp divergence beyond 250ms). Trusting per-buffer stamps for position turns sender-side clock drift into zero-gaps punched into continuous audio (measured live 2026-07 as periodic clicks). Bounded drift is absorbed by a **micro-slew**: past a 10ms deadband, a buffer's 64-frame tail is linearly stretched/compressed by ≤4 frames — no splice, imperceptible. Capture **streams**: as coverage passes each 20ms window, the window is emitted for immediate Opus encoding + transmission (NINJAM-style — the interval is a playout concept, not a transmission one), so WAIF frames leave in real time *during* the interval. When a buffer opens the next interval it fills the old tail and the new head seamlessly; only genuine gaps read as silence. With the default offset `D=1`, every frame arrives roughly a full interval before the receiver's playout boundary needs it.
- **Send pacing** (`internal/pace`): defense in depth behind streaming capture. Normal operation emits single frames at real-time cadence, which pass through the pacer untouched; only abnormal batches (an interval-close flush after a capture stall, config edges) are spaced out at one frame per 10ms instead of bursting into the send queue or the relay's per-peer rate limiter.
- **Playback** (`internal/emit` + `internal/playout`): decoded frames are reassembled per room interval index; the hold-until-boundary scheduler releases interval `N` at the local boundary labeled `N+D` (offset D, default 1). A **cushioned feeder** keeps each Link Audio sink fed ~80ms ahead of the wall-clock playhead (catch-up bursts after scheduler stalls, pre-roll across interval boundaries), so a Go/GC hiccup no longer reaches the ear; feed shortfalls past the cushion are counted as sink underruns — the honest audible-dropout metric. The cushion default (`WAIL_EMIT_CUSHION_MS`) is live-tunable from the Debug tab's *Playback* slider (10–500ms), so it can be dialed in against the underrun counters without a restart. An optional **WAIL Metronome** (Debug tab toggle) publishes a locally-generated click track — a beat on the local Link grid, accented on bar downbeats — as its own Link Audio channel through this same feeder/cushion/sink path (no relay round-trip); subscribing to it in a DAW alongside the DAW's metronome checks grid alignment by ear and surfaces emit-path glitches audibly. Late frames for the interval currently playing are **live-appended** (play-partial) rather than dropped or delayed a whole interval. A sequence gap in arrival order (TCP transport ⇒ permanent loss: reconnect gap or sender queue drop) is masked with **Opus packet-loss concealment**: up to 120ms per gap is synthesized in decode order, and a late real frame still replaces the concealment.
- **Channel affinity** (`internal/affinity`): each remote `(persistent identity, stream index)` maps to a stable published Link Audio channel. When a peer disconnects and reconnects (new session peer id, same identity), it reclaims the same channel/sink, so LAN apps' routing survives the blip. There is no fixed 15-slot cap — each remote stream is its own channel.
- **Own-channel exclusion** (`internal/affinity.OwnChannels`): capture discovery must skip WAIL's own republished channels (feedback loop) but must not hide a third-party publisher that merely shares WAIL's Link peer name. Since the sink channel ID isn't exposed by the `abl_link` C API, the classifier records every sink name WAIL mints and learns each sink's channel ID from the first discovery snapshot that pairs a minted name with WAIL's peer name; from then on exclusion is by ID (rename-proof).

## Audio Flow

### Full Path (Link Audio → Network → Link Audio)

```
DAW / Link-Audio app A publishes a Link Audio channel on LAN A
  → WAIL A subscribes (LinkAudioSource); pure-C callback → C ring → Go drainer
  → internal/capture buckets buffers into a fixed-length interval (local index),
    emitting each 20ms window as its audio arrives
  → interval_codec.go Opus-encodes the window → one WAIF frame (labeled with the room
    index), sent immediately — frames stream in real time during the interval
  → WebSocket binary frame → relay server → Peer B
  → WAIL B receives WAIF
  → interval_codec.go Opus-decodes; internal/emit reassembles per (identity, stream index)
  → internal/playout holds the interval until the local boundary labeled N+D
  → internal/emit paces the interval into a LinkAudioSink (a published channel)
DAW / Link-Audio app B plays WAIL B's published channel — Peer A's previous interval
```

### Audio engine

`audio_engine.go` defines the `AudioEngine` interface; `audio_engine_real.go` (`//go:build !linkstub`) is the implementation, with a no-op `audio_engine_stub.go` for stub builds. The engine owns the capture drain goroutines (one per bridged channel), the emit loop (boundary detection + paced sink writes), and Link Audio channel discovery. It wraps the pure, unit-tested packages above plus the `internal/abllink` cgo binding. The realtime capture callback stays pure C (`internal/abllink/capture.c`) and never enters the Go runtime, so a GC pause can never drop incoming Link Audio UDP (ADR-0002).

### Wire Format (AudioFrameWire / WAIF)

Streaming format: one WAIF frame per 20ms Opus packet. The final frame of an interval carries metadata so the receiver can reconstruct the full interval. When the interval length isn't a multiple of the 20ms window (most tempos), the final frame's tail is padded with the next interval's real head samples — never silence — so the encoder's input stays continuous across the boundary; the receiver plays through that padding and starts the next interval past its twice-encoded head (silent padding from older senders falls back to truncating playout at the exact interval end).

```
[4 bytes]  magic: "WAIF"
[1 byte]   flags: bit 0 = stereo, bit 1 = final (last frame of interval)
[2 bytes]  stream_id: u16 LE
[8 bytes]  interval_index: i64 LE
[4 bytes]  frame_number: u32 LE (0-indexed within interval)
[4 bytes]  frame_seq: u32 LE (monotonic per (sender, stream_id); used for loss detection)
[2 bytes]  opus_len: u16 LE
[N bytes]  opus_data

If final flag set, append:
[4 bytes]  sample_rate: u32 LE
[4 bytes]  total_frames: u32 LE
[8 bytes]  bpm: f64 LE
[8 bytes]  quantum: f64 LE
[4 bytes]  bars: u32 LE
```

On the receiver side, `internal/emit.Reassembler` collects decoded WAIF frames per `(remote identity, stream index)` and reassembles them into interval PCM (out-of-order tolerant; the final frame carries the total; missing frames read as silence). WAIF frames now carry the shared *room* interval index (relay clock), so there is no per-peer index remap.

## Tempo Sync Flow

```
1. User changes tempo in DAW A
2. Link broadcasts on LAN
3. WAIL App A Link bridge detects change (50Hz poll)
4. Echo guard check: was this our own recent remote-applied change?
5. If genuine local change → serialize as SyncMessage::TempoChange
6. Broadcast via PeerMesh → WebSocket relay server → all room peers (JSON)
7. Remote peers receive, parse, apply to their local Link via set_tempo()
8. Echo guard activated on remote to prevent re-broadcast loop
9. Remote DAWs see tempo change via Link
```

## Connection Establishment

```
1. Peer connects to WebSocket signaling server, sends join (with room password, stream_count, client_version)
   - Server rejects outdated clients with join_error code "version_outdated"
2. Server replies with join_ok containing list of existing peers and lan_peer_present flag
3. All sync and audio data flows through the server (no direct P2P connections)
   - Sync: JSON text frames relayed via "sync" / "sync_to" message types
   - Audio: binary WebSocket frames relayed with sender header prepended by server
   - Loopback (debug): a "set_loopback" message opts the sender into receiving its
     own audio frames back from the relay (off by default, reset on rejoin); the
     client republishes them as a "(loopback)" Link Audio channel one interval late
```

### LAN Peer Detection

The signaling server records each client's public IP (from `Fly-Client-IP`, `X-Forwarded-For`, or `RemoteAddr`). When a peer joins a room, the server checks whether any existing peer shares the same public IP — indicating they are on the same LAN. The `join_ok` response includes a `lan_peer_present` boolean.

WAIL is a **passive** Link peer (ADR-0003): it never `ForceBeat`s the local transport — beat/phase alignment comes for free from Ableton Link's own LAN multicast sync, at the interval quantum WAIL asks for. `lan_peer_present` is now informational only.

## Interval Boundaries

Each peer detects boundaries from its own local Link beat position:

```
local_interval_index = floor(beat_position / (bars × quantum))
```

Example: 4 bars × 4.0 quantum = 16 beats per interval. Beat 15.9 → interval 0. Beat 16.0 → interval 1.

The beat position is obtained from Link at quantum = **bars × quantum (BPI)**, not beats-per-bar. Link shares beat phase only mod the quantum you ask for; asking at the bar pinned `beat mod 4` but left `beat mod 16` — which bar starts the interval — per-peer arbitrary, so audio landed bar-aligned but whole bars apart. At the BPI lens the interval grid is session-shared, and it nests on bar boundaries (quantum grids share a common origin), matching a DAW whose launch quantization is set to the whole interval (ADR-0004). The same BPI quantum is used for capture buffer beat stamps (`BeginBeats`) and sink commits, keeping the (beat, quantum) pairs self-consistent end to end.

**Relay-authoritative room index (ADR-0003):** the relay server owns the room interval index and broadcasts an `interval_anchor` (index + tempo/config + server time). Each client maps its *local* index to the shared *room* index via a constant offset established from the anchor (`internal/interval.RoomLabeler`). Every WAIL agrees on the room index by construction; WAIF frames are tagged with it, and the receiver's hold-until-boundary scheduler releases interval `N` at the local boundary labeled `N+D`.

**Room interval length (ADR-0004):** the interval length is shown to users as BPI (beats per interval = bars × beats per bar), but the wire model stays bars × quantum. The first peer in a room anchors its config; joiners adopt it, and anyone can change it mid-jam (`IntervalConfig` broadcast → relay reanchors at the next boundary). Peers always re-broadcast the *adopted* config, never their join-time preference — the relay's last-writer-wins gossip would otherwise flap the room clock. A DAW's launch quantization can't be read or set over Link, so alignment guidance is communicated (join prompt + display), never enforced.

**WAN peers' boundaries are NOT synchronized in wall-clock.** Peer A might cross into interval 1 while Peer B is still in interval 0. This is fine for NINJAM semantics — you always play the _previous_ interval, so drift up to 1 full interval is tolerable. As long as the wire data arrives before the receiver's `N+D` boundary, it plays on time; late frames live-append.

## Clock Domains

Two independent time domains exist in the system:

1. **Link clock** (`link.clock_micros()`): Ableton Link's internal monotonic clock, used for beat/phase synchronization. This is the authoritative clock for interval boundaries.

2. **ClockSync epoch** (`std::time::Instant::now()`): Used by WAIL's Ping/Pong protocol to measure peer-to-peer RTT. Clock offset computation was removed — these two clocks are different domains and cannot be combined. ClockSync RTT is useful for diagnostics (displaying latency to peers) but does not participate in interval boundary calculations.

## Sync Protocol Messages

| Message | Channel | Format | Purpose |
|---------|---------|--------|---------|
| `Ping` | sync | JSON | Clock sync request |
| `Pong` | sync | JSON | Clock sync response |
| `TempoChange` | sync | JSON | BPM change from local Link |
| `StateSnapshot` | sync | JSON | Periodic full state (every 200ms) |
| `IntervalConfig` | sync | JSON | Agree on interval bars/quantum |
| `Hello` | sync | JSON | Greeting on connect |
| `AudioCapabilities` | sync | JSON | Announce send/receive support |
| `AudioIntervalReady` | sync | JSON | Metadata before binary audio |
| `StreamNames` | sync | JSON | Human-readable names for sender's audio streams |
| `interval_anchor` | sync | JSON | Relay-authoritative room interval index + tempo/config (server → clients, ADR-0003) |
| _(binary audio)_ | audio | WAIF (AudioFrameWire) | Opus-encoded streaming frames |

## Signaling Protocol Messages

| Message | Direction | Purpose |
|---------|-----------|---------|
| `Join` | Client → Server | Join a named room (includes `stream_count`, `client_version`) |
| `PeerList` | Server → Client | Current room members |
| `PeerJoined` | Server → Client | New peer notification |
| `PeerLeft` | Server → Client | Peer disconnect notification |
| `Sync` | Client → Server → Room | Relay sync message to all room peers |
| `SyncTo` | Client → Server → Client | Relay sync message to a specific peer |
| `LogBroadcast` (`log`) | Client → Server → Room | Broadcast structured log entry to all room peers (opt-in) |
| `MetricsReport` | Client → Server | Per-peer audio frame counts + pipeline state (consumed server-side, not relayed) |
| `rate_limit_warning` | Server → Client | Warning that the peer is sending too fast and will be disconnected if it continues |

## Key Design Decisions

1. **NINJAM over real-time**: 1-interval latency makes WAN jams possible without sub-20ms RTT. Musicians adapt to the delay.

2. **Binary WebSocket frames for audio**: Separate from the JSON "sync" text frames. Avoids base64 overhead and JSON parsing for large audio payloads.

3. **Opus codec**: Designed for interactive audio. 48kHz, configurable bitrate (64-128 kbps). Frame size = 960 samples (20ms).

4. **Poll-based Link monitoring** (50Hz): Polling is simpler than cross-thread callbacks, and 20ms is fast enough for tempo changes.

5. **Echo guard** (150ms): Prevents infinite tempo change ping-pong when applying remote changes to local Link.

6. **Server-relayed architecture**: All data flows through the signaling server (no direct P2P). This eliminates ICE/STUN/TURN negotiation complexity at the cost of an extra hop and server bandwidth scaling quadratically with room size.

7. **Stream-count-aware rate limiting**: Per-connection token bucket rate limiting on the signaling server. Binary message rate scales with `stream_count` (60 tokens/sec/stream, 2× burst), text rate is fixed at 100/sec. Escalation: drop excess messages → warn log → send `rate_limit_warning` to client → disconnect after 50 cumulative violations. Join messages are exempt from text rate limiting so peers can always reconnect.

8. **Pure engine logic in `internal/` packages**: interval math, scheduler, loss, affinity, and interval assembly/reassembly are cgo- and network-free, so they are fully unit-tested; the cgo Link Audio layer and the relay wrap that proven logic.

9. **Link Audio is the only local audio interface** (ADR-0001/0002): no CLAP/VST3 plugins and no TCP IPC. Audio enters and leaves the process as Link Audio; the realtime capture callback is pure C with a lock-free ring so a Go GC pause can't drop UDP.

10. **JSON sync protocol**: Readable for debugging. Bandwidth is negligible for small sync messages.

11. **Stable channel affinity via `(identity, stream)`**: each remote stream is keyed by `(persistent identity UUID, stream index)` and mapped to a stable published Link Audio channel (`internal/affinity`). On reconnect with the same identity, the same channel/sink is reused, so LAN apps' routing survives brief network interruptions.

12. **Local session recording**: Sessions can be recorded to WAV files — either a single mixed file or per-peer stems. Managed by `recorder.go` in wail-app.

13. **Fade-in on peer join**: When a new or reconnecting peer's first audio interval arrives, a 10ms linear ramp-from-silence is applied before mixing into the playback buffer. This prevents audible pops/clicks caused by abrupt sample onset. The fade length is clamped to the interval length for safety. After the first interval, subsequent intervals play at full amplitude with no ramping.

14. **Lock-free audio broadcast via copy-on-write**: The signaling server uses per-room `atomic.Pointer[[]connEntry]` snapshots so the audio hot path (~50 frames/sec/peer) iterates the connection list without holding any lock. Mutations (join/leave) acquire the per-room `r.mu`, rebuild the slice, and store it atomically. To avoid data races on `conn.room`/`conn.peerID` fields (which are cleared during eviction and leave), broadcast functions receive `room` and `peerID` as value parameters captured once per join in `readPump`, rather than reading them from the shared `conn` struct.

15. **Headless CLI mode with pluggable emitter**: The WAIL app supports a `-headless` flag that bypasses the Wails GUI entirely. A `NoopEmitter` satisfies the `EventEmitter` interface (logging events instead of dispatching to a webview), decoupling session orchestration from the frontend. The `WavSenderTask` goroutine (and the GUI test tone) provide a built-in audio source that Opus-encodes 20ms frames and pushes WAIF straight to the relay — the in-app Go test client, enabling automated/scripted participation in a room without a DAW.

## Session Metrics and Live Dashboard

The signaling server tracks aggregate session metrics to monitor whether audio is flowing between peers.

### Session model

A **session** starts when the 2nd peer joins a room (≥2 peers) and ends when the count drops below 2. Sessions have two phases:

1. **Joining** — from session start until all peers report `dc_open` and `plugin_connected`. Captures connection establishment.
2. **Playing** — steady-state audio flow after all peers are fully connected.

> **Note:** Both field names are WebRTC/plugin-era holdovers kept for protocol compatibility. `dc_open` is always `true` once the WebSocket connects; `plugin_connected` is now always `true` (the Link Audio engine is the audio path, so there is no separate plugin to attach). The joining→playing transition is therefore effectively immediate once peers connect.

### Per-direction metrics

For each unique direction (e.g., `peer1→peer2`), the server tracks metrics independently per phase (joining vs playing). This distinguishes setup-related issues from steady-state network quality.

**Frame-level metrics:**
- `frames_expected` / `frames_received` / `frames_dropped` — tracked via zero-copy WAIF header parsing (`PeekWaifHeader`) in `session.go` as frames pass through. Each "frame" is a single 20ms WAIF streaming Opus packet. WAN relay loss is `packet_loss.go` (WAIF frame-seq gaps); Link Audio LAN capture loss is `internal/lanloss` (per-buffer count gaps).
- `boundary_drift_us` — interval boundary timing drift (actual − expected gap, µs)

**Network health metrics (per direction):**
- `rtt_us` — median RTT to the peer (µs), from `ClockSync` Ping/Pong
- `jitter_us` — mean absolute deviation from median RTT (µs), the key signal for intermittent issues
- `late_frames` — WAIF frames that arrived for already-passed intervals (detected in `session.go`)
- `decode_failures` — Opus decode failures on the playback path

The former `ipc_drops` (plugin → app IPC channel-full) is retired with IPC; the equivalent now is local-sender WAIF drops (in-app test tone / WAV feeding the relay).

Clients report cumulative per-peer metrics every 2 seconds via a `metrics_report` message on the signaling WebSocket. The server computes playing-phase deltas by snapshotting cumulative values at the joining→playing transition. Point-in-time values (RTT, jitter) are overwritten with the latest report.

### Endpoints

| Path | Description |
|------|-------------|
| `GET /metrics` | JSON snapshot of active + completed sessions (`?room=` filter supported) |
| `WS /metrics/ws` | Streaming metrics every 2s (`?room=` filter supported) |
| `GET /metrics/dashboard` | Live HTML dashboard with auto-reconnecting WebSocket |

### CLI tool

`signaling-server/cmd/wail-metrics/` queries the `/metrics` endpoint:

```sh
wail-metrics -server https://signal.wail.live -room my-room
wail-metrics -json   # raw JSON
```

## CI/CD Pipeline

Every push to `main` triggers continuous deployment:

1. `auto-release.yml` → consumes `.changeset/` files and conventional commits, bumps versions, updates CHANGELOG, creates a release PR, auto-merges it, then runs `knope release` (creates GitHub release + git tag) and dispatches artifact builds
2. `release.yml` → builds platform artifacts (macOS, Windows, Linux — the Go `wail` app built via cgo against `vendor/link`, plus a Homebrew source tarball) and uploads them to the GitHub release

The release and artifact dispatch steps run inline in `auto-release.yml` because `GITHUB_TOKEN` merges don't trigger other workflows. `release-on-merge.yml` remains as a fallback for manual merges of release PRs.

> **Note:** The auto-merge step uses `GITHUB_TOKEN` which cannot bypass branch protection rules. If required status checks or PR review requirements are ever added to `main`, the auto-merge will fail and you'll need a GitHub App token with bypass permissions, or exempt the `release` branch from those rules.
