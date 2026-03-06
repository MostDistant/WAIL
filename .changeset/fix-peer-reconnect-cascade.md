---
default: patch
---

Fix stale peer list and broken audio after WebRTC peer disconnection.

A single ICE failure was generating up to 6 concurrent failure signals (from
Disconnected state, Failed state, DataChannel closes, and reader exits),
instantly exhausting the 5-attempt reconnect budget and spawning multiple
overlapping reconnect timers. Peers appeared stuck in the list while audio
stopped flowing to reconnected peers.

Now: Disconnected state no longer triggers failure signals (it's transient
and may recover), reader exits no longer duplicate DataChannel close signals,
and a `reconnect_pending` guard ensures only one reconnect timer runs per
peer per failure.
