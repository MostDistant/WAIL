#!/usr/bin/env bash
#
# Tier 2 audio E2E — validates WAIL's real Link Audio data path end-to-end on a
# single machine, no DAW required.
#
# It stands up a local relay, then runs two headless WAIL instances against it:
#   • SweepSender — injects a rising frequency-sweep WAV and ships it to the relay
#   • Receiver    — pulls the stream from the relay and republishes it as a real
#                   Link Audio channel on the LAN
# and finally runs linkaudio-probe, a DAW-free Link Audio consumer that subscribes
# to the republished channel (which is what *activates* the receiver's sink) and
# measures what actually arrives: buffers, RMS (non-silent?), an estimated dominant
# frequency (should climb with the sweep — proof the audio is intact and in order),
# and LAN loss.
#
# Exercises the parts the in-process `go test` suite can't: the real Link Audio
# Sink/Source UDP path, the paced emit loop, the capture drain, the relay round
# trip, and real-time timing.
#
# Usage:   scripts/tier2-e2e.sh
# Tunables (env): TIER2_PORT TIER2_BPM TIER2_SWEEP_DUR TIER2_PROBE_SECS TIER2_ROOM
#
# Exit code 0 = PASS, 1 = FAIL. Needs a working cgo toolchain + libopus + the
# Link SDK submodule (same as a normal build).

set -eo pipefail

PORT="${TIER2_PORT:-8899}"
BPM="${TIER2_BPM:-240}"                 # higher BPM ⇒ shorter intervals ⇒ audio flows sooner
SWEEP_DUR="${TIER2_SWEEP_DUR:-45}"
PROBE_SECS="${TIER2_PROBE_SECS:-40}"
ROOM="${TIER2_ROOM:-tier2-$$}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/wail-tier2.XXXXXX")"
RELAY_URL="ws://localhost:${PORT}"

PIDS=""
cleanup() {
  set +e
  for p in $PIDS; do kill "$p" 2>/dev/null; done
  sleep 0.5
  for p in $PIDS; do kill -9 "$p" 2>/dev/null; done
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

log() { printf '\n\033[1;36m[tier2]\033[0m %s\n' "$*"; }

export CGO_ENABLED=1

log "building relay, app, gen-sweep, linkaudio-probe → $WORK"
( cd "$REPO_ROOT/signaling-server" && go build -o "$WORK/relay" . )
( cd "$REPO_ROOT/wail-app" && go build -o "$WORK/wail" . )
( cd "$REPO_ROOT/wail-app" && go build -o "$WORK/gen-sweep" ./cmd/gen-sweep )
( cd "$REPO_ROOT/wail-app" && go build -o "$WORK/probe" ./cmd/linkaudio-probe )

log "generating ${SWEEP_DUR}s frequency sweep (80 Hz → 12 kHz)"
"$WORK/gen-sweep" -o "$WORK/sweep.wav" -dur "$SWEEP_DUR" >/dev/null

log "starting local relay on :${PORT}"
PORT="$PORT" DB_PATH="$WORK/rooms.db" "$WORK/relay" >"$WORK/relay.log" 2>&1 &
PIDS="$PIDS $!"
ready=""
for _ in $(seq 1 50); do
  if curl -fsS "http://localhost:${PORT}/health" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.2
done
[ -n "$ready" ] || { echo "FAIL: relay did not come up"; cat "$WORK/relay.log"; exit 1; }

log "launching SweepSender + Receiver in room '$ROOM' at ${BPM} BPM"
WAIL_SIGNAL_URL="$RELAY_URL" "$WORK/wail" -headless -room "$ROOM" -bpm "$BPM" \
  -instance 1 -name SweepSender -wav "$WORK/sweep.wav" >"$WORK/sender.log" 2>&1 &
PIDS="$PIDS $!"
WAIL_SIGNAL_URL="$RELAY_URL" "$WORK/wail" -headless -room "$ROOM" -bpm "$BPM" \
  -instance 2 -name Receiver >"$WORK/receiver.log" 2>&1 &
PIDS="$PIDS $!"

log "letting them connect + fill the first interval(s)…"
sleep 6

log "probing republished Link Audio for ${PROBE_SECS}s (subscribing activates the sink)"
"$WORK/probe" -name wail-probe -match SweepSender >"$WORK/probe.log" 2>&1 &
probe_pid=$!
PIDS="$PIDS $probe_pid"
sleep "$PROBE_SECS"
kill "$probe_pid" 2>/dev/null || true
sleep 0.5

echo "---------------------------------------------------------------- probe log"
cat "$WORK/probe.log"
echo "--------------------------------------------------------------------------"

awk '
/→ subscribed/          { subs++ }
/! LAN loss/            { lossev++ }
/silent \(no buffers/   { silent++ }
/rms=/ {
  rms=0; freq=0; lost=0
  for (i=1; i<=NF; i++) {
    if ($i ~ /^rms=/)       rms  = substr($i,5)+0
    else if ($i ~ /^~/)     freq = substr($i,2)+0
    else if ($i ~ /^lost=/) lost = substr($i,6)+0
  }
  if (lost > maxlost) maxlost = lost
  if (rms >= 500) {
    nonsilent++
    if (freq > 0) { if (fmin==0 || freq<fmin) fmin=freq; if (freq>fmax) fmax=freq }
  }
}
END {
  ratio = (fmin>0) ? fmax/fmin : 0
  printf "subscribed=%d  non-silent-sec=%d  silent-sec=%d  maxLost=%d  lossEvents=%d  freq=%d..%d Hz (x%.1f)\n", \
         subs+0, nonsilent+0, silent+0, maxlost+0, lossev+0, fmin+0, fmax+0, ratio
  pass = (subs>=1 && nonsilent>=10 && maxlost==0 && lossev==0 && fmax>=2000 && ratio>=1.5)
  print (pass ? "VERDICT=PASS" : "VERDICT=FAIL")
}
' "$WORK/probe.log" | tee "$WORK/verdict.txt"

if grep -q "VERDICT=PASS" "$WORK/verdict.txt"; then
  log "PASS — intact, in-order, lossless audio through the real Link Audio path"
  exit 0
fi
log "FAIL — see logs above (sender.log / receiver.log / relay.log in $WORK were removed on exit)"
echo "--- sender.log (tail) ---";   tail -20 "$WORK/sender.log"   2>/dev/null || true
echo "--- receiver.log (tail) ---"; tail -20 "$WORK/receiver.log" 2>/dev/null || true
exit 1
