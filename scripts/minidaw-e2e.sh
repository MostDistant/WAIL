#!/usr/bin/env bash
#
# Mini-DAW system E2E — the deterministic form of the field offset ritual.
#
# Topology (3 processes, no DAW):
#   local relay  ←  headless WAIL app (-metronome-broadcast -loopback)
#                      metronome grid-rendered → room → echoed back →
#                      republished as a "WAIL · …" Link Audio channel
#   minidaw      —  a miniature DAW (own Link peer + hosted WAIL Receive +
#                   Link-faithful rolling transport) that measures the phase
#                   offset of arriving click onsets against the session grid
#
# The sender's click is grid-aligned by construction, so any measured sub-beat
# offset is the receive chain's error: publish path, bridge filter, stamp
# alignment, output pipeline. Exit 0 = PASS (|median offset| <= threshold).
#
# Usage:   scripts/minidaw-e2e.sh
# Tunables (env): MDE2E_PORT MDE2E_BPM MDE2E_SECS MDE2E_THRESHOLD_MS MDE2E_VERBOSE

set -eo pipefail

PORT="${MDE2E_PORT:-8897}"
BPM="${MDE2E_BPM:-240}"
SECS="${MDE2E_SECS:-25}"
THRESHOLD_MS="${MDE2E_THRESHOLD_MS:-5}"
ROOM="minidaw-e2e-$$"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/wail-minidaw.XXXXXX")"
RELAY_URL="ws://localhost:${PORT}"

PIDS=""
cleanup() {
  set +e
  for p in $PIDS; do kill "$p" 2>/dev/null; done
  sleep 0.5
  for p in $PIDS; do kill -9 "$p" 2>/dev/null; done
  if [ -n "${MDE2E_KEEP:-}" ]; then
    echo "logs kept in $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT INT TERM

log() { printf '\n\033[1;36m[minidaw-e2e]\033[0m %s\n' "$*"; }

export CGO_ENABLED=1

log "building relay + app + minidaw → $WORK"
( cd "$REPO_ROOT/signaling-server" && go build -o "$WORK/relay" . )
( cd "$REPO_ROOT/wail-app" && go build -o "$WORK/wail" . )
cmake -S "$REPO_ROOT/plugins" -B "$REPO_ROOT/build/plugins" > /dev/null
cmake --build "$REPO_ROOT/build/plugins" --target minidaw wail-recv > /dev/null

MINIDAW="$REPO_ROOT/build/plugins/tests/minidaw"
RECV_PLUGIN="$REPO_ROOT/build/plugins/wail-recv.clap"
# The CLAP bundle on macOS is a directory; the loader wants the binary inside.
if [ -d "$RECV_PLUGIN" ]; then
  RECV_PLUGIN="$RECV_PLUGIN/Contents/MacOS/wail-recv"
fi

log "starting local relay on :${PORT} (room $ROOM)"
PORT="$PORT" DB_PATH="$WORK/rooms.db" "$WORK/relay" >"$WORK/relay.log" 2>&1 &
PIDS="$PIDS $!"
sleep 0.5

log "starting app: metronome-broadcast + loopback (instance 30, ${BPM} BPM)"
WAIL_SIGNAL_URL="$RELAY_URL" "$WORK/wail" -headless -room "$ROOM" -bpm "$BPM" \
  -instance 30 -metronome-broadcast -loopback \
  >"$WORK/app.log" 2>&1 &
PIDS="$PIDS $!"

# Wait for the loopback channel to be published before measuring.
sleep 4

log "running minidaw (${SECS}s, threshold ±${THRESHOLD_MS}ms)"
set +e
VERBOSE_FLAG=""
if [ -n "${MDE2E_VERBOSE:-}" ]; then VERBOSE_FLAG="--verbose"; fi
"$MINIDAW" recv --plugin "$RECV_PLUGIN" --seconds "$SECS" \
  --threshold-ms "$THRESHOLD_MS" --name-contains Metronome $VERBOSE_FLAG \
  2>&1 | tee "$WORK/minidaw.log"
RC=${PIPESTATUS[0]}
set -e

if [ "$RC" -eq 0 ]; then
  log "PASS — click lands on the session grid within ±${THRESHOLD_MS}ms"
else
  log "FAIL — see $WORK/minidaw.log and $WORK/app.log"
fi
exit "$RC"
