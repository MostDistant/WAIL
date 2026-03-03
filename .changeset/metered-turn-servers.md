---
default: minor
---

# Automatic TURN server credentials via Metered

WAIL now fetches TURN relay credentials automatically from Metered's REST API at session start. This replaces the manual TURN URL/username/credential configuration in the join-room UI. Credentials are short-lived (not stored in source), and the app falls back to STUN-only if the fetch fails.
