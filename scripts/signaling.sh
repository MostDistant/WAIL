#!/usr/bin/env bash
# Start the WAIL signaling server on :9090
set -euo pipefail
cd "$(dirname "$0")/.."
RUST_LOG=info cargo run -p wail-signaling
