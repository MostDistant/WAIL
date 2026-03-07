---
default: patch
---

Replace linear fade-in with equal-power crossfade at interval boundaries.

Applies to all interval transitions (not just peer joins): saves the last 128
samples per channel (256 interleaved, matching NINJAM's MAX_FADE constant) from
each peer's outgoing interval and blends them into the head of the incoming
interval using sin/cos weights (sin²+cos²=1), preserving constant energy
throughout the transition. New peers and reconnecting peers retain fade-from-silence
behaviour since their crossfade tail is zero-initialized.
