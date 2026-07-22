# Room interval length (BPI) is communicated, not enforced

WAIL exposes the room interval length to users as BPI (beats per interval), NINJAM-style, with guidance to set the DAW's launch quantization to match. The motivating scenario — a joining musician's clips should launch on the room's interval grid — cannot be delivered by automation: Link shares only tempo and beat position, quantum is a per-app phase lens that is never transmitted (Live ties its own to Global Quantization), and no Link or WAIL mechanism can read or set a DAW's launch quantization. So the feature is communication — display and prompts — not enforcement.

## Decisions

- **BPI is presentation over the existing Bars × Quantum model.** No wire-format or protocol change: `IntervalConfig` and the WAIF trailer keep carrying integer bars + quantum; the UI displays and accepts beats (bars × beats per bar), default 16, shown as "16 (4 bars)".
- **Whole-bar validation.** User-entered BPI must be a multiple of beats per bar; non-multiples (e.g. 10 beats in 4/4) are rejected. Raw-beats NINJAM intervals that aren't bar-aligned are out of scope — they would require carrying beats instead of bars in the protocol and would break the bar-denominated launch-quantization guidance.
- **Founder-sets, joiners-adopt; anyone can change mid-session.** The first peer's config anchors the room clock (ADR-0003); joiners adopt via `interval_anchor`/`IntervalConfig`, and their join-time preference is only used when founding an empty room. Mid-session changes broadcast `IntervalConfig` and the relay reanchors at the next interval boundary — NINJAM-consistent, expected to be rare.
- **Join prompt, silent changes.** On first anchor receipt, a dismissable prompt tells the user the room's BPI and to set their DAW's launch quantization to match. Mid-session changes are silent — peers see the persistent readout update, no toast.

## Consequences

- **Peers must re-broadcast the *adopted* room config, not their join-time preference.** The gossip convergence (each peer re-broadcasts `IntervalConfig` when a peer joins) is last-writer-wins at the relay; a peer that adopted a different config than it joined with would flap the room clock and trigger a mid-jam reanchor. This is a latent bug in `session.go` that the mid-session-change feature makes load-bearing.
- If true free-BPI (non-bar-aligned) rooms are ever wanted, that is a wire-format change (beats instead of bars in `IntervalConfig` and the WAIF trailer) — a deliberate follow-up, not something to smuggle in through the input field.
- The quantum field remains in the UI (labeled "Beats per bar", default 4): it is the only way non-4/4 rooms are possible, and the beats display must stay honest for them ("12 (4 bars)").
