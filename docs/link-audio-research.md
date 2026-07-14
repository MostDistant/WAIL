# Link Audio (Ableton Link 4.0) — deep dive and WAN-bridge feasibility

Research into the Link Audio API introduced in Ableton Link 4.0, based on the vendored SDK
at `vendor/link` (submodule pinned to tag `Link-4.0.0b1`, commit `082691b`). The upstream
repo also carries tags `Link-4.0.0b2`–`b4` and the final `Link-4.0`; deltas that matter are
called out explicitly. All `file:line` citations below are relative to `vendor/link/` at the
vendored `Link-4.0.0b1` checkout unless a tag is named.

Related: general Link 4 research lives in `docs/link-4-research.md` (separate document).

## TL;DR

- **Link Audio is LAN-only, UDP unicast, uncompressed 16-bit PCM, fire-and-forget.** Audio
  buffers are sent as ≤576-byte UDP datagrams with **no ACK, no retransmission, and no
  recovery** — only a per-chunk sequence counter that lets a receiver *detect* loss. This is
  best-effort transport, not the bulletproof capture WAIL's NINJAM model requires.
- **The timing model is a great fit for WAIL.** Every buffer carries a session-relative beat
  timestamp plus tempo; publishers supply `beatsAtBufferBegin` themselves at commit time
  (mechanically arbitrary), and consumers map buffer beats into their local timeline and
  choose their own playback latency (the reference renderer plays 4 beats behind "now").
  Nothing prevents WAIL from republishing audio offset by exactly one NINJAM interval.
- **The API affordances for a bridge all exist**: channel discovery with stable IDs and
  display names, subscribe-to-activate semantics (sinks transmit only while somebody
  subscribes), push-based receive callbacks with full metadata, and a C API (`abl_link`, at
  tag `Link-4.0`) that `rusty_link`/`abletonlink-go`-style bindings can wrap.
- **Verdict**: WAIL can act as a Link Audio WAN bridge — subscribe on LAN A, ship intervals
  over the existing relay, republish on LAN B one interval late — but **capture via Link
  Audio is lossy by construction** (UDP on the local network, 16-bit quantization), so it
  can only be a zero-install convenience mode, not a replacement for the in-DAW Send plugin,
  which remains the only lossless-by-construction capture path.
- **Licensing is unchanged**: dual GPLv2+ / proprietary (contact Ableton). No Link
  Audio-specific carve-out. The vendored pin (`4.0.0b1`) predates several correctness fixes
  (peer-name buffer overrun, source-only peers unable to receive audio); bump the submodule
  to `Link-4.0` before building anything on it.

---

## 1. API surface (cited facts)

### 1.1 The `LinkAudio` class

`ableton::LinkAudio` extends `BasicLink` and replaces `ableton::Link` ("LinkAudio and Link
should not be used simultaneously", `include/ableton/LinkAudio.hpp:39-40`). It adds:

- Constructor takes an initial tempo **and a peer name** for identification in the session
  (`LinkAudio.hpp:58-61`). At `Link-4.0` names are truncated to 256 chars
  (`kMaxNameSize`, `link_audio/v1/Messages.hpp` at tag `Link-4.0`).
- `enableLinkAudio(bool)` / `isLinkAudioEnabled()` — audio sharing is opt-in on top of an
  enabled Link instance (`LinkAudio.hpp:65-75`).
- `setChannelsChangedCallback(cb)` — fires on a Link-managed thread when channels appear,
  disappear, or are renamed (`LinkAudio.hpp:83-93`).
- `channels()` returns `vector<Channel>` where `Channel = {ChannelId id, string name,
  PeerId peerId, string peerName}`; IDs are "persistent for the lifetime of a channel",
  names are display-only and mutable (`LinkAudio.hpp:95-113`). `ChannelId`/`PeerId`/
  `SessionId` are all 8-byte random node IDs (`link_audio/Id.hpp:29`,
  `link/NodeId.hpp:34`, `link_audio/ApiConfig.hpp:46-48`).
- `callOnLinkThread(fn)` — e.g. to raise the Link thread's priority before enabling audio,
  as LinkAudioHut does (`LinkAudio.hpp:115-124`, `examples/linkaudiohut/main.cpp:244-254`).

Sample format is fixed by the API contract: "Audio buffers are interleaved and samples are
represented as 16-bit signed integers" (`LinkAudio.hpp:51`).

### 1.2 Publishing: `LinkAudioSink`

- Constructed with the `LinkAudio` instance, a channel name, and `maxNumSamples` (channels ×
  frames of one audio callback) (`LinkAudio.hpp:158-164`). Each sink announces exactly one
  channel; an app creates multiple sinks for multiple channels (`MainProcessor` holds a
  `vector` of sink processors and builds one `ChannelAnnouncement` per sink,
  `link_audio/MainProcessor.hpp:191-200`).
- Writing is a two-phase, realtime-safe handshake:
  1. `BufferHandle handle(sink)` retains an internal queue slot; the handle is invalid
     ("`operator bool` false") **if no corresponding source exists** or no slot is free
     (`LinkAudio.hpp:199-228`). Internally `Sink::retainBuffer()` returns `nullptr` unless
     `mIsConnected` — i.e. at least one remote peer has an active channel request
     (`link_audio/Sink.hpp:58-79`, set in `SinkProcessor.hpp:126-131`). "Sinks only send
     audio if at least one corresponding source is present in the session"
     (`LinkAudio.hpp:50`).
  2. Write `int16_t` samples into `handle.samples` and call
     `handle.commit(sessionState, beatsAtBufferBegin, quantum, numFrames, numChannels,
     sampleRate)` (`LinkAudio.hpp:249-255`). Commit validates `numChannels == 1 || 2` and
     `numFrames * numChannels <= maxNumSamples` (`LinkAudio.ipp:153-154`).
- The commit doc states the session state, quantum, and begin beats "must be same as used
  for rendering the audio locally" (`LinkAudio.hpp:245-247`) — but the value is a plain
  caller-supplied `double`; nothing validates it against the clock (see §6).
- The sink's internal queue holds 128 slots (`Sink.hpp:41`); commit converts the local beat
  to a session-global beat via `globalBeatAtBeat(timeline, beats, quantum)` and stamps the
  buffer with tempo, channels, sample rate, frames, and session ID
  (`Sink.hpp:81-104`, `link_audio/BeatTimeMapping.hpp:31-38`).

### 1.3 Subscribing: `LinkAudioSource`

- Constructed with the `LinkAudio` instance, a `ChannelId` (from `channels()`), and a
  callback "invoked on a Link-managed thread when a buffer is received"
  (`LinkAudio.hpp:279-286`). Receive is **push-based**; there is no pull/poll API.
- The callback gets a `BufferHandle{int16_t* samples, Info info}` where `Info` carries:
  `numChannels`, `numFrames`, `sampleRate`, `count` (sequence number), `sessionBeatTime`
  (session-global beat at buffer begin), `tempo` (BPM), and `sessionId`
  (`LinkAudio.hpp:304-342`).
- `Info::beginBeats(sessionState, quantum)` / `endBeats(...)` map the buffer's beat range
  into the local timeline; both return `nullopt` if the buffer's `sessionId` differs from
  the local timeline's session (`LinkAudio.ipp:175-207`). `endBeats` is derived from
  `numFrames / sampleRate` at the buffer's `tempo` (`LinkAudio.ipp:200-206`).
- Creating a source starts a subscription loop: a `ChannelRequest` is sent to the channel's
  peer immediately and re-sent every 5 s (`kTtl = 5`,
  `link_audio/SourceProcessor.hpp:41,108-122`); destroying the source sends a
  `ChannelStopRequest` (`SourceProcessor.hpp:53-59,124-128`). On the publisher, receivers
  expire if no fresh request arrives within the TTL (`link_audio/Receivers.hpp:81-130`).

### 1.4 What LinkAudioHut does end-to-end

`examples/linkaudiohut/main.cpp` + `examples/linkaudio/LinkAudioRenderer.hpp`:

1. **Setup**: constructs `ableton::LinkAudio link(120., name)` and an audio platform whose
   engine owns a `LinkAudioRenderer` (`main.cpp:38-53`). The renderer creates one sink
   named `"A Sink"` with `maxNumSamples = 4096` at construction
   (`LinkAudioRenderer.hpp:68-76`). Registers a channels-changed callback that reprints the
   channel list (`main.cpp:293`).
2. **Enable**: pressing `c` raises the Link thread priority via `callOnLinkThread` then
   calls `enableLinkAudio(true)` (`main.cpp:242-255`).
3. **Discovery/subscribe**: pressing `o` lists `link.channels()` and creates a
   `LinkAudioSource` for the chosen `ChannelId` (`main.cpp:150-194`,
   `LinkAudioRenderer.hpp:341-348`).
4. **Audio callback (send)**: each callback captures the audio session state, commits any
   tempo/transport changes, renders a metronome into the left buffer, then calls the
   renderer (`examples/linkaudio/AudioEngine.ipp:210-267`). `send()` retains a sink buffer,
   converts `double` → `int16` (`util::floatToInt16`), computes `beatsAtBufferBegin =
   sessionState.beatAtTime(hostTime, quantum)`, and commits as mono at the device sample
   rate (`LinkAudioRenderer.hpp:80-106`).
5. **Receive path**: the source callback (`onSourceBuffer`) runs on the Link thread — the
   comment warns "we should not block it for too long" — and copies samples + metadata into
   a lock-free SPSC `Queue` (2048 slots × 512-sample buffers; 512 "should at least hold max
   network buffer size") (`LinkAudioRenderer.hpp:60-65,73-75,350-368`).
6. **Audio callback (render)**: `receive()` drains the queue and renders the remote channel
   **4 beats behind the local beat** (`constexpr auto kLatencyInBeats = 4`,
   `LinkAudioRenderer.hpp:121-123`): it computes the target beat window for the output
   buffer, drops queued buffers that are entirely older than the window, waits (renders
   silence) if the next buffer is too new or too little is buffered, then resamples with
   cubic interpolation — "This automatically handles both tempo changes and sample rate
   differences by re-pitching the audio" (`LinkAudioRenderer.hpp:108-298`, re-pitch comment
   at 229-232). It also reports how many seconds are buffered (`main.cpp:118`).
7. **Teardown**: `removeSource()` resets the source (sends the stop request) and drains the
   queue (`LinkAudioRenderer.hpp:319-337`); the `LinkAudio` destructor clears callbacks and
   the session controller stops audio before shutting down
   (`LinkAudio.ipp:40-44`, `link_audio/SessionController.hpp:74-78`).

### 1.5 C API

At tag `Link-4.0`, the `abl_link` C extension exposes the full audio API:
`abl_link_audio_enable_link_audio`, `abl_link_audio_get_channels`,
`abl_link_audio_set_channels_changed_callback`, `abl_link_audio_sink_create` /
`_retain_buffer` / `_buffer_commit`, `abl_link_audio_source_create` (callback-based,
"Neither the buffer pointer nor its samples pointer remain valid after the callback
returns"), and beat-mapping helpers `abl_link_audio_source_buffer_info_begin_beats` /
`_end_beats` (`extensions/abl_link/include/abl_link.h` at `Link-4.0`; commit `1380ccb`
"Extend abl_link with an audio API"). This is the natural FFI layer for WAIL:
`rusty_link` wraps `abl_link` (`crates/wail-core/Cargo.toml:7`) and `wail-app` uses
`abletonlink-go` (`wail-app/go.mod:8`) — both would need updating to a Link 4 + audio-aware
binding, or WAIL would bind `abl_link.h` directly.

## 2. Timing model (cited facts)

- **Beat-domain timestamps, not wall clock.** A committed buffer's local beat is converted
  to a session-global beat (`globalBeatAtBeat`) on the sender and mapped back into the
  receiver's local timeline (`beatAtGlobalBeat`) (`BeatTimeMapping.hpp:31-47`,
  `Sink.hpp:94-96`, `LinkAudio.ipp:175-207`). Tempo is carried per chunk so the receiver
  can compute the buffer's beat *extent* (`AudioBuffer.hpp:97-100`,
  `LinkAudio.ipp:200-206`).
- **Publisher controls the timestamp.** `commit(...)` takes `beatsAtBufferBegin` as an
  argument (`LinkAudio.hpp:249-255`); the API's contract is that it matches what was
  rendered locally (`LinkAudio.hpp:245-247`) but the value is not checked against any
  clock — a publisher *can* inject audio at an arbitrary (e.g. one-interval-delayed)
  timeline position. There is no send-side scheduling: buffers are encoded and put on the
  wire within ~1 ms of commit (`MainProcessor::kProcessTimerPeriod = 1ms`,
  `MainProcessor.hpp:47`; `SinkProcessor::process` drains the sink queue each tick,
  `SinkProcessor.hpp:96-117`).
- **Receive is push; playback policy is the consumer's.** Buffers are delivered to the
  source callback as datagrams arrive; the SDK imposes no playout schedule. The reference
  consumer buffers and plays 4 beats behind local "now", dropping too-old buffers and
  stalling on too-new ones (`LinkAudioRenderer.hpp:121-164`). So a consumer *can* read the
  timestamp and render at any offset it likes; equally, that policy is per-app and not
  standardized (what Ableton Live does is not visible in this SDK).
- **Cross-session guard**: beat mapping returns `nullopt` when the buffer's `sessionId`
  differs from the local timeline's session (`LinkAudio.ipp:179-183`) — raw
  `sessionBeatTime`/`tempo` remain readable in `Info` regardless (`LinkAudio.hpp:309-317`).
- **Latency compensation** for the *local* audio device is the app's job, as with classic
  Link (host-time filters + output-latency offset; `README.md:119-155`,
  `examples/linkaudio/AudioPlatform_*.ipp`). A dead constant `kSourceQueueSize = 10s`
  exists in `Source.hpp:35` but is unused at both `b1` and `Link-4.0` — receive-side
  buffering is entirely the app's responsibility.

## 3. Transport, scope, and reliability (cited facts)

- **Discovery bootstrap rides on classic Link discovery.** Each peer's Link discovery
  payload now advertises an optional **audio endpoint** (`AudioEndpointV4`/`V6` payload
  entries, `link/PeerState.hpp:49-54,77-92,129`). When a peer with an audio endpoint is
  seen, it's registered as a potential receiver (`SessionController.hpp:116-121`,
  `link_audio/UdpMessenger.hpp:157-183`). Classic Link discovery is LAN
  multicast/broadcast; Link Audio inherits that scope — there is no NAT traversal, no TCP,
  no relay of any kind in the SDK.
- **All Link Audio traffic is unicast UDP** on a dedicated socket per interface:
  `MessengerInterface = UnicastIpInterface<..., kMaxMessageSize>` opened via
  `openUnicastSocket` (`UdpMessenger.hpp:574-583`,
  `discovery/UnicastIpInterface.hpp:32-40`). Messages use an 8-byte protocol header
  `"chnnlsv1"` + 24-byte header; types: `kPeerAnnouncement`, `kChannelByes`, `kPong`,
  `kChannelRequest`, `kStopChannelRequest`, `kAudioBuffer`
  (`link_audio/v1/Messages.hpp:44-53,107-108`).
- **Datagram sizing**: control messages ≤1200 bytes to avoid IP fragmentation
  (`Messages.hpp:33-38`). Audio messages are far smaller: the encoder caps audio payload at
  `576 − 24 − 50 = 502 bytes` per datagram, citing RFC 791's minimum reassembly size, with
  a literal `TODO: Find the best size for audio buffer messages`
  (`link_audio/Encoder.hpp:40-45`). 502 bytes = 251 int16 samples ≈ **5.2 ms of mono
  48 kHz audio per packet** (~191 packets/s, ≈0.9 Mbps per mono channel per receiver,
  derived).
- **Fan-out is per-receiver unicast**: a sink sends one copy of every audio datagram to
  each subscribed receiver (`Receivers.hpp:150-159`).
- **Reliability: none for audio.** `kAudioBuffer` messages are fire-and-forget; send
  errors are swallowed (`SinkProcessor.hpp:69-77` catches and logs at debug;
  `Channels.hpp:58-71`'s `SendHandler` catches and returns 0). There is **no ACK, no
  retransmission, no FEC, and no jitter-buffer in the SDK** — searching the entire
  `link_audio` tree finds no resend path. The only recovery affordance is detection: each
  chunk carries a monotonically increasing `count` (uint64, `Resizer.hpp:130-134`,
  `AudioBuffer.hpp:97`) surfaced as `Info.count` (`LinkAudio.hpp:314`), so a receiver can
  observe gaps. Lost packets are simply missing audio; the reference renderer papers over
  gaps by interpolation/underrun handling (`LinkAudioRenderer.hpp:209-227`).
- **Liveness, not delivery, is maintained by repetition**: peer announcements (with channel
  lists and a piggybacked ping) repeat at `ttl × 1000 / ttlRatio = 250 ms` nominal, rate
  limited to ≥50 ms (`UdpMessenger.hpp:289-321,600-601`); channel requests repeat every 5 s
  (`SourceProcessor.hpp:108-122`); receivers/channels expire on TTL
  (`Receivers.hpp:118-130`, `Channels.hpp:442-483`). Ping/pong RTTs feed a
  quality metric `speed / (1 + jitter)` used only to pick the best gateway path to a peer
  (`link_audio/NetworkMetrics.hpp:13-79`, `Channels.hpp:334-342`).
- **No encryption or authentication** anywhere in the audio path (plain UDP payloads; the
  only filter is dropping own/foreign-group messages, `UdpMessenger.hpp:376`).

## 4. Formats and limits (cited facts)

| Property | Value | Citation |
|---|---|---|
| Sample format | int16 interleaved PCM; codec enum has a single real entry `kPCM_i16 = 1` (wire allows future codecs via a codec byte) | `LinkAudio.hpp:51`, `AudioBuffer.hpp:41-45`, `PCMCodec.hpp:33-69` |
| Compression | none | `PCMCodec.hpp` (raw big-endian int16) |
| Channels per buffer | 1 (mono) or 2 (stereo), enforced at commit | `LinkAudio.ipp:153`, `LinkAudio.hpp:238` |
| Channels per peer | one announced channel per sink; unbounded sink count — announcements split across multiple ≤1176-byte messages | `MainProcessor.hpp:191-200`, `UdpMessenger.hpp:265-287` |
| Sample rate | free-form `uint32_t`, carried per buffer; receivers are expected to resample (reference impl re-pitches via cubic interpolation) | `LinkAudio.hpp:239,313`, `LinkAudioRenderer.hpp:229-232` |
| Identity | channel/peer/session IDs = 8-byte random values, stable for the channel's lifetime; names are mutable display strings (≤256 chars at `Link-4.0`) | `Id.hpp:29`, `NodeId.hpp:34`, `LinkAudio.hpp:97-99`, `Messages.hpp` (4.0) |
| Audio payload per datagram | ≤502 bytes (251 mono / 125 stereo int16 frames), chunked with per-chunk `{count, numFrames, beginBeats, tempo}` | `Encoder.hpp:40-45`, `AudioBuffer.hpp:51-101`, `Resizer.hpp:80-105` |
| Sink queue depth | 128 buffers of `maxNumSamples`; drained every 1 ms | `Sink.hpp:41`, `MainProcessor.hpp:47` |
| Tempo changes mid-stream | supported; the Resizer starts a new chunk when tempo changes discontinuously | `Resizer.hpp:76-79` |

## 5. Beta status, stability, licensing (cited facts)

- The vendored submodule is pinned at tag **`Link-4.0.0b1`** ("Update README.md for
  LinkAudio", commit `082691b`). Upstream has since tagged `Link-4.0.0b2`–`b4` and a final
  **`Link-4.0`** (`git tag` in the submodule clone).
- The **public API is essentially stable** from b1 → 4.0: additions are name truncation at
  256 chars, a `peerName()` getter, and `channels()` re-annotated from realtime-safe *yes*
  to *no* (`git diff Link-4.0.0b1..Link-4.0 -- include/ableton/LinkAudio.*`).
- Correctness fixes landed after b1 that matter to a bridge (all in `Link-4.0`):
  - `2ea60ca` **"Fix receiving without Sink"** — at b1, a peer that announces **no sink
    channels never gets a send handler registered on the publisher**, so a *source-only*
    peer cannot receive audio (fix moves send-handler registration out of the per-channel
    loop; test `PeerSendHandlerForPeerWithNoChannels`).
  - `d8a47ba` "Truncate the peer name to avoid buffer overruns on serialization".
  - `e9a2e41` "Catch message size exceptions in UdpMessenger".
  - `addb7da` "Prune channels from disconnected peers"; `fcaefc6` "Set LinkAudio to
    disabled when Link is disabled"; `1676fb8` "Fix pruneSendHandlers not running for
    source-only peers".
- Signs of in-flux design: the encoder's packet-size `TODO` (`Encoder.hpp:40`), the unused
  `kSourceQueueSize` constant (`Source.hpp:35`), and the reserved `groupId` field ("session
  groups") that must currently be 0 (`Messages.hpp:45,59`, `UdpMessenger.hpp:376`).
- **Licensing**: identical to classic Link — dual-licensed **GPLv2+ or proprietary**
  ("Ableton Link is dual licensed under GPLv2+ and a proprietary license. If you would like
  to incorporate Link into a proprietary software application, please contact
  link-devs@ableton.com", `README.md` §License; same header on every `link_audio` file,
  e.g. `LinkAudio.hpp:5-20`). There is **no Link Audio-specific license text** anywhere in
  the repo — no extra beta terms, no separate SDK agreement. The practical constraint for
  WAIL is the same one already accepted by vendoring Link for sync.
- The in-repo `TEST-PLAN.md` covers sync/audio-alignment behavior but contains no Link
  Audio-specific certification requirements at this tag.

---

## 6. Feasibility: WAIL as a Link Audio WAN bridge (analysis, not cited fact)

Proposed shape: one WAIL instance per LAN acts as a Link Audio peer. On the capture side it
subscribes (`LinkAudioSource`) to selected local channels, assembles their buffers into
NINJAM intervals, Opus-encodes, and ships them over the existing WebSocket relay. On the
playback side it republishes remote peers' intervals (`LinkAudioSink`), timestamped exactly
one interval late — which, because WAIL already aligns interval boundaries across the WAN,
lands on the *current* local interval, exactly like WAIL Recv does today.

### What the API supports (works in favor)

1. **Discovery and identity map cleanly onto WAIL's stream model.** `channels()` +
   `setChannelsChangedCallback` give stable 8-byte channel IDs with human-readable channel
   and peer names — a direct replacement for WAIL's per-stream naming
   (`stream_names.go`), and enough to build a channel-picker UI in wail-app.
2. **Subscribe-to-activate matches the bridge lifecycle.** Sinks transmit only while a
   source subscribes (`Sink.hpp:62`), so WAIL subscribing is what turns on the DAW-side
   stream, and WAIL's republished channels cost zero LAN bandwidth until someone on the
   remote LAN actually listens.
3. **Timestamps are readable and writable in the beat domain.** On capture, every buffer
   arrives with `sessionBeatTime` + `tempo` + `count`; WAIL can bucket frames into interval
   `n = floor(globalBeat / beatsPerInterval)` (matching
   `crates/wail-core/src/interval.rs:3-26`) without touching wall clocks. On republish,
   `commit(sessionState, beatsAtBufferBegin, …)` accepts any beat value, so committing at
   `originalBeat + beatsPerInterval` is mechanically supported. The reference consumer
   demonstrates that playback offset is consumer-chosen (4 beats in LinkAudioHut), so a
   well-behaved consumer will happily render audio whose beat-stamps equal "now".
4. **Interval-sized latency hides the WAN.** Link Audio's own design tolerates seconds of
   receive-side buffering (the example buffers multiple buffers and reports seconds
   buffered); WAIL's one-interval offset is just a larger, beat-aligned version of the same
   pattern.
5. **A C API exists** (`abl_link` at `Link-4.0`), so the bridge can live in wail-app (Go,
   CGo) or a Rust helper without writing a C++ shim from scratch.

### What blocks or degrades it (works against)

1. **Capture reliability — the hard one.** WAIL's stated requirement is *complete* capture
   and delivery; the current Send plugin is lossless by construction because it reads the
   DAW's buffers in-process. Link Audio capture inserts a UDP hop with **no retransmission**
   (§3): any dropped/reordered/late datagram is a hole in the captured interval. `count`
   gaps make loss *detectable* (fill with silence + surface a warning), never *recoverable*.
   On a quiet wired LAN this may be near-zero; on Wi-Fi under load it will not be. Link
   Audio capture therefore cannot carry WAIL's "bulletproof" guarantee — it can only be an
   explicitly best-effort mode.
2. **16-bit PCM quantization.** The wire format is int16 (§4); WAIL's plugin path captures
   f32 and Opus-encodes from full precision. Bridged audio takes an extra int16 quantization
   (and possibly a resample if the DAW's rate differs from WAIL's Opus pipeline rate).
   Acceptable for jam audio; still a fidelity step down from the plugin path.
3. **Republish must be paced, not burst.** WAIL holds a complete interval before relaying,
   but it cannot dump 8 s of audio into a sink at once: the sink queue is 128 slots drained
   at 1 ms ticks straight onto UDP (§3/§4), and receivers like the reference renderer drop
   buffers older than their latency window and stall on far-future ones
   (`LinkAudioRenderer.hpp:133-164`). The bridge must play the interval out in
   real time — retain/commit a few ms of audio per tick with monotonically advancing beat
   stamps — i.e., impersonate a live performer one interval behind. That's straightforward
   (it mirrors what the Recv plugin does) but it is a realtime loop WAIL must run per
   republished channel.
4. **Consumer playback policy is app-defined.** Nothing in the protocol forces a consumer
   to honor arbitrary beat offsets; a consumer could clamp its latency window (the reference
   uses a fixed 4 beats) or validate timestamps against "now". Because WAIL stamps
   republished audio at ≈ local now, this mostly doesn't bite — but consumers with very
   small fixed windows will add their own few-beats latency on top, and the commit-doc
   contract ("must be same as used for rendering locally", `LinkAudio.hpp:245-247`) means
   Ableton could tighten validation in a future version. Low risk today, worth tracking.
5. **Session/timeline coupling.** Beat mapping only works within one Link session
   (`beginBeats` → `nullopt` across sessions). The bridge naturally re-timestamps into the
   destination LAN's session at commit time, so this is fine — but it means WAIL's existing
   tempo/interval sync must keep both LANs' tempos and interval indices aligned (it already
   does; that's WAIL's core job). Mid-interval tempo changes arrive per-chunk and would need
   the same re-pitching treatment the example implements, or WAIL's existing policy of
   quantizing tempo changes to interval boundaries.
6. **Version pin.** The vendored `4.0.0b1` has the source-only-receiver bug (`2ea60ca`,
   §5): a WAIL bridge that only *listens* on a LAN (no sink yet) would never receive audio
   at b1. Workaround: always create the republish sink up front. Correct fix: **bump the
   submodule to `Link-4.0`** before any Link Audio work.
7. **Stereo max / channel granularity.** Each Link Audio channel is mono or stereo. WAIL's
   16-slot mixer model maps fine (one source per slot), but >2-channel stems need multiple
   channels, and per-datagram overhead (~15%) plus per-receiver unicast fan-out makes many
   channels × many listeners the LAN bandwidth worst case (~0.9 Mbps per mono channel per
   listener) — fine for a handful, worth surfacing in UI for big rooms.

### Capture-side sketch and the loss question, concretely

WAIL-as-capturer on LAN A: enable Link Audio, enumerate channels, create a
`LinkAudioSource` per user-selected channel. The callback (Link thread) copies
`{samples, count, sessionBeatTime, tempo, numFrames, sampleRate}` into a lock-free queue
(exactly the LinkAudioHut pattern); a worker maps `sessionBeatTime` → interval index,
writes frames at the correct offset in the interval buffer (frame offset =
`(bufferBeginBeat − intervalBeginBeat) / tempo × sampleRate`), tracks `count` continuity,
and at the boundary hands the interval to the existing Opus/`AudioBridge` pipeline. Versus
the in-DAW plugin this is: same interval math, plus resampling to the pipeline rate, plus
an unavoidable *loss window* — the plugin's capture is loss-free by construction, Link
Audio capture is loss-detectable at best. Recommendation follows directly:

### Recommendation

Treat Link Audio as a **zero-install on-ramp, not a transport upgrade**:

- **Do**: prototype a capture/republish bridge in wail-app behind a flag — it removes the
  plugin-install requirement for casual participants and makes any Link Audio-capable app
  (e.g. Live 12.3+) a WAIL audio source/sink with no DAW plugin. Bump `vendor/link` to
  `Link-4.0` first. Fill detected gaps (`count` discontinuities) with silence and surface a
  per-stream "lossy capture" indicator in the UI.
- **Don't**: position it as a replacement for WAIL Send/Recv. The plugins remain the only
  path that satisfies WAIL's complete-capture requirement (in-process f32 capture, TCP IPC,
  WebSocket delivery), and they should stay the default for performers whose audio matters.
- **Licensing**: no new obligations beyond the existing Link vendoring — GPLv2+ compliance
  or an Ableton proprietary license covers Link Audio identically.
