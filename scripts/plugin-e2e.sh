#!/usr/bin/env bash
#
# Plugin-chain E2E — validates the CLAP bridge plugin path (ADR-0005) end-to-end
# against two real WAIL apps, no DAW required.
#
# It stands up a local relay, then two headless WAIL instances in one room:
#   • ChainSender   — captures audio from a wail-send plugin over loopback IPC
#   • ChainReceiver — decodes the stream and ships it to a wail-recv plugin over IPC
# and finally runs wail-plugin-chain (plugins/tests/chain_main.cpp), which hosts
# both plugins via clap-trap and drives them in real time like a DAW would: a log
# sweep (80 Hz → 12 kHz) goes in, and what comes back on recv port 0 is measured
# per second (RMS + zero-crossing frequency, same scheme as linkaudio-probe).
#
# This exercises the whole plugin contract that tier2-e2e.sh can't: RawPCM capture
# framing, the ipcCaptureSource Link-clock anchor, Opus/WAIF/relay round trip,
# hold-until-N+D playout, and the ipcEmitSink → RemotePCM → plugin ring path.
#
# Usage:   scripts/plugin-e2e.sh
# Tunables (env): PLUGE2E_PORT PLUGE2E_BPM PLUGE2E_SECS PLUGE2E_TAIL_SECS PLUGE2E_ROOM
#
# Exit code 0 = PASS, 1 = FAIL. Needs cgo + libopus + the Link SDK submodule (for
# the app) and vendor/clap (for the plugins).

set -eo pipefail

PORT="${PLUGE2E_PORT:-8898}"
BPM="${PLUGE2E_BPM:-240}"               # higher BPM ⇒ shorter intervals ⇒ audio flows sooner
SECS="${PLUGE2E_SECS:-30}"
TAIL_SECS="${PLUGE2E_TAIL_SECS:-12}"    # received audio lags the sweep by ~2 intervals
ROOM="${PLUGE2E_ROOM:-pluge2e-$$}"
SEND_INSTANCE=11                        # IPC ports: 9191 + instance
RECV_INSTANCE=12

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/wail-pluge2e.XXXXXX")"
RELAY_URL="ws://localhost:${PORT}"
PLUGINS_BUILD="$REPO_ROOT/build/plugins"   # persistent: cmake + clap-trap fetch stay incremental

PIDS=""
cleanup() {
  set +e
  for p in $PIDS; do kill "$p" 2>/dev/null; done
  sleep 0.5
  for p in $PIDS; do kill -9 "$p" 2>/dev/null; done
  if [ -n "${PLUGE2E_KEEP:-}" ]; then
    echo "logs kept in $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT INT TERM

log() { printf '\n\033[1;36m[plugin-e2e]\033[0m %s\n' "$*"; }

export CGO_ENABLED=1

log "building relay + app → $WORK"
( cd "$REPO_ROOT/signaling-server" && go build -o "$WORK/relay" . )
( cd "$REPO_ROOT/wail-app" && go build -o "$WORK/wail" . )

log "building plugins + harness (cmake, -DWAIL_PLUGIN_TESTS=ON) → $PLUGINS_BUILD"
cmake -S "$REPO_ROOT/plugins" -B "$PLUGINS_BUILD" -DCMAKE_BUILD_TYPE=Release -DWAIL_PLUGIN_TESTS=ON >/dev/null
cmake --build "$PLUGINS_BUILD" >/dev/null

CHAIN="$PLUGINS_BUILD/tests/wail-plugin-chain"
case "$(uname)" in
  Darwin)
    SEND_CLAP="$PLUGINS_BUILD/wail-send.clap/Contents/MacOS/wail-send"
    RECV_CLAP="$PLUGINS_BUILD/wail-recv.clap/Contents/MacOS/wail-recv"
    ;;
  *)
    SEND_CLAP="$PLUGINS_BUILD/wail-send.clap"
    RECV_CLAP="$PLUGINS_BUILD/wail-recv.clap"
    ;;
esac
[ -x "$CHAIN" ] && [ -e "$SEND_CLAP" ] && [ -e "$RECV_CLAP" ] || { echo "FAIL: harness/plugin artifacts missing"; exit 1; }

log "starting local relay on :${PORT}"
PORT="$PORT" DB_PATH="$WORK/rooms.db" "$WORK/relay" >"$WORK/relay.log" 2>&1 &
PIDS="$PIDS $!"
ready=""
for _ in $(seq 1 50); do
  if curl -fsS "http://localhost:${PORT}/health" >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.2
done
[ -n "$ready" ] || { echo "FAIL: relay did not come up"; cat "$WORK/relay.log"; exit 1; }

log "launching ChainSender (instance $SEND_INSTANCE, IPC :$((9191+SEND_INSTANCE))) + ChainReceiver (instance $RECV_INSTANCE, IPC :$((9191+RECV_INSTANCE))) in room '$ROOM' at ${BPM} BPM"
WAIL_SIGNAL_URL="$RELAY_URL" "$WORK/wail" -headless -room "$ROOM" -bpm "$BPM" \
  -instance "$SEND_INSTANCE" -name ChainSender >"$WORK/sender.log" 2>&1 &
PIDS="$PIDS $!"
WAIL_SIGNAL_URL="$RELAY_URL" "$WORK/wail" -headless -room "$ROOM" -bpm "$BPM" \
  -instance "$RECV_INSTANCE" -name ChainReceiver >"$WORK/receiver.log" 2>&1 &
PIDS="$PIDS $!"

log "letting them connect + sync Link / room clock…"
sleep 6

log "running chain driver: ${SECS}s sweep + ${TAIL_SECS}s tail, real time"
"$CHAIN" --send "$SEND_CLAP" --recv "$RECV_CLAP" \
  --send-port $((9191+SEND_INSTANCE)) --recv-port $((9191+RECV_INSTANCE)) \
  --seconds "$SECS" --tail-seconds "$TAIL_SECS" >"$WORK/chain.log" 2>&1 || {
    echo "FAIL: chain driver exited nonzero"; cat "$WORK/chain.log"; exit 1;
  }

echo "---------------------------------------------------------------- chain log"
cat "$WORK/chain.log"
echo "--------------------------------------------------------------------------"

awk '
/port0 name=/             { named++ }
/rms=/ {
  sec=0; rms=0; freq=0; oth=0
  for (i=1; i<=NF; i++) {
    if ($i ~ /^sec=/)         sec  = substr($i,5)+0
    else if ($i ~ /^rms=/)    rms  = substr($i,5)+0
    else if ($i ~ /^~/)       freq = substr($i,2)+0
    else if ($i ~ /^others=/) oth  = substr($i,8)+0
  }
  othersTotal += oth
  if (rms >= 500) {
    nonsilent++
    if (first == -1) first = sec
    if (freq > 0) { if (fmin==0 || freq<fmin) fmin=freq; if (freq>fmax) fmax=freq }
  }
}
BEGIN { first = -1 }
END {
  ratio = (fmin>0) ? fmax/fmin : 0
  printf "named=%d  firstAudioSec=%d  non-silent-sec=%d  others=%d  freq=%d..%d Hz (x%.1f)\n", \
         named+0, first, nonsilent+0, othersTotal+0, fmin+0, fmax+0, ratio
  pass = (named>=1 && first>=0 && first<=15 && nonsilent>=8 && othersTotal==0 && fmax>=2000 && ratio>=1.5)
  print (pass ? "VERDICT=PASS" : "VERDICT=FAIL")
}
' "$WORK/chain.log" | tee "$WORK/verdict.txt"

for f in sender receiver; do
  echo "--- $f.log [ipc] lines ---"
  grep -i "\[ipc\]" "$WORK/$f.log" || echo "(none)"
done

grep -q "plugin send stream 0 registered" "$WORK/sender.log" \
  || { echo "FAIL: sender never registered the plugin stream"; tail -20 "$WORK/sender.log"; exit 1; }
grep -q "recv plugin connected" "$WORK/receiver.log" \
  || { echo "FAIL: receiver never saw the recv plugin"; tail -20 "$WORK/receiver.log"; exit 1; }

if grep -q "VERDICT=PASS" "$WORK/verdict.txt"; then
  log "PASS — intact, in-order audio through the CLAP plugin bridge + relay round trip"
  exit 0
fi
log "FAIL — see logs above (sender.log / receiver.log / relay.log removed on exit)"
echo "--- sender.log (tail) ---";   tail -20 "$WORK/sender.log"   2>/dev/null || true
echo "--- receiver.log (tail) ---"; tail -20 "$WORK/receiver.log" 2>/dev/null || true
exit 1
