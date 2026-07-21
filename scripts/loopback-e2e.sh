#!/usr/bin/env bash
#
# Loopback audio E2E — validates the server-echo loopback round trip in one
# process: a single headless WAIL sends dense music-like program material
# (cmd/gen-complex) to a local relay with -loopback, the relay echoes it back,
# and the client decodes + republishes it as a "(loopback)" Link Audio channel.
# linkaudio-probe subscribes and measures what arrives.
#
# PASS = the probe hears continuous, lossless audio: the full encode → relay →
# decode → playout path round-trips complex material without a single dropped
# frame. Complements tier2-e2e.sh (two-instance send/receive with a sweep).
#
# Usage:   scripts/loopback-e2e.sh
# Tunables (env): LOOP_PORT LOOP_BPM LOOP_PROBE_SECS LOOP_ROOM
#
# Exit code 0 = PASS, 1 = FAIL. Same toolchain needs as a normal build.

set -eo pipefail

PORT="${LOOP_PORT:-8899}"
BPM="${LOOP_BPM:-240}"
PROBE_SECS="${LOOP_PROBE_SECS:-40}"
ROOM="${LOOP_ROOM:-loopback-$$}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/wail-loopback.XXXXXX")"

PIDS=""
cleanup() {
  set +e
  for p in $PIDS; do kill "$p" 2>/dev/null; done
  sleep 0.5
  for p in $PIDS; do kill -9 "$p" 2>/dev/null; done
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '\n\033[1;36m[loopback-e2e]\033[0m %s\n' "$*"; }

export CGO_ENABLED=1

log "building relay, app, gen-complex, linkaudio-probe → $WORK"
( cd "$REPO_ROOT/signaling-server" && go build -o "$WORK/relay" . )
( cd "$REPO_ROOT/wail-app" && go build -o "$WORK/wail" . )
( cd "$REPO_ROOT/wail-app" && go build -o "$WORK/gen-complex" ./cmd/gen-complex )
( cd "$REPO_ROOT/wail-app" && go build -o "$WORK/probe" ./cmd/linkaudio-probe )

log "generating complex program material"
"$WORK/gen-complex" -o "$WORK/complex.wav" -dur 60 >/dev/null 2>&1

log "starting local relay on :${PORT}"
PORT="$PORT" DB_PATH="$WORK/rooms.db" "$WORK/relay" >"$WORK/relay.log" 2>&1 &
PIDS="$PIDS $!"
ready=""
for _ in $(seq 1 50); do
  if curl -fsS "http://localhost:${PORT}/health" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.2
done
[ -n "$ready" ] || { echo "FAIL: relay did not come up"; cat "$WORK/relay.log"; exit 1; }

log "launching WAIL: complex WAV sender + server-echo loopback in room '$ROOM'"
WAIL_SIGNAL_URL="ws://localhost:${PORT}" "$WORK/wail" -headless -room "$ROOM" -bpm "$BPM" \
  -instance 1 -name RoundTrip -wav "$WORK/complex.wav" -loopback >"$WORK/wail.log" 2>&1 &
PIDS="$PIDS $!"

log "letting the first echoed interval land…"
sleep 8

log "probing the republished (loopback) channel for ${PROBE_SECS}s"
"$WORK/probe" -name loop-probe -match "loopback" >"$WORK/probe.log" 2>&1 &
probe_pid=$!
PIDS="$PIDS $probe_pid"
sleep "$PROBE_SECS"
kill "$probe_pid" 2>/dev/null || true
sleep 0.5

echo "---------------------------------------------------------------- probe log"
tail -20 "$WORK/probe.log"
echo "--------------------------------------------------------------------------"

awk -v need=$((PROBE_SECS/2)) '
/→ subscribed/        { subs++ }
/! LAN loss/          { lossev++ }
/silent \(no buffers/ { silent++ }
/rms=/ {
  rms=0; lost=0
  for (i=1; i<=NF; i++) {
    if ($i ~ /^rms=/) rms = substr($i,5)+0
    else if ($i ~ /^lost=/) lost = substr($i,6)+0
  }
  if (lost > maxlost) maxlost = lost
  if (rms >= 500) nonsilent++
}
END {
  printf "subscribed=%d non-silent-sec=%d silent-sec=%d maxLost=%d lossEvents=%d\n", subs+0, nonsilent+0, silent+0, maxlost+0, lossev+0
  print (subs>=1 && nonsilent>=need && maxlost==0 && lossev==0) ? "VERDICT=PASS" : "VERDICT=FAIL"
}' "$WORK/probe.log" | tee "$WORK/verdict.txt"

if grep -q "VERDICT=PASS" "$WORK/verdict.txt"; then
  log "PASS — lossless, continuous loopback round trip through the relay"
  exit 0
fi
log "FAIL — see $WORK (kept for inspection)"
trap - EXIT
for p in $PIDS; do kill "$p" 2>/dev/null || true; done
exit 1
