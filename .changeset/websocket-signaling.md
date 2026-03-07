---
default: minor
---

Replace HTTP polling signaling with WebSocket for instant message delivery. Connection setup drops from ~15s to under 1s. Adds a Go WebSocket signaling server (SQLite-backed, deployed on fly.io) and replaces the Val Town dependency.
