---
default: minor
---

Add peer affinity slots — when a peer drops and rejoins, they reclaim their original DAW aux output slot.

Each WAIL installation now generates a persistent identity (UUID stored in app data) that survives restarts. This identity is exchanged in Hello messages and used by the recv plugin's IntervalRing to reserve slot assignments. When a peer disconnects, their slot is freed but an affinity reservation maps their identity to the old slot index. On reconnect (even with a new peer_id), the identity match reclaims the original slot.

Also adds slot number labels to the status update events so the frontend can display "Peer 1 (Ringo)", "Peer 2 (Paul)", etc.
