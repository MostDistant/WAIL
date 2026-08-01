# Room-authoritative tempo: enforcement, intent arbitration, and declared changes

Under ADR-0001 pillar 4, the local Link session is authoritative for tempo: WAIL observes it and reports changes to the room. That works while every peer's session tempo *is* the tempo its musician chose. It breaks when a session tempo moves for reasons nobody chose — Link convergence on a merge, a peer joining at its own tempo, a clock that wanders around one intended value — because WAIL cannot tell an intended change from a disturbance, and reports both.

Field evidence, 2026-07-31: a peer whose clock wandered 119.9↔120 had every excursion broadcast as a tempo change. That dragged the whole room's tempo, and because each broadcast arms the slew's 3 s tempo gate, it also left every peer's drift correction suppressed. This is the cost case `tradeoffs.md` recorded when it closed *steady-state room-tempo enforcement* on 2026-07-26 ("a member's LAN dragging the room… which no user has hit. **Reopen only on an actual yank report**"). This is that report.

**Decision: the room owns tempo. The local Link session is the interface, not the authority.**

1. **The detector arbitrates intent.** An observed session tempo is a *deliberate change* only when it is both large enough and steady enough to be one; anything else is a disturbance. What the room is told is intent, never a raw reading.

2. **Divergence that is not intent is corrected, not adopted.** Small enough for the grid slew to hold → nudged away inaudibly, as now. Beyond that and still not intent → the peer is written back to the room tempo. WAIL never fights a change it has already accepted as intent, because such a change becomes the room tempo and there is nothing left to diverge from.

3. **Declared changes bypass inference entirely.** A tempo change made in WAIL's own UI is intent by construction — no threshold, no hold-down, no snapping to a plausible value. It broadcasts immediately and re-anchors the room. A DAW change stays *observed* and goes through (1). This is also what makes (2) safe: enforcement needs a sanctioned path for changing the room tempo, and before this there was none — the UI has only ever shown a read-only readout.

The concrete thresholds are deliberately **not** fixed here. Every attempt to reason them out in the design session produced a band that was neither reported, deferred to, nor correctable, because the three existing constants (`SlewMaxFraction`, the reporting bar, the steering gate) were tuned independently against different questions. They will be set from a simulation harness that replays the documented jitter shapes and measures the outcome — see Consequences.

## Considered options (rejected)

- **Keep the local session authoritative and raise the reporting bar** (what shipped in #499, 0.01 → 0.25 BPM). Suppresses the wobble, but opens a band where a standing divergence is never reported, never deferred to, and larger than the slew can close — so the slew writes over it, silently reverting a deliberate small change. Treating magnitude as the only filter cannot distinguish a 0.1 BPM wobble from a deliberate 0.1 BPM nudge.
- **Make the DAW knob inert outright** — only WAIL's UI may change the room tempo. Cleanest semantics, and rejected for the same reason 2026-07-26 rejected it: the DAW is where musicians set tempo, and taking that away is a worse product. Under this ADR the knob stays live for deliberate changes and goes inert only below the reporting bar, where WAIL's own control remains available.
- **Widen the slew so it can close larger divergence.** Rejected on field evidence already in the tree: the old 0.3 % cap (5.2 cents) was audible — the 2026-07-25 session heard every slew episode — which is why it is 0.05 % today. Widening also fails on direction: a faster slew reverts a user's tempo *sooner*.
- **Accept the drift.** A standing sub-threshold offset leaks phase for the whole session (~1–3 s/hour at 0.1 BPM), silently, in a workload built around holding one tempo for an hour.

## Consequences

- **Pillar 4 changes.** Ableton Link still owns time — beat, phase, and the timeline WAIL reads and writes — but the *room* is authoritative for tempo, and a peer's session is held to it. CONTEXT.md is updated accordingly.
- **The DAW knob is inert below the reporting bar.** A deliberate change smaller than that is not propagated and is nudged away. The escape hatch is WAIL's own control, where any size is honoured because it is declared rather than inferred. This is a user-visible contract and should be documented as one.
- **Enforcement must yield.** A DAW that re-asserts its tempo indefinitely (automation, an external clock) must not be fought forever: after repeated reverts, stop. This is the #424 two-enforcer lesson, worked out in `tradeoffs.md` and carried over unchanged.
- **The thresholds are measured, not chosen.** A deterministic in-process harness (one WAIL per LAN, coupled through a simulated relay) replays the jitter shapes this repo has actually observed — convergence nudges on merge, the two-peer adoption oscillator, the VPN offset teleport, crystal drift, post-change aftershocks, RTT/2 bias, the 119.9↔120 wobble — and the constants are set from what it measures. Until that exists, the reporting bar and the enforcement trigger are provisional.
- **Supersedes** the *steady-state room-tempo enforcement* entry in `tradeoffs.md` (Closed 2026-07-26, "superseded, not built"). Its worked-out edge cases are adopted rather than rediscovered: do not fight the slew, suspend during the post-`ChangeBpm` window until the re-anchor lands, and yield after repeated reverts.
