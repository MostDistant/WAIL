# WAIL

WAIL makes remote musicians feel like one Ableton Link session: on each LAN it is a native Link peer — time and audio — and it extends the session across the internet, intervalic, compressed, and loss-free.

## Pillars

1. **WAIL is primarily a Link peer.** Audio enters WAIL as local Link Audio channels and leaves it as published Link Audio channels; time enters and leaves as Link sync. If an app speaks Link + Link Audio, it works with WAIL with nothing to install. For DAWs without Link Audio there is one **optional, opt-in** exception: a thin first-party CLAP bridge (WAIL Send/Recv) that moves raw PCM to the app over loopback IPC (ADR-0005) — deliberately minimal so "nothing to install" stays the norm.
2. **Intervalic, not real-time.** The NINJAM model: everyone hears everyone else's *previous* interval, perfectly beat-aligned. The delay is a fixed, configured number of intervals (default one), by design. WAIL never chases sub-20ms streaming over the WAN.
3. **The WAN leg is loss-free.** Everything WAIL captures is completely delivered to every peer — the interval offset buys the time to do it, and dropping samples to save milliseconds is never the right trade. The LAN hop is Link Audio's domain: best-effort by design; WAIL detects loss there, conceals what it can, and surfaces the rest as metrics rather than hiding it.
4. **Ableton Link owns time.** WAIL never invents a musical clock — it participates as a Link peer, and the local Link session is authoritative for tempo, beat, and within-bar phase. Two deliberate, narrow exceptions keep each local interval grid physically aligned to the room grid (ADR-0006): a consented **entry conformance** snap at join/rejoin — transition moments only, and only when the measured alignment error exceeds the perceptual threshold — and a gated **grid slew** in steady state that closes drift with bounded tempo nudges, suppressed for a few seconds after any tempo change and never a `ForceBeat` outside entry. Peer RTT measurement feeds this alignment as well as diagnostics.
5. **Boring transport.** One relay server carries everything across the WAN — JSON sync and binary audio — and owns the room interval clock (NINJAM-style); otherwise it is a dumb broadcast relay. No P2P, no ICE/STUN/TURN, no per-peer connection state. We pay an extra hop for a system one person can hold in their head.
6. **One app is the whole system.** Session orchestration, Link bridging, codec, and networking live in a single app per musician (GUI or headless). The optional CLAP bridge (ADR-0005) is a *thin* exception: it moves raw PCM over loopback IPC and holds no codec, interval, or per-DAW logic — all intelligence stays in the app.
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
The room-wide interval counter, owned by the relay (NINJAM-style) so every WAIL in a room agrees on it. WAIF frames are tagged with it.

**Interval boundary**:
Where one interval ends and the next begins. Each WAIL places boundaries on its own local Link phase and labels them with the shared interval index; completed audio ships and pending remote audio is released at the boundary.

**BPI (beats per interval)**:
The room interval's length in beats — bars × beats per bar (default 16). How interval length is displayed and chosen in the UI; the internal model and wire format remain bars × quantum.

**Interval offset**:
The NINJAM delay: audio captured during interval N is played by remote peers during their interval N+D. D is configurable (default one). A design constant, not network lag.
_Avoid_: latency (reserved for network timing), one-interval offset (D is not fixed at one)

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

**LAN loss**:
Samples lost on the Link Audio hop between a LAN app and WAIL. Detectable via sequence counters, never recoverable; concealed where possible and always surfaced in metrics.

### Grid alignment

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
A small bounded tempo nudge that closes steady-state alignment error; gated so it never acts near user tempo changes or just after entry. The only steady-state steering WAIL ever applies.
_Avoid_: phase lock (WAIL deliberately does not phase-lock across the WAN), micro-slew (the capture path's per-buffer sample-domain drift correction — a different mechanism)
