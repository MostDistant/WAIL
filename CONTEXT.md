# WAIL

WAIL makes remote musicians feel like one Ableton Link session: on each LAN it is a native Link peer — time and audio — and it extends the session across the internet, intervalic, compressed, and loss-free.

## Pillars

1. **WAIL is a Link peer, and nothing else.** Audio enters WAIL as local Link Audio channels and leaves it as published Link Audio channels; time enters and leaves as Link sync. WAIL has no other LAN interface — nothing to install in the DAW, nothing on the LAN needs to know WAIL exists. If an app speaks Link + Link Audio, it works with WAIL.
2. **Intervalic, not real-time.** The NINJAM model: everyone hears everyone else's *previous* interval, perfectly beat-aligned. Latency is exactly one interval, by design. WAIL never chases sub-20ms streaming over the WAN.
3. **The WAN leg is loss-free.** Everything WAIL captures is completely delivered to every peer — the one-interval offset buys the time to do it, and dropping samples to save milliseconds is never the right trade. The LAN hop is Link Audio's domain: best-effort by design; WAIL detects loss there, conceals what it can, and surfaces the rest as metrics rather than hiding it.
4. **Ableton Link owns time.** WAIL never invents a musical clock. The local Link session is authoritative for tempo, beat, and phase; interval boundaries derive from it. Peer RTT measurement is diagnostics only.
5. **Boring transport.** One relay server carries everything across the WAN — JSON sync and binary audio. No P2P, no ICE/STUN/TURN, no per-peer connection state. We pay an extra hop for a system one person can hold in their head.
6. **One app is the whole system.** Session orchestration, Link bridging, codec, and networking live in a single app per musician (GUI or headless). No plugins, no IPC, no per-DAW integration surface.
7. **Real-time callbacks are sacred.** No allocation, locking, encoding, or blocking I/O on Link's audio delivery threads. Heavy work happens on background threads.
8. **Degrade gracefully, stay observable.** Network blips must not ruin a jam: a reconnecting musician's streams reappear as the same published channels, late audio live-appends, joins fade in. Failures never crash and never happen silently.

## Language

### Session

**Room**:
A named space on the relay that peers join; everyone in a room shares one jam session.

**Peer**:
A participant in a room, identified by a session-scoped ID that changes on reconnect.

**Identity**:
A client's persistent UUID, surviving reconnects; the basis for channel affinity.

**Relay**:
The server every peer connects to; it relays sync (JSON) and audio (binary) to all room peers.
_Avoid_: signaling server (WebRTC-era name)

**Sync message**:
A JSON control message relayed to room peers (tempo, clock sync, chat, stream names).

**Echo guard**:
The suppression window that stops a just-applied remote tempo change from being re-broadcast as a local change.

### Intervalic audio

**Interval**:
A fixed span of bars (e.g. 4 bars of 4/4) of beat-aligned audio; the unit of capture, transmission, and playback.

**Interval index**:
The count of intervals since beat zero; each peer computes it independently from its own beat position.

**Interval boundary**:
The beat position where one interval ends and the next begins; completed audio ships at the boundary and pending remote audio starts playing.

**One-interval offset**:
The NINJAM delay: audio recorded during interval N plays on remote peers during their interval N+1. A design constant, not network lag.
_Avoid_: latency (reserved for network timing)

**Stream**:
One audio feed a peer sends across the WAN, fed by one capture channel and identified by a stream index. A peer may send several.

**WAIF frame**:
One 20ms Opus packet wrapped in WAIL's wire header; intervals travel as sequences of WAIF frames, the last one flagged final.

**Live-append**:
Playing remote audio that arrives mid-interval directly into the current playback interval instead of dropping it or waiting a full interval.

### Link

**Link peer**:
A participant in the LAN's Ableton Link session. WAIL itself is one.

**Capture channel**:
A local Link Audio channel WAIL subscribes to; the source of a stream.

**Published channel**:
A Link Audio channel WAIL publishes onto the LAN, carrying one remote stream one interval late.
_Avoid_: slot, aux output (plugin-era names)

**Channel affinity**:
Republishing a reconnecting identity's streams under the same channel identities, so LAN apps' routing survives the blip.

**LAN loss**:
Samples lost on the Link Audio hop between a LAN app and WAIL. Detectable via sequence counters, never recoverable; concealed where possible and always surfaced in metrics.
