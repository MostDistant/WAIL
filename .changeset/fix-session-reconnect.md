---
default: patch
---

Fix silent disconnections in long sessions by detecting DataChannel failures and dead reader tasks.

- Add `on_close` and `on_error` handlers to both sync and audio DataChannels (initiator and responder paths) so that DataChannel failures immediately signal peer failure via `failure_tx`
- Signal peer failure when sync/audio reader tasks exit (previously exited silently with no notification)
- Add peer liveness watchdog: track last message time per peer, close peers silent for >30 seconds
- Emit `session:stale` event after 10 failed signaling reconnection attempts so the UI can warn users
- Server-side eviction detection: signaling server now returns `evicted: true` when a deleted peer polls, and the client triggers reconnection instead of silently receiving empty responses
