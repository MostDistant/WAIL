---
default: patch
---

Increase peer audio DataChannel receive buffer from 64 to 256 frames and upgrade
silent drop logging from debug to warn. Add burst (zero-delay) audio phase to e2e
test to validate buffer headroom under high-frequency packet sends.
