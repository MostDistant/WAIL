# Link Audio is WAIL's only audio interface (retire the Send/Recv plugins)

WAIL originally captured and played DAW audio via two in-process CLAP/VST3 plugins (WAIL Send / WAIL Recv) talking to the app over TCP IPC — lossless by construction, but requiring per-DAW installation and carrying a large surface (two plugin crates, a nih_plug fork for dynamic port names, an IPC protocol, installers, plugin CI). With Link 4.0 final (May 2026) shipping Link Audio, we decided WAIL interacts with local audio **exclusively as a Link peer**: capture subscribes to local Link Audio channels (`LinkAudioSource`), playback publishes remote streams as Link Audio channels (`LinkAudioSink`) offset by one interval. The plugins, the IPC protocol, and their supporting crates are retired.

## Considered options

- **Plugins only (status quo)** — lossless capture, but per-DAW install friction and a permanent parallel integration surface.
- **Both, as adapters behind capture/emit seams** — plugins as the "guaranteed path," Link Audio as zero-install convenience. Rejected: two integration surfaces to maintain forever, and the seam exists to hedge a bet we're now willing to make outright.
- **Link Audio only (chosen)** — one integration surface, zero install, WAIL is invisible on the LAN.

## Consequences

- **The lossless-capture guarantee narrows to the WAN leg.** Link Audio's LAN hop is fire-and-forget unicast UDP: loss is detectable (per-stream sequence counters) but never recoverable. WAIL detects, conceals what it can, and surfaces LAN loss in metrics. Same-machine setups (DAW + WAIL on one computer) deliver over loopback and are lossless in practice; multi-machine setups should prefer wired LAN. (See CONTEXT.md pillar 3.)
- **WAIL only works with Link-Audio-capable apps.** Recorded assumption: the apps musicians use with WAIL speak Link Audio (Live 12.3+ era). DAWs without Link Audio support have no WAIL path anymore.
- **Prerequisite:** bump `vendor/link` from 4.0.0b1 to the final `Link-4.0` tag — b1 has a bug where source-only peers (exactly what WAIL's capture side is) never receive audio, fixed in b3. The `abl_link` C audio API added in the final release is the intended FFI layer for bindings.
- Research grounding: `docs/link-4-research.md`, `docs/link-audio-research.md`.
