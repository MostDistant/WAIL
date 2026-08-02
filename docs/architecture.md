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
    ├── interval (interval math: beat↔index mapping, window placement, the tempo de-noising bar)
    ├── capture  (interval assembler)
    ├── emit     (reassembler + paced sink reader)
    ├── playout  (adaptive per-sender round scheduler, ADR-0009)
    ├── lanloss  (Link Audio count-gap loss)
    └── affinity ((identity, stream) → stable published channel)

signaling-server/ (Go WebSocket relay server, deployed to fly.io)
├── interval_clock.go (room tempo/BPI state + interval_anchor broadcast — joiner seeding, ADR-0009)
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

- **Capture** (`internal/capture`): buffers are placed **sample-contiguously** — consecutive Link Audio buffers land back to back, and beat stamps only anchor the stream (first buffer, after a real discontinuity, or stamp divergence beyond 250ms). Trusting per-buffer stamps for position turns sender-side clock drift into zero-gaps punched into continuous audio (measured live 2026-07 as periodic clicks). Bounded drift is absorbed by a **micro-slew**: past a 10ms deadband, a buffer's 64-frame tail is linearly stretched/compressed by ≤4 frames — no splice, imperceptible. Capture **streams**: as coverage passes each 20ms window, the window is emitted for immediate Opus encoding + transmission (NINJAM-style — the interval is a playout concept, not a transmission one), so WAIF frames leave in real time *during* the interval. When a buffer opens the next interval it fills the old tail and the new head seamlessly; only genuine gaps read as silence. Streaming send means nearly the whole round has arrived before the receiver's next boundary releases it (ADR-0009 adaptive playout).
- **Send pacing** (`internal/pace`): defense in depth behind streaming capture. Normal operation emits single frames at real-time cadence, which pass through the pacer untouched; only abnormal batches (an interval-close flush after a capture stall, config edges) are spaced out at one frame per 10ms instead of bursting into the send queue or the relay's per-peer rate limiter.
- **Playback** (`internal/emit` + `internal/playout`): decoded frames are reassembled per sender round (the sender's own interval index — no shared numbering, ADR-0009) and realigned by the codec's algorithmic delay (`OPUS_GET_LOOKAHEAD` ≈ 6.5ms in `AppAudio` mode — left uncompensated, every transient lands that late on the room grid): the emit path reads each interval shifted by the lookahead (`emit.ShiftedPCM`), pulling the next interval's head for the tail. The adaptive scheduler (`playout.Adaptive`, ADR-0009) releases each sender's round at the receiver's next local boundary once it is ready (complete, or streaming with one boundary of age); when rounds back up, the freshest ready round plays and the stale queue is skipped at the speakers — still recorded, since the archive taps frames at receipt. One cursor per sender identity, shared across their streams, so a musician's mic and guitar can never split across rounds. A **cushioned feeder** keeps each Link Audio sink fed a bounded amount ahead of the wall-clock playhead (catch-up bursts after scheduler stalls, pre-roll across interval boundaries), so a Go/GC hiccup no longer reaches the ear; feed shortfalls past the cushion are counted as sink underruns — the honest audible-dropout metric. The cushion (`WAIL_EMIT_CUSHION_MS`, default **100ms**, clamped 100–500) is live-tunable from the Debug tab's *Playback* slider, so it can be dialed in against the underrun counters without a restart. It stamps audio into the future and some Link Audio receivers stall on that (a third-party DAW bridge was measured with ~0 tolerance); lower it toward the floor for intolerant receivers, raise it when a buffering-tolerant receiver (Ableton Live's native Link Audio) runs dry with underruns. An optional **WAIL Metronome** (Debug tab toggle) publishes a locally-generated click track — a beat on the local Link grid, accented on bar downbeats — as its own Link Audio channel (named `WAIL · <you> · Metronome`, carrying the room-channel prefix) through this same feeder/cushion/sink path (no relay round-trip); subscribing to it in a DAW alongside the DAW's metronome checks grid alignment by ear and surfaces emit-path glitches audibly. A separate **Broadcast metronome to room** toggle optionally streams that same click to the room as an in-app audio sender (`MetronomeSenderTask`, alongside the test tone / WAV sender): it renders each interval on the shared room grid, Opus-encodes it into WAIF frames, and paces them over the relay, so every peer republishes it as a `WAIL · <peer> · Metronome` channel and hears the identical grid one interval late (independent of the local channel — either or both). Late frames for the interval currently playing are **live-appended** (play-partial) rather than dropped or delayed a whole interval. A sequence gap in arrival order (TCP transport ⇒ permanent loss: reconnect gap or sender queue drop) is masked with **Opus packet-loss concealment**: up to 120ms per gap is synthesized in decode order, and a late real frame still replaces the concealment.
- **Channel affinity** (`internal/affinity`): each remote `(persistent identity, stream index)` maps to a stable published Link Audio channel. When a peer disconnects and reconnects (new session peer id, same identity), it reclaims the same channel/sink, so LAN apps' routing survives the blip. There is no fixed 15-slot cap — each remote stream is its own channel. **Retirement** is the counterpart: each sender's `StreamNames` sync is the authoritative list of what it is still sending, so a published stream missing from it (or belonging to a peer that left) is unpublished at an interval boundary once it has drained and stayed idle past a grace — 5s for a stream the sender dropped, 30s (`WAIL_STREAM_RETIRE_SEC`) for a departed peer, the longer one being what lets affinity hold a channel across a reconnect. The drained check matters: playout runs D boundaries behind the last frame, so retiring on idle time alone would truncate the final interval. Without retirement the published set only grew for a session's lifetime — dead channels kept publishing silence and held a port on every WAIL Receive on the LAN. A retired stream's diagnostic counters fold into an engine-level total so the cumulative `Health` numbers never dip (the session logs a counter only when it exceeds the previous snapshot, so a dip would silently swallow later events).
- **Own-channel exclusion** (`internal/affinity.OwnChannels`): capture discovery must skip WAIL's own republished channels (feedback loop) but must not hide a third-party publisher that merely shares WAIL's Link peer name. Since the sink channel ID isn't exposed by the `abl_link` C API, the classifier records every sink name WAIL mints and learns each sink's channel ID from the first discovery snapshot that pairs a minted name with WAIL's peer name; from then on exclusion is by ID (rename-proof). In addition, capture discovery excludes **any** channel whose name starts with the room prefix `WAIL · ` (`affinity.RoomChannelPrefix`), so *other* WAIL peers' republished channels — and the metronome — also stay out of the send-mixer; capturing one would re-relay already-relayed room audio (a cross-peer loop).

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
  → internal/playout releases the round at B's next local boundary once ready (adaptive)
  → internal/emit paces the interval into a LinkAudioSink (a published channel)
DAW / Link-Audio app B plays WAIL B's published channel — Peer A's most recent round
```

### Audio engine

`audio_engine.go` defines the `AudioEngine` interface; `audio_engine_real.go` (`//go:build !linkstub`) is the implementation, with a no-op `audio_engine_stub.go` for stub builds. The engine owns the capture drain goroutines (one per bridged channel), the emit loop (boundary detection + paced sink writes), and Link Audio channel discovery. It wraps the pure, unit-tested packages above plus the `internal/abllink` cgo binding. The realtime capture callback stays pure C (`internal/abllink/capture.c`) and never enters the Go runtime, so a GC pause can never drop incoming Link Audio UDP (ADR-0002).

### Optional CLAP bridge (WAIL Send / WAIL Receive, ADR-0007)

For DAWs that are Link *sync* peers but lack Link Audio (Live pre-12.3, Bitwig-class hosts), two first-party CLAP plugins (`plugins/wail_send.c`, `plugins/wail_recv.c`) make the DAW a Link Audio citizen outright. They carry **no room intelligence** — no codec, no intervals, no relay, no room clock — and require no app changes at all: each plugin instance joins the LAN Link session as its own audio-only peer, so the WAIL app sees it as an ordinary Link Audio channel.

- **WAIL Send:** publishes its track as a Link Audio channel, named from the DAW track (host `clap.track-info`, re-queried on `changed()`). The app's normal capture discovery picks it up; from there the usual capture → assemble → Opus → WAIF → relay path runs unchanged. Deliberately publishes *without* the `WAIL · ` room prefix, so the app captures it rather than excluding it as room content.
- **WAIL Receive:** subscribes only to channels carrying the full `WAIL · ` room prefix (`affinity.RoomChannelPrefix`) — the app's republished remote streams — and renders them onto 16 stereo output ports, renamed live to `{peer} · {stream}` via CLAP's `RESCAN_NAMES`. A manager thread polls channel discovery off the audio thread; `process()` only touches lock-free rings (ADR-0002). A port slot is keyed by **channel id, not name**: the app renames a room channel in place once the sender's stream name arrives, and a name-keyed slot took a second port per rename while never releasing the first (its id was still live, so the disappeared-channel reclaim never fired). Name matching survives only as a first-wins guard so two WAILs bridging one room don't double a port. The filter is the exact prefix for the same reason it is published that way — a looser `WAIL` match subscribes raw LAN channels, so a Send track named `WAIL Bass` came back as a receive port.
- The engine's `captureSource` / `emitSink` interfaces (`audio_engine_ports.go`) remain, but Link Audio (`*abllink.Source` / `*abllink.Sink`) is now their only implementation.

The C-facing Link session/audio API the plugins compile against (`plugins/wail_link.{h,cpp}`) mirrors the app's own `internal/abllink/wrap.cpp`.

> **History:** ADR-0005 shipped a different bridge under the same bundle IDs (`software.wail.send`/`software.wail.recv`, reused here) — raw PCM moved to and from the app over loopback TCP. Its floor was structural (the DAW's block pull bounded delivery lead), so ADR-0007 superseded it and the IPC path was removed. See `docs/adr/0005-clap-thin-pcm-bridge.md` for the reasoning.

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

Tempo crosses the WAN only as a **declaration** (`TempoDeclare`, ADR-0009): the value plus a monotonic priority stamp and an owner id, adopted iff strictly greater (lowest owner breaks exact ties) — Link's own timeline rule run WAN-side. Duplicates and echoes carry an equal stamp and are structurally inert, so this path has no echo guard and no thresholds. Three sources declare: WAIL's own tempo control (the UI field), an observed DAW change that survives the detector's de-noising bar (`SlewAuthorityBPM`, the 0.05% audibility bound), and the room founder's seed. Each dual-sends the legacy `TempoChange` for one beta compat cycle. Periodic `StateSnapshot` messages still flow for old receivers, but their tempo is no longer adopted — snapshot adoption was a 2-cycle oscillator waiting to happen (the 110↔120 field flap).

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

WAIL is a fully **passive** Link peer (ADR-0003, restored by ADR-0009): within-bar beat/phase alignment comes for free from Ableton Link's own LAN multicast sync, at the interval quantum WAIL asks for, and WAIL never moves anyone's grid — no `ForceBeat`, no tempo slew. Cross-LAN grids are never aligned; playout re-quantizes every round onto the listener's own grid, so cross-LAN phase never reaches the ear. `lan_peer_present` is informational only.

## Interval Boundaries

Each peer detects boundaries from its own local Link beat position:

```
local_interval_index = floor(beat_position / (bars × quantum))
```

Example: 4 bars × 4.0 quantum = 16 beats per interval. Beat 15.9 → interval 0. Beat 16.0 → interval 1.

The beat position is obtained from Link at quantum = **bars × quantum (BPI)**, not beats-per-bar. Link shares beat phase only mod the quantum you ask for; asking at the bar pinned `beat mod 4` but left `beat mod 16` — which bar starts the interval — per-peer arbitrary, so audio landed bar-aligned but whole bars apart. At the BPI lens the interval grid is session-shared, and it nests on bar boundaries (quantum grids share a common origin), matching a DAW whose launch quantization is set to the whole interval (ADR-0004). The same BPI quantum is used for capture buffer beat stamps (`BeginBeats`) and sink commits, keeping the (beat, quantum) pairs self-consistent end to end.

**Sender-relative rounds (ADR-0009):** there is no shared interval numbering. Senders stamp WAIF frames with their *own* local interval index — an opaque, monotonic per-sender sequence — and each receiver's adaptive scheduler plays whatever round is ready at its next boundary. Nothing ever compares a sender's index to a receiver's, so the whole class of label-offset bugs (off-by-one alignment, frozen wrong offsets, watchdog healing) is structurally impossible rather than defended against. The cost, accepted deliberately: no coordinated same-round changes ("everyone switch NOW" can land a round apart for different listeners — the NINJAM trade), and per-pair delay is grid luck within one interval rather than a fixed constant.

The relay still broadcasts an `interval_anchor`, but it is a config message now — the room's tempo and interval shape for joiner seeding (NINJAM's `ConfigChangeNotify`), with a vestigial zero index for wire-shape stability. The room clock and the label watchdog are gone; the relay tracks two plain values per room and forwards everything else opaquely (pillar 5, fully honored again). The relay also watches `TempoDeclare` alongside the legacy `TempoChange`, so clients can eventually stop dual-sending.

**Grid alignment is retired (ADR-0009).** WAIL never moves a Link session's grid or tempo: capture stores audio as beats-from-round-start on the sender's grid, playback starts each round on the listener's own boundary, so sender beat 3 lands on listener beat 3 whatever the absolute offset between their grids — cross-LAN phase never reaches the ear. The honest alignment requirement was always coarse (delivery headroom within the interval), not the 25 ms the retired ADR-0006 stack pursued; NINJAM has run this model with zero cross-client clock sync for twenty years. What survives of the alignment math is its audibility bound, `SlewAuthorityBPM` (0.05% = 0.86 cents), as the tempo detector's de-noising bar; and grid-*jump* detection survives as pure observability — a Link session merge or transport reset that moves the local timeline is attributed (peer-count and tempo evidence) and reported to the local and relay logs, because a musician whose bar lines just moved deserves the explanation, but nothing acts on it.

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
| `TempoDeclare` | sync | JSON | Tempo declaration: value + priority stamp + owner (ADR-0009) |
| `interval_anchor` | sync | JSON | Room tempo/config for joiners (server → clients; its index is ignored by ADR-0009 clients) |
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
| `update_streams` | Client → Server | Redeclare the send-stream count mid-session; the relay rescales the binary rate limit and room slot accounting. Acked with `update_streams_ok`, rejected with `update_streams_error` (`room_full`) |

## Key Design Decisions

1. **NINJAM over real-time**: 1-interval latency makes WAN jams possible without sub-20ms RTT. Musicians adapt to the delay.

2. **Binary WebSocket frames for audio**: Separate from the JSON "sync" text frames. Avoids base64 overhead and JSON parsing for large audio payloads.

3. **Opus codec**: Designed for interactive audio. 48kHz, configurable bitrate (64-128 kbps). Frame size = 960 samples (20ms).

4. **Poll-based Link monitoring** (50Hz): Polling is simpler than cross-thread callbacks, and 20ms is fast enough for tempo changes.

5. **Echo guard** (150ms): Prevents infinite tempo change ping-pong when applying remote changes to local Link.

6. **Server-relayed architecture**: All data flows through the signaling server (no direct P2P). This eliminates ICE/STUN/TURN negotiation complexity at the cost of an extra hop and server bandwidth scaling quadratically with room size.

7. **Stream-count-aware rate limiting**: Per-connection token bucket rate limiting on the signaling server. Binary message rate scales with `stream_count` (100 tokens/sec/stream, 2500-token burst/stream — a full interval of frames), text rate is fixed at 100/sec. Escalation: drop excess messages → warn log → send `rate_limit_warning` to client → disconnect after 50 cumulative violations. Join messages are exempt from text rate limiting so peers can always reconnect. Streams open and close mid-session (capture toggles, restore auto-enable, in-app senders), so the client recomputes its live send-stream count each status tick (enabled capture channels + test tone/WAV/metronome) and pushes `update_streams` when it drifts from the last declaration; the relay rescales the bucket in place (preserving tokens, so updates can't mint free capacity), re-checks room capacity, and resets the violation counter.

8. **Pure engine logic in `internal/` packages**: interval math, scheduler, loss, affinity, and interval assembly/reassembly are cgo- and network-free, so they are fully unit-tested; the cgo Link Audio layer and the relay wrap that proven logic.

9. **Link Audio is the only local audio interface** (ADR-0001/0002): no in-app plugin transport and no TCP IPC. Audio enters and leaves the process as Link Audio; the realtime capture callback is pure C with a lock-free ring so a Go GC pause can't drop UDP. The optional WAIL Send/Receive plugins (ADR-0007) are separate Link peers, not an app front end.

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
