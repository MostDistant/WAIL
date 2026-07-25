# Link Bridge: Link-Audio-native CLAP plugins (amends ADR-0005)

Status: accepted (pre-implementation; sequencing and verification gates below)

ADR-0005's thin PCM bridge solved the install-surface problem but left two things on
the table: timing precision is structurally bounded on its FIFO path (delivery time IS
play time, so playback offset ≥ the DAW's block pull — field-measured as a ~88 ms
cushion leak in #438, then a ~20 ms lead floor), and it duplicates Link Audio's
receiver contract over a private IPC instead of speaking it. Meanwhile the plugins'
actual target DAWs turned out all to be native Link *sync* citizens (Live pre-12.3,
Bitwig-class hosts) that lack only **Link Audio**. The direct consequence: the plugins
can become Link Audio peers themselves, and every WAIL-room feature flows through the
app's existing capture/emit with zero app changes.

## Decision

Build a second CLAP plugin pair — the **Link Bridge** (Send/Recv) — that speaks Link
Audio directly. Each plugin instance joins the LAN Link session as its own peer; the
WAIL app stays exactly as it is (rooms, relay, codec, intervals). The ADR-0005 PCM
bridge ships in parallel (see Consequences).

- **Same repo, bundled installer.** One knope version, one `vendor/link` pin, one
  clap-trap harness; separate bundle IDs/vendor space (a standalone product surface),
  but distributed inside the WAIL installer — Recv is useless without a running WAIL
  app, so a separate download is a guaranteed support case.
- **Recv: one device, 16 named ports, zero config.** Subscribes only to
  **room-published channels** (named `WAIL · {peer} · {stream}` — the app adopts this
  prefix at publish time; the room metronome passes naturally). Ports auto-assign per
  channel and live-rename via `RESCAN_NAMES`; a peer joining the jam appears as a named
  sub-chain with no user action. First-wins dedupe on stream name keeps a multi-WAIL
  LAN benign.
- **Send: one channel per track instance**, named from the DAW track name, **no
  prefix** — the prefix marks room content, not WAIL adjacency (a prefixed Send channel
  would be heard raw *and* one-interval-late via the room, doubled, on every Recv in
  the LAN). Send is inherently generic: any Link Audio app on the LAN can hear it.
- **Audio-only peer.** The target DAWs already own tempo/phase/transport via native
  Link; the plugins never call a tempo-mutating Link API. Truly Link-less DAWs are out
  of scope.
- **48 kHz only, fail loud.** Link Audio is 48k int16; other host rates render silence
  with a loud indicator. Resampling is a fast-follow if users ask (its latency is a
  known constant the stamp math can subtract).
- **Stamp-aligned rendering, ported from the IPC path.** Recv renders each buffer at
  its stamped beat: host-transport phase alignment when rolling (sample-accurate — the
  host's transport→sample mapping absorbs output latency), Link-clock alignment when
  stopped (residual output-path constant `L`, documented and calibratable), 32-frame
  deadband against cadence jitter. Lessons carried from the IPC timing work: delivery
  lead is decoupled from playback offset, late audio is skipped never played late, and
  no two clocks are assumed to be the same oscillator without measurement.

## Considered options

- **One channel per Recv instance (DAW-idiomatic)** — rejected: per-participant
  configuration burden; "a peer joins and their named port appears" is the feature.
- **Subscribe to all LAN channels (fully generic)** — rejected: port slots allocated by
  accident of timing, and WAIL's affinity layer already gives room streams rock-solid
  identity; the WAIL filter is what makes stable ports easy. Generic listening remains
  possible via the Send side and native Link Audio apps.
- **Tempo/transport injection into the session** — moot: the DAWs are already Link
  citizens; nothing to inject.
- **Separate repo / installer** — rejected: duplicates the vendored SDK, harness, and
  knope plumbing; splitting later is cheap if the seam proves itself.
- **Keep IPC-only** — rejected: the block-pull floor is structural; no knob fixes it.

## Consequences

- **ADR-0005 is amended, not replaced.** Both bridges ship; the PCM bridge is marked
  legacy once the Link Bridge proves out in real sessions; deletion is a separate
  decision one release cycle later, on evidence (e.g. hosts that sandbox plugin
  networking may need the IPC path — Bitwig's per-process hosting is a verification
  item).
- **Pillar 6 narrows** (CONTEXT.md): room intelligence (codec, intervals, relay, room
  clock) never enters any plugin; Link Audio rendering is legitimately plugin-resident.
- **A channel-name semantic enters the protocol surface**: `WAIL · ` means
  room-published. It is cosmetic for native Link Audio subscribers (IDs unchanged) and
  load-bearing for bridge Recv filtering.
- **Verification gates before feature work**: (1) Link SDK links into a `.clap` bundle
  on macOS + Windows/Linux CI; (2) Live and Bitwig CLAP hosting populates
  `song_pos_beats` (the transport-phase path depends on it; also tracked for the IPC
  retrofit as #444); (3) Bitwig's per-process plugin hosting allows Link's networking.
- **Sequencing**: build spike (gate 1) → Send (simpler half) → Recv renderer.
