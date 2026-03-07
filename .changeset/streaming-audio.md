---
default: minor
---

NINJAM-style streaming audio: encode and transmit Opus frames every 20ms during the interval instead of batching the entire interval at the boundary. Receivers buffer frames progressively, improving delivery reliability and enabling shorter interval lengths on high-latency connections.
