---
default: patch
---

Fix audio dropout at interval boundaries by decoding WAIF frames incrementally instead of waiting for full interval assembly.
