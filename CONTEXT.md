# WAIL

WAIL makes remote musicians feel like one Ableton Link session: on each LAN it is a native Link peer — time and audio — and it extends the session across the internet, intervalic, compressed, and loss-free.

## Pillars

1. **WAIL is primarily a Link peer.** Audio enters WAIL as local Link Audio channels and leaves it as published Link Audio channels; time enters and leaves as Link sync. If an app speaks Link + Link Audio, it works with WAIL with nothing to install. For DAWs without Link Audio there is one **optional, opt-in** bridge: WAIL Send / WAIL Receive (ADR-0007) — CLAP plugins that are themselves Link peers, making the DAW a Link Audio citizen outright. It stays optional so "nothing to install" stays the norm.
2. **Intervalic, not real-time.** The NINJAM model: everyone hears everyone else's recent interval, perfectly beat-aligned to their own grid. The delay is bounded by the interval and adaptive — each round lands on the listener's next boundary once delivered (ADR-0008, NINJAM's actual behavior). WAIL never chases sub-20ms streaming over the WAN.
3. **The WAN leg is loss-free; the speakers prefer freshness.** Everything WAIL captures is completely delivered to every peer and lands in the archive — dropping samples in transit to save milliseconds is never the right trade. Playback is NINJAM-style (ADR-0008): when rounds back up, the freshest plays and the superseded round goes to the recorder, never silently lost. The LAN hop is Link Audio's domain: best-effort by design; WAIL detects loss there, conceals what it can, and surfaces the rest as metrics rather than hiding it.
4. **Ableton Link owns time.** WAIL never invents a musical clock — it participates as a Link peer, and the local Link session is authoritative for tempo, beat, and within-bar phase. Across the WAN only the tempo *number* and the interval length are agreed (ADR-0008); grids are never physically aligned across LANs — capture and playback re-quantize onto each listener's own grid, so cross-LAN phase never reaches the ear. (ADR-0006's snap-and-slew alignment is superseded and remains in the tree only until its retirement lands.) Peer RTT measurement remains for diagnostics.
5. **Boring transport.** One relay server carries everything across the WAN — JSON sync and binary audio — and owns the room interval clock (NINJAM-style); otherwise it is a dumb broadcast relay. No P2P, no ICE/STUN/TURN, no per-peer connection state. We pay an extra hop for a system one person can hold in their head.
6. **One app is the whole system.** Session orchestration, Link bridging, codec, and networking live in a single app per musician (GUI or headless). The optional WAIL Send / WAIL Receive plugins (ADR-0007) are a narrow exception: they are plugin-resident only for Link Audio rendering (each is a Link peer — sink/source and timeline mapping). Room intelligence — codec, intervals, relay, room clock — never enters any plugin.
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
A sender's own interval counter, stamped on its WAIF frames (ADR-0008). Sender-relative: there is no room-wide round, and receivers never need to agree on it.
_Avoid_: room index (the retired relay-owned counter, ADR-0003)

**Interval boundary**:
Where one interval ends and the next begins. Each WAIL places boundaries on its own local Link phase; completed audio ships and pending remote audio is released at a boundary.

**BPI (beats per interval)**:
The room interval's length in beats — bars × beats per bar (default 16). How interval length is displayed and chosen in the UI; the internal model and wire format remain bars × quantum.

**Interval offset**:
Retiring (ADR-0008): the fixed per-round delay D. Playback is now adaptive — each round lands at the listener's next boundary once delivered — so the heard delay is bounded by the interval rather than pinned to a constant.
_Avoid_: latency (reserved for network timing)

**Freshest-wins**:
The playback rule (ADR-0008, from NINJAM): when a sender's rounds back up in the queue, the newest complete one plays and the stale one is skipped at the speakers — but still recorded. Rounds advance per sender, never per channel, so one musician's streams cannot split across rounds.

**Tempo declaration**:
A tempo change broadcast with a monotonic priority stamp; peers adopt by Link's strictly-greater rule, ties broken by owner id (ADR-0008). The only way tempo crosses the WAN — observed DAW changes are declared on the musician's behalf after de-noising; WAIL's own tempo control declares directly.

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

**Beats per bar**:
WAIL's quantum — the local phase lens passed to Link's beat/phase math. Per-peer and never shared by Link (Live ties its own to Global Quantization). Default 4.
_Avoid_: meter, time signature (neither travels over Link; beats per bar is the number Link uses)

**Launch quantization**:
A DAW-local setting (e.g. Live's Global Quantization) that aligns clip launches to a bar multiple. Invisible to Link and WAIL — it can be communicated (prompts, display) but never read or enforced.

**Channel affinity**:
Republishing a reconnecting identity's streams under the same channel identities, so LAN apps' routing survives the blip.

**WAIL Send / WAIL Receive**:
The Link-Audio-native CLAP plugin pair (ADR-0007), and the only bridge WAIL ships: each instance is a Link peer, making a Link-but-not-Link-Audio DAW a Link Audio citizen. WAIL Receive is a 16-port device auto-populated from room-published channels; WAIL Send publishes one channel per track instance, named from the track.

**Room-published channel**:
A Link Audio channel a WAIL app publishes from room content — one remote stream one interval late, or the room metronome. Named `WAIL · {peer} · {stream}`.

**WAIL · prefix**:
The channel-name marker meaning room-published. WAIL Receive subscribes only to prefixed channels (first-wins dedupe on the stream name when two local WAILs publish the same room); WAIL Send channels never carry it — the prefix marks room content, not WAIL adjacency.

**Bridge port**:
One of WAIL Receive's 16 stereo output ports, auto-assigned per room-published channel and live-renamed via CLAP `RESCAN_NAMES`, so a peer joining the jam appears as a named sub-chain with no user action.

**LAN loss**:
Samples lost on the Link Audio hop between a LAN app and WAIL. Detectable via sequence counters, never recoverable; concealed where possible and always surfaced in metrics.

### Grid alignment

_This section describes ADR-0006 machinery superseded by ADR-0008 (alignment is not a musical requirement — re-quantization means cross-LAN phase never reaches the ear). The terms remain while the mechanism is still in the tree._

**Room grid**:
The room's shared interval timeline, owned by the relay's room clock. The single fixed reference every WAIL aligns its local grid to; peers never align to each other directly.

**Local grid**:
A peer's interval boundaries derived from its own Link session timeline at the BPI phase lens. Capture, playout, and the metronome all live on this grid.

**Alignment error (δ)**:
The phase distance between a peer's local grid and the room grid at a moment in time. The quantity entry conformance and the grid slew measure and act on.

**Entry conformance**:
The rule every peer runs on join or rejoin: adopt the room tempo, measure δ, and snap only when |δ| exceeds the perceptual threshold (~25 ms). Mid-blip reconnects find δ ≈ 0 and no-op; first joins and app restarts snap.

**Join-time snap**:
The one-time re-mapping of the local Link session onto the room grid during entry conformance. Confined to transition moments: never fires mid-session, never when already aligned.

**Grid slew**:
A small bounded tempo nudge (≤0.05%, below the pitch JND) that closes steady-state alignment error; gated so it never acts near user tempo changes or just after entry, and cancelled by any mid-slew tempo commit or rejoin. The only steady-state steering WAIL ever applies.
_Avoid_: phase lock (WAIL deliberately does not phase-lock across the WAN), micro-slew (the capture path's per-buffer sample-domain drift correction — a different mechanism)

**Grid steer**:
The module that owns the ADR-0006 surface end to end: entry conformance, the gated grid slew, snapshot-tempo arbitration, and the committed-tempo record (what tempo the session last committed to, and when — the single home of the slew's tempo gate). Implemented as `internal/align`'s `Steerer`; it drives the `GridAligner` math and the Link bridge, and the session loop only forwards events to it.
