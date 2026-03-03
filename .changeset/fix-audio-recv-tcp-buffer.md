---
type: patch
---

Fix audio receive failure after plugin split: send and recv plugins now identify themselves via IPC role bytes, and the session only forwards received audio to recv plugin connections. This prevents TCP buffer bloat on the send plugin's connection (which never reads forwarded audio), fixing the deadlock that caused no `[AUDIO RECV]` events. Adds AudioStatus message for remote audio pipeline health monitoring, improves DC state tracking with responder on_open callback, and escalates audio drop logs to warn level.
