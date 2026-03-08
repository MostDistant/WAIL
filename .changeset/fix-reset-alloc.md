---
default: patch
---

Fix crash when Bitwig (or other hosts) calls start_processing: plugin reset() was
dropping heap-allocated data (Strings, encoder/decoder objects) inside assert_no_alloc's
no-alloc zone. Wrapped reset() bodies in permit_alloc, matching the existing pattern
used in process().
