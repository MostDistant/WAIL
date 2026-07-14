# Ableton Link 4.0 Research

Research date: 2026-07-14. All claims cite primary sources: the Ableton/link GitHub
repository at the `Link-4.0` tag, the GitHub releases API, and the official docs at
ableton.github.io/link. Analysis and WAIL implications are clearly marked as such.

> Note on the vendored SDK: `vendor/link` in this repo is an **uninitialized (empty)
> submodule** pinned to commit `082691b46ef1fc40155cab68384fd0a2ce3e5c40`
> (`.gitmodules`, `git submodule status`). That commit is exactly the `Link-4.0.0b1`
> tag (GitHub tags API). All file citations below are therefore to GitHub at the
> `Link-4.0` tag (`e9a2e41`), not to local files.

## TL;DR

Link 4.0 (final, released 2026-05-04) adds exactly one headline feature over Link 3.x:
**Link Audio** — real-time, uncompressed 16-bit PCM audio streaming between Link peers,
sent as unicast UDP datagrams (≤1200 bytes each) between peers discovered via Link's
existing LAN multicast discovery. It is a low-latency continuous stream with beat-time
metadata per buffer, not an intervalic/buffered model. Channel discovery is via
`channels()` / `setChannelsChangedCallback()`; publishing is `LinkAudioSink`, receiving is
`LinkAudioSource`. Licensing is unchanged: GPLv2+ dual-licensed with a proprietary option
via link-devs@ableton.com — Link Audio carries the identical license header. There is no
WAN transport, no NAT traversal, no compression, and no encryption. Link 4.0 also bumps
the language requirement to C++17 and raises minimum compiler versions. WAIL currently
pins **beta 1** (both `vendor/link` and the `abletonlink-go` dependency), which is missing
fixes shipped in betas 2–4 and the final release.

## Sources

- Release notes: https://github.com/Ableton/link/releases/tag/Link-4.0 (and tags
  `Link-4.0.0b1` … `Link-4.0.0b4`), fetched via the GitHub releases API.
- README at Link-4.0: https://github.com/Ableton/link/blob/Link-4.0/README.md
- LICENSE at Link-4.0: https://github.com/Ableton/link/blob/Link-4.0/LICENSE.md
- Public API header: https://github.com/Ableton/link/blob/Link-4.0/include/ableton/LinkAudio.hpp
- Wire/transport internals: `include/ableton/link_audio/{v1/Messages.hpp, AudioBuffer.hpp,
  PCMCodec.hpp, UdpMessenger.hpp, Channels.hpp, Resizer.hpp}` and
  `include/ableton/link/PeerState.hpp`, `include/ableton/discovery/IpInterface.hpp` at `Link-4.0`.
- C API: https://github.com/Ableton/link/blob/Link-4.0/extensions/abl_link/include/abl_link.h
- Concepts docs: https://ableton.github.io/link/ ("Link Audio API" section).
- WAIL repo files: `.gitmodules`, `.github/workflows/release.yml`, `wail-app/go.mod`,
  `crates/wail-core/Cargo.toml`.

## 1. Release timeline and what's new vs Link 3.x

Facts (GitHub releases API, `repos/Ableton/link/releases`):

| Tag | Date | Notes (release body) |
|---|---|---|
| `Link-3.1.5` | 2025-12-03 | last 3.x release |
| `Link-4.0.0b1` | 2026-02-10 | "**PRE-RELEASE** — Require C++ 17; Add Link Audio (docs in README, LinkAudio.hpp, and https://ableton.github.io/link/#link-audio); Add LinkAudioHut example app" |
| `Link-4.0.0b2` | 2026-03-06 | "**PRE-RELEASE** — Make discovery work across subnets; Make ESP example compatible with ESP-IDF 6.0" |
| `Link-4.0.0b3` | 2026-04-01 | "**PRE-RELEASE** — Fix an issue where peers that only have sources would never receive audio buffers; Add support for Link Audio to the abl_link C API" |
| `Link-4.0.0b4` | 2026-04-14 | "**PRE-RELEASE** — Fix an issue where long peer names would lead to an uncaught exception" |
| `Link-4.0` | 2026-05-04 | "Add support for Link Audio" — final; tag commit `e9a2e41`, **identical to `Link-4.0.0b4`** (tags API) |

- The entire release body of `Link-4.0` is the single line "Add support for Link Audio".
  Link Audio *is* the 4.0 release.
- Core `Link.hpp` API changes vs 3.1.5 are minimal (diff of `Link.hpp` between tags):
  `isEnabled()`/`numPeers()` become realtime-safe (were "Realtime-safe: no"),
  `SessionState` gains `operator==`/`operator!=`, and the controller typedef moves behind
  `link/ApiConfig.hpp` with `protected` access so `BasicLinkAudio` can subclass
  `BasicLink` (LinkAudio.hpp line 55). No sync-protocol behavior change is claimed in the
  release notes besides discovery (next point).
- "Make discovery work across subnets" (b2) is implemented by **removing the subnet
  filter** from the discovery `UdpMessenger` and setting `IP_MULTICAST_ALL=0` on Linux
  sockets (compare `Link-4.0.0b1...Link-4.0.0b2`: commits "Remove the subnet filter from
  UdpMessenger", "Set the IP_MULTICAST_ALL flag to 0 on Linux sockets"). Discovery is
  still multicast-based (see §3) — this only stops Link from *ignoring* peers whose
  multicast announcements arrive from another subnet; it does not add any internet/WAN
  rendezvous.
- Toolchain: C++17 required; minimum compilers are MSVC 17 2022, Xcode 16.2.0, Clang 13 /
  GCC 10 (README.md build-requirements table, lines 84–91). Link 3.1.5 required only
  C++11 (MSVC 2015, Xcode 9.4.1, Clang 3.6/GCC 5.2 — README at `Link-3.1.5`). Still
  header-only (README line 49). iOS remains excluded ("iOS developers should not use this
  repo", README line 93 — LinkKit is separate).

## 2. Link Audio API surface

All from `include/ableton/LinkAudio.hpp` at `Link-4.0` (line numbers in that file) and
the "Link Audio API" section of https://ableton.github.io/link/.

- **`LinkAudio` / `BasicLinkAudio<Clock>`** subclasses `BasicLink` — "LinkAudio provides
  the Link functionality plus audio sharing … LinkAudio and Link should not be used
  simultaneously" (lines 37–46). Constructed with `(double bpm, std::string name)`; the
  peer name identifies the peer in the session, truncated at 256 chars (lines 58–61).
- **Enable/disable at runtime**: `enableLinkAudio(bool)` / `isLinkAudioEnabled()` (lines
  65–75). Docs: "Link Audio can be enabled and disabled at runtime without affecting
  Link's tempo and beat synchronization."
- **Channel discovery**: `std::vector<Channel> channels() const` (line 120) where
  `Channel = {ChannelId id, std::string name, PeerId peerId, std::string peerName}`
  (lines 108–114); IDs are persistent for the channel's lifetime, names are display-only
  and mutable (lines 102–107). `setChannelsChangedCallback(callback)` fires "when
  channels are discovered or disappeared and when names change", on a Link-managed
  thread (lines 90–100).
- **`LinkAudioSink`** (publish): constructed with `(LinkAudio&, std::string name, size_t
  maxNumSamples)`; creating it announces the channel to the session (lines 154–171).
  "Sinks only send audio if at least one corresponding source is present in the session"
  (line 50) — bandwidth is only consumed on demand. Writing is via a scoped
  `BufferHandle` (realtime-safe to retain and commit): write `int16_t* samples`, then
  `commit(sessionState, beatsAtBufferBegin, quantum, numFrames, numChannels, sampleRate)`
  — `numChannels` "Can be 1 for mono or 2 for stereo" (lines 207–263). The session
  state/quantum/beats passed to `commit` must be the same values used to render the audio
  locally (lines 253–255).
- **`LinkAudioSource`** (receive): constructed with `(LinkAudio&, ChannelId id, callback)`;
  the callback is "invoked on a Link-managed thread when a buffer is received" (lines
  282–294). Received `BufferHandle` exposes `int16_t* samples` plus `Info {numChannels,
  numFrames, sampleRate, count (sequence number), sessionBeatTime, tempo, sessionId}`
  and `beginBeats(sessionState, quantum)` / `endBeats(...)` to map the remote beat time
  onto the local timeline — returning `std::nullopt` if the buffer came from a different
  Link session (lines 309–350).
- **Sample format**: "Audio buffers are interleaved and samples are represented as 16-bit
  signed integers" (LinkAudio.hpp line 51; same statement on the docs page).
- **Block-size/sample-rate mismatch is the receiver's problem**: "incoming buffers don't
  match the receivers audio engine block size and sample rate as those properties are
  dependant on the senders properties" (docs page). No resampling or jitter buffering is
  provided by the public API — buffers arrive asynchronously as they come.
- **C API**: as of b3, `extensions/abl_link/include/abl_link.h` exposes the whole surface
  (`abl_link_audio_enable_link_audio`, `abl_link_audio_get_channels`,
  `abl_link_audio_set_channels_changed_callback`, `abl_link_audio_sink`/`source` types,
  etc.).
- **Example**: `examples/linkaudiohut/main.cpp` (LinkAudioHut) demonstrates usage;
  referenced from README lines 102–103.

## 3. Transport, network scope, and wire format

From `include/ableton/link_audio/` internals at `Link-4.0`:

- **Audio and channel signaling travel over unicast UDP**, not multicast. The Link Audio
  messenger is built on `UnicastIpInterface` (`link_audio/UdpMessenger.hpp` lines
  574–586), and audio buffers are sent per-receiver (`Channels.hpp` `SendHandler`).
- **Peers find each other's Link Audio endpoints through Link's existing discovery**:
  `link/PeerState.hpp` adds `AudioEndpointV4` (`'aep4'`) / `AudioEndpointV6` (`'aep6'`)
  payload entries alongside the existing measurement endpoints (lines 49–54, 75–84).
  Link discovery itself is still UDP multicast on `224.76.78.75:20808` (IPv6
  `ff12::8080`, a link-local multicast address) — `discovery/IpInterface.hpp` lines
  30–38. So the reachability model is: **discoverable wherever Link multicast reaches
  (LAN, or multicast-routed subnets as of b2); audio then flows peer-to-peer unicast
  UDP.** There is no relay, no NAT traversal, and no internet rendezvous anywhere in the
  codebase.
- **Protocol**: versioned "chnnlsv1" message format (`link_audio/v1/Messages.hpp` line
  109). Message types: `kPeerAnnouncement`, `kChannelByes`, `kPong`, `kChannelRequest`,
  `kStopChannelRequest`, `kAudioBuffer` (lines 48–54). Announcements repeat with a 5 s
  TTL at a 250 ms nominal period, rate-limited to 50 ms minimum
  (`link_audio/UdpMessenger.hpp` lines 293–305, 600–601); ping/pong RTT measurements per
  receiver feed a `NetworkMetricsFilter` "network quality" estimate (lines 325–345,
  438–464).
- **Datagram budget**: `kMaxMessageSize = 1200` bytes, chosen to stay under Ethernet/IPv6
  MTU and avoid IP fragmentation; 24-byte message header ⇒ `kMaxPayloadSize = 1176`
  (`v1/Messages.hpp` lines 33–40). Audio buffer metadata reserves 50 bytes ⇒
  `kMaxAudioBytes = 1126` audio bytes per datagram (`AudioBuffer.hpp` lines 51–52).
- **Codec: uncompressed PCM only.** The codec enum has exactly one valid entry,
  `kPCM_i16 = 1` (`AudioBuffer.hpp` lines 41–45); `PCMCodec.hpp` serializes raw int16
  samples in network byte order. No Opus, no compression of any kind.
- **Chunking**: each `AudioBuffer` datagram carries chunks of
  `{count: u64, numFrames: u16, beginBeats, tempo}` plus `codec, sampleRate: u32,
  numChannels: u8, numBytes: u16` (`AudioBuffer.hpp` lines 54–104, 234–241). A sink's
  committed block is split by `Resizer.hpp` into ≤1126-byte datagrams — e.g. a stereo
  int16 stream fits ≤281 frames (~5.9 ms @ 48 kHz) per datagram, so audio is a steady
  stream of small packets (analysis of the constants above).
- **No encryption or authentication**: the v1 message format is a plain header (type,
  ttl, groupId, node id) + payload (`v1/Messages.hpp` lines 56–101); there is no crypto
  anywhere in `link_audio/`.

### Constraints summary (facts)

- Sample format: interleaved int16 PCM, network byte order. Arbitrary `sampleRate`
  (u32); receiver must cope with the sender's rate and block sizes.
- Per-stream channel count: 1 (mono) or 2 (stereo) per `commit()` documentation.
  `numChannels` is a u8 on the wire.
- Number of announced channels per peer: no documented hard limit — channel
  announcements are split across multiple ≤1176-byte datagrams as needed
  (`link_audio/UdpMessenger.hpp` `updateAnnouncement`, lines 265–287).
- Names (peer + channel): truncated at 256 chars (`kMaxNameSize`, `v1/Messages.hpp`
  line 41; LinkAudio.hpp lines 58–59).
- Delivery: UDP, no retransmission logic in the headers read; buffers carry a `count`
  sequence number so receivers can detect loss/reordering (`AudioBuffer.hpp` line 97).
- Platforms: macOS, Windows, Linux with C++17 compilers (README table); `platforms/esp32`
  exists and the b2 notes mention ESP-IDF 6.0 compatibility; iOS via the separate
  LinkKit SDK only.

### Latency model (analysis, grounded in the above)

Link Audio is **real-time continuous streaming**, not intervalic: every audio-callback
block is committed and immediately sent as one-or-more small UDP datagrams; the receiver
callback fires per arriving buffer. End-to-end latency is network transit plus whatever
jitter/alignment buffering the *application* implements using the per-buffer beat-time
metadata (`beginBeats`/`endBeats`). Nothing in the API imposes a bar/interval of delay.
This is the opposite trade-off from WAIL's NINJAM model (fixed 1-interval latency,
loss-tolerant, WAN-friendly).

Bandwidth (analysis): raw PCM at 48 kHz stereo int16 is 1.536 Mbit/s of sample data per
channel-stream, plus ~6–9% wire overhead given ≤1126 audio bytes per 1200-byte datagram
and UDP/IP headers — roughly 1.7 Mbit/s per subscribed stereo stream, per receiver
(sinks unicast separately to each subscriber, `Channels.hpp` send path). Fine for a LAN,
prohibitive for most WAN links compared to WAIL's ~0.1 Mbit/s Opus streams.

## 4. Licensing

- `LICENSE.md` at `Link-4.0` is **unchanged from 3.x**: GPL v2 or later, with "If you
  would like to incorporate Link into a proprietary software application, please contact
  link-devs@ableton.com". README (lines 10–14) states the same dual-license position:
  "Ableton Link is dual licensed under GPLv2+ and a proprietary license."
- **Link Audio has no separate terms**: `LinkAudio.hpp`, and every `link_audio/*.hpp`
  file, carries the identical GPLv2+ header with the same proprietary-license contact
  (e.g. LinkAudio.hpp lines 5–21, copyright 2025 Ableton AG).
- Nothing in the Link-4.0 release notes or README mentions any license change.
- (Analysis) For WAIL this is status quo: vendoring and linking Link 4.0 — including
  Link Audio — keeps the same GPLv2+ obligations (or requires the proprietary license)
  as Link 3.x did. Adopting Link Audio adds no *new* licensing exposure, but also removes
  none.

## 5. Beta status / API stability

- Link 4.0 is **no longer beta**: the final `Link-4.0` release shipped 2026-05-04 and is
  not marked pre-release (releases API, `prerelease: false`; the b1–b4 bodies each said
  "**PRE-RELEASE**", the final does not).
- The docs page presents Link Audio as a standard part of the Link ecosystem, with no
  beta or API-stability disclaimers.
- The wire protocol is explicitly versioned (`chnnlsv1` protocol header, `v1` namespace),
  which is the only forward-compatibility signal in the source.

## 6. Where WAIL's pins actually are (facts about this repo)

- `vendor/link` is pinned to `082691b46` = **`Link-4.0.0b1`** (`.gitmodules` +
  `git submodule status` vs GitHub tags API) and is currently **not initialized** (empty
  directory). The submodule is only consumed for the release source tarball
  (`.github/workflows/release.yml` lines 14, 40) — no local build compiles it directly.
- The Go app's actual Link comes from `abletonlink-go`
  (`wail-app/go.mod` line 8), which vendors **the same `082691b46` = b1 commit**
  (DatanoiseTV/abletonlink-go `vendor/link` submodule pointer, checked 2026-07-14).
- CI patches abletonlink-go's vendored `link_audio/Channels.hpp` for MinGW GCC (the
  `endpoint` parameter shadowing and the Windows `interface` macro collision —
  `.github/workflows/release.yml` lines 152–183). **Both collisions are still present at
  the final `Link-4.0` tag** (`Channels.hpp` lines 52 and 60), so upgrading b1 → 4.0
  final does not eliminate the patch.
- The Rust crates use `rusty_link = "0.4"` (`crates/wail-core/Cargo.toml`), whose
  upstream currently vendors a **Link 3.x-lineage commit** (`0fc58dc`, 2025-12-15, 13
  commits ahead of `Link-3.1.3`) — i.e. rusty_link does not expose Link Audio today,
  even though abl_link (which rusty_link wraps) gained Link Audio bindings in 4.0.0b3.

## 7. Implications for WAIL (analysis — not sourced claims)

1. **Link Audio does not replace WAIL's core value.** It is LAN-scoped (multicast
   discovery + unicast UDP, no NAT traversal, no relay) and uncompressed. WAIL's whole
   point — bridging Link sessions across the internet via a WebSocket relay with
   Opus-compressed intervalic audio — remains outside Link Audio's design envelope.
   CLAUDE.md's framing ("could replace the custom Opus pipeline for LAN scenarios, while
   WAIL continues to bridge audio over the internet") matches what the source supports.
2. **Different latency philosophy.** Link Audio is real-time streaming with app-managed
   jitter handling; WAIL is fixed 1-interval latency. A hybrid (Link Audio on LAN,
   intervalic Opus over WAN) would mean two audibly different latency domains in one jam
   — a product decision, not just a plumbing swap.
3. **A WAIL peer could act as a LAN↔WAN gateway**: subscribe to local Link Audio
   channels (16-bit PCM in), Opus-encode into WAIL intervals for the relay, and
   conversely publish remote WAIL streams as local Link Audio sinks. The int16
   interleaved format and per-buffer beat metadata (`beginBeats`) map cleanly onto
   WAIL's beat-aligned interval recorder.
4. **Upgrade the pins before building on Link Audio.** Beta 1 (what WAIL and
   abletonlink-go vendor) predates: cross-subnet discovery + channel pruning for
   disconnected peers (b2), the "sources-only peers never receive audio" fix and the
   abl_link C audio API (b3), and the long-peer-name exception fix (b4). The final 4.0
   tag is b4's commit. Bumping `vendor/link` to `Link-4.0` is cheap; the Go side depends
   on abletonlink-go moving its own submodule (or WAIL's CI checkout pinning a newer
   Link inside it). CLAUDE.md/architecture docs still say "4.0.0 beta" — now stale.
5. **Rust plugins can't reach Link Audio yet** via rusty_link 0.4 (Link 3.x vendored).
   If the plugins ever need it directly, options are: wait for a rusty_link release that
   vendors ≥4.0, fork it, or keep all Link Audio work in the Go app via abletonlink-go
   (which already targets the 4.0 lineage). Note nih-plug plugins doing LAN UDP from a
   DAW process is also a sandboxing/firewall consideration.
6. **Licensing is unchanged but real**: GPLv2+ (or negotiate proprietary terms with
   Ableton). Using Link Audio adds no new license risk beyond what WAIL already accepted
   by vendoring Link — but any future move to closed-source distribution has the same
   Link licensing question with or without Link Audio.
7. **Windows CI patch stays.** The MinGW `interface`/`endpoint` collisions in
   `Channels.hpp` are unfixed at `Link-4.0`, so the sed patch in `release.yml` (and its
   grep guard) must survive any submodule bump; consider upstreaming a fix to Ableton.
