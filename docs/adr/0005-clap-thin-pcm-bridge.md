# First-party CLAP bridge as a thin PCM transport (amends ADR-0001)

Status: **superseded by ADR-0007** (2026-07-28). The plugins, their loopback IPC, and
the app-side IPC stack were removed; the body below is kept verbatim as the record of
why the path existed and what it taught.

ADR-0001 made Link Audio WAIL's only audio interface and retired the CLAP/VST3
plugins, accepting the consequence that "DAWs without Link Audio support have no WAIL
path anymore." That gap is real: outside Ableton Live 12.3+, users must bridge audio
with a third-party plugin (VoidLinkAudio VST), which carries the same install friction
the WAIL plugins had — just outsourced. We reintroduce a **first-party CLAP plugin
path**, but shaped to avoid the maintenance surface that justified ADR-0001.

The original plugins were killed for surface, not quality: two Rust plugin crates, a
`nih_plug` fork for dynamic port names, an **in-plugin Opus codec** (the shared
`wail-audio` crate), installers, and plugin CI. Since then the entire codec/interval/
room-clock/PLC/pacing pipeline moved into the Go app (`wail-app/internal/*`,
`interval_codec.go`, ADR-0002). That relocation is what makes a thin revival possible:
the plugin no longer needs to know anything about audio beyond moving PCM.

## Decision

WAIL ships two **thin** first-party CLAP plugins — WAIL Send and WAIL Recv — that are
**raw-PCM transports only**, talking to the running WAIL app over loopback TCP IPC.
The app owns all Opus/WAIF/interval/relay logic; nothing about WAIL's audio identity
re-enters the plugin. Link Audio remains WAIL's **primary** interface; the CLAP bridge
is **optional and opt-in**, for DAWs without Link Audio.

- **Thin bridge, codec stays in Go.** IPC carries raw PCM + a tiny header
  (`wail-app/ipc.go`): `RawPCM` (plugin→app), `RemotePCM`/`StreamName`/`StreamGone`
  (app→plugin). This is the guardrail against re-growing ADR-0001's surface — there is
  no shared codec crate to maintain, and no path back to a "fat" plugin.
- **Pluggable engine seams, one engine.** The engine's capture source and emit sink are
  interfaces (`captureSource`/`emitSink`); Link Audio and IPC are two implementations of
  each, feeding the same `linkAudioEngine`. A user can run Link Audio and the plugins at
  once.
- **Minimal C, CLAP-only, no fork.** The plugins are plain C against the official CLAP
  headers (`vendor/clap`), reusing the C toolchain the cgo build already needs — no Rust,
  no Cargo, no `nih_plug` fork. The fork existed only for runtime aux-port renaming, which
  CLAP does natively (`CLAP_AUDIO_PORTS_RESCAN_NAMES` is usable while active). VST3/AU
  remain a future `clap-wrapper` target from the same source, not a second codebase.
- **Realtime discipline (ADR-0002).** The plugin's `process()` only touches lock-free
  SPSC rings + memcpy; all socket I/O is on a dedicated IPC thread — the same "never a Go
  (or blocking) call on the audio thread" invariant as the capture ring.

## Considered options

- **Third-party bridge only (status quo)** — zero WAIL code, but pushes users to a paid
  external plugin and leaves the gap unowned. Rejected: it's the same friction, just not
  ours to fix.
- **Revive the Rust/nih_plug plugins** — fastest to first light, but re-imports every
  liability ADR-0001 named (Rust toolchain, the fork, plugin CI) for a plugin whose body
  is now "copy a buffer to a socket." Rejected.
- **Thin C bridge over IPC (chosen)** — reintroduces one small, opt-in surface (two ~C
  files + a loopback protocol the app already speaks internally) and no codec duplication.

## Consequences

- **Two capture/emit transports exist again**, but behind one set of engine seams and one
  codec — not the "two integration surfaces to maintain forever" ADR-0001 rejected, since
  everything downstream of the seam is shared.
- **Precondition: the DAW is a Link *sync* peer.** The send path maps the plugin's PCM
  onto WAIL's local Link beat by anchoring the plugin's frame counter to the app's Link
  clock. This relies on the DAW being tempo/phase-locked via Link sync — which it already
  is for all WAIL use, and which is far more widely supported than Link Audio. It does not
  require Link *Audio*.
- **Same-machine is lossless in practice.** Plugin↔app IPC is loopback TCP (reliable,
  ordered); the lossy hop remains the WAN relay, as with Link Audio.
- **Bounded fan-out.** The recv plugin exposes a fixed pool of 16 stereo output ports
  (CLAP fixes port *count* while active; only *names* update live). Plugin send streams
  use WAIF stream ids offset to `0x8000+` so they never collide with Link Audio channels.
- **ADR-0002 and ADR-0003 stand unchanged.** No slot table returns to the engine; the
  interval/room-clock model is untouched.
- **Docs/pillars updated.** `CONTEXT.md` pillars 1 and 6 are amended from "nothing else /
  no plugins" to "primarily a Link peer, with an optional thin bridge."

## Grounding

- Engine seams: `wail-app/audio_engine_ports.go`, `audio_engine_real.go`.
- IPC protocol + adapters: `wail-app/ipc.go`, `ipc_source.go`, `ipc_sink.go`, `ipc_server.go`.
- Plugins: `plugins/wail_send.c`, `plugins/wail_recv.c`, `plugins/wail_ipc.h`.
- CLAP naming semantics: `vendor/clap/include/clap/ext/audio-ports.h` (`RESCAN_NAMES` has
  no `[!active]` restriction; `RESCAN_LIST` does).
