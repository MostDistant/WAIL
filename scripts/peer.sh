#!/usr/bin/env bash
# Start a WAIL peer and join the test room.
#
# Usage:
#   ./scripts/peer.sh [BPM] [IPC_PORT]
#
# Examples:
#   ./scripts/peer.sh 120 9191   # peer A
#   ./scripts/peer.sh 121 9192   # peer B (different BPM to force a Link merge)
#
# Watch for these log lines:
#   ">>> INTERVAL BOUNDARY <<<"  — local boundary hit, index broadcasted
#   "Interval index mismatch"    — remote index differed, peer self-corrected
#
set -euo pipefail
cd "$(dirname "$0")/.."

BPM="${1:-120}"
IPC_PORT="${2:-9191}"

RUST_LOG="wail_app=info,wail_core=info,wail_net=info" \
  cargo run -p wail-app -- join \
    --room test \
    --server ws://localhost:9090 \
    --bpm "$BPM" \
    --bars 4 \
    --quantum 4 \
    --ipc-port "$IPC_PORT"
