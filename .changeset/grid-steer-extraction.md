---
default: patch
---

Extract the grid steer into `internal/align` (#428): ADR-0006's grid-alignment orchestration (entry conformance, gated slew, snapshot-tempo arbitration, committed-tempo record, post-snap room-label derivation) moves out of the session select loop into one unit-tested module. Also adds NINJAM-like interval-placement verification to the tier2 audio E2E (step mode, now the default).
