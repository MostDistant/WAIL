#!/usr/bin/env bash
#
# Tier 2 audio E2E — validates WAIL's real Link Audio data path end-to-end on a
# single machine, no DAW required.
#
# It stands up a local relay, then runs two headless WAIL instances against it:
#   • SweepSender — injects a test WAV and ships it to the relay
#   • Receiver    — pulls the stream from the relay and republishes it as a real
#                   Link Audio channel on the LAN
# and finally runs linkaudio-probe, a DAW-free Link Audio consumer that subscribes
# to the republished channel (which is what *activates* the receiver's sink) and
# measures what actually arrives: buffers, RMS (non-silent?), an estimated dominant
# frequency (should climb with the sweep — proof the audio is intact and in order),
# and LAN loss.
#
# Two modes (TIER2_MODE):
#   step  (default) — the WAV is stepped tones: one constant frequency per
#         interval-length block, so received audio identifies WHICH content
#         block is playing. On top of the integrity checks, this verifies
#         NINJAM-like interval placement end-to-end: a block captured in room
#         interval N must arrive during the receiver's local grid interval
#         labeled N+D. Ground truth: the sender logs (room, content) per
#         boundary, the receiver's boundary log gives the local→room offset,
#         and the probe bins received frames by shared-grid interval (BPI-lens
#         beat stamps). Every second of received audio is checked.
#   sweep — the original rising log sweep: integrity/order/loss only.
#
# Exercises the parts the in-process `go test` suite can't: the real Link Audio
# Sink/Source UDP path, the paced emit loop, the capture drain, the relay round
# trip, real-time timing, and (step mode) hold-until-N+D playout placement.
#
# Usage:   scripts/tier2-e2e.sh
# Tunables (env): TIER2_PORT TIER2_BPM TIER2_SWEEP_DUR TIER2_PROBE_SECS TIER2_ROOM
#                 TIER2_MODE TIER2_BPI TIER2_D
#
# Exit code 0 = PASS, 1 = FAIL. Needs a working cgo toolchain + libopus + the
# Link SDK submodule (same as a normal build).

set -eo pipefail

PORT="${TIER2_PORT:-8899}"
BPM="${TIER2_BPM:-240}"                 # higher BPM ⇒ shorter intervals ⇒ audio flows sooner
MODE="${TIER2_MODE:-step}"              # step = interval placement check; sweep = integrity only
BPI="${TIER2_BPI:-16}"                  # room beats-per-interval (app default: 4 bars × 4)
D="${TIER2_D:-1}"                       # NINJAM interval offset (WAIL_INTERVAL_OFFSET default)
PROBE_SECS="${TIER2_PROBE_SECS:-40}"
ROOM="${TIER2_ROOM:-tier2-$$}"
# Stepped-tone params (step mode): block k plays at F0 + k*STEP Hz, one block
# per interval. BLOCK must equal the interval length in seconds.
F0=240
STEP=160
BLOCK="$(awk "BEGIN{print $BPI*60/$BPM}")"
if [ "$MODE" = step ]; then
  SWEEP_DUR="${TIER2_SWEEP_DUR:-60}"
else
  SWEEP_DUR="${TIER2_SWEEP_DUR:-45}"
fi

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

if [ "$MODE" = step ]; then
  log "generating ${SWEEP_DUR}s stepped tones (${BLOCK}s blocks = one interval, ${F0}Hz +${STEP}Hz/block)"
  "$WORK/gen-sweep" -o "$WORK/sweep.wav" -dur "$SWEEP_DUR" -block "$BLOCK" -f0 "$F0" -step "$STEP" >/dev/null
else
  log "generating ${SWEEP_DUR}s frequency sweep (80 Hz → 12 kHz)"
  "$WORK/gen-sweep" -o "$WORK/sweep.wav" -dur "$SWEEP_DUR" >/dev/null
fi

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
"$WORK/probe" -name wail-probe -match SweepSender -bpi "$BPI" >"$WORK/probe.log" 2>&1 &
probe_pid=$!
PIDS="$PIDS $probe_pid"
sleep "$PROBE_SECS"
kill "$probe_pid" 2>/dev/null || true
sleep 0.5

echo "---------------------------------------------------------------- probe log"
cat "$WORK/probe.log"
echo "--------------------------------------------------------------------------"

if [ "$MODE" = step ]; then
  # Ground truth for interval placement, all wall-clock correlated (Link only
  # shares beat phase mod the quantum, so absolute grid indices are per-peer
  # and CANNOT be compared across processes — ADR-0003):
  #   blockroom.txt — content position (seconds) at which each room interval
  #     STARTS on the sender, per its boundary marker. A heard frequency maps
  #     to content time ((f-f0)/step blocks × BLOCK seconds), and the room
  #     whose content range contains it is the capture room. Exact for ANY
  #     room tempo (the room may not run at TIER2_BPM — WAIL adopts the LAN
  #     Link session's tempo, pillar 4, and the room follows it).
  #   releases.txt  — wall time + room interval that STARTED PLAYING at each
  #     of the receiver's boundaries (its INTERVAL log line).
  # Assertion per received second: captureRoom(content) == playingRoom - D.
  awk '
    /\[wav-sender\] boundary room=/ {
      r=0; c=0
      for (i=1; i<=NF; i++) {
        if ($i ~ /^room=/)        r = substr($i,6)+0
        else if ($i ~ /^content=/) c = substr($i,9)+0
      }
      print c, r
    }' "$WORK/sender.log" > "$WORK/blockroom.txt"
  awk '/>>> INTERVAL local=/ {
      t = $1; gsub(/[:.]/, " ", t); split(t, p, " ")
      secs = p[1]*3600 + p[2]*60 + p[3] + p[4]/1e6
      r = 0
      for (i=1; i<=NF; i++) if ($i ~ /^room=/) r = substr($i,6)+0
      print secs, r
    }' "$WORK/receiver.log" > "$WORK/releases.txt"
  echo "---------------------------------------------------------------- ground truth"
  echo "sender room/content ranges: $(wc -l < "$WORK/blockroom.txt" | tr -d ' ')   receiver playout boundaries: $(wc -l < "$WORK/releases.txt" | tr -d ' ')"
  if [ ! -s "$WORK/releases.txt" ] || [ ! -s "$WORK/blockroom.txt" ]; then
    echo "VERDICT=FAIL" | tee "$WORK/verdict.txt"
    echo "missing ground truth (sender markers or receiver boundary log)" | tee -a "$WORK/verdict.txt"
  else
  awk -v d="$D" -v f0="$F0" -v step="$STEP" -v blockdur="$BLOCK" '
    FILENAME == ARGV[1] { cs[++nb] = $1; cr[nb] = $2; next }   # blockroom.txt: content-sec → room (sorted)
    FILENAME == ARGV[2] { rt[++nr] = $1; rr[nr] = $2; next }   # releases.txt: wall-sec → room playing
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
      # Probe timestamp: "YYYY/MM/DD HH:MM:SS" ($1, $2) → seconds of day.
      split($2, p, ":"); t = p[1]*3600 + p[2]*60 + p[3]
      if (rms >= 500 && freq > 0) {
        nonsilent++
        if (fmin==0 || freq<fmin) fmin=freq; if (freq>fmax) fmax=freq
        # Which room interval is playing at t? Latest boundary ≤ t, skipping
        # the straddle second right after a boundary (mixed content).
        ri = 0
        for (i = nr; i >= 1; i--) if (rt[i] <= t) { ri = i; break }
        if (ri == 0 || t - rt[ri] < 1.2) next
        # Content time from frequency: block k = (f-f0)/step, k*blockdur secs.
        c = (freq - f0)/step * blockdur
        if (c < -1) next
        # Capture room: the content range containing c, skipping seconds
        # within 1.4s of a range edge (freq tolerance ±50Hz ⇒ content-time
        # tolerance ±(50/step)*blockdur — ambiguous classification there).
        bi = 0
        for (i = nb; i >= 1; i--) if (cs[i] <= c) { bi = i; break }
        if (bi == 0) next
        if (c - cs[bi] < 1.4) next
        if (bi < nb && cs[bi+1] - c < 1.4) next
        checks++
        want = cr[bi] + d   # capture room + D == room now playing
        if (rr[ri] != want) {
          fails++
          printf "  ! placement: t=%d ~%.0f Hz (content %.1fs, captured room %d) heard while room %d plays, want room %d\n", \
                 t, freq, c, cr[bi], rr[ri], want
        }
      }
    }
    END {
      printf "subscribed=%d  non-silent-sec=%d  silent-sec=%d  maxLost=%d  lossEvents=%d  freq=%d..%d Hz\n", \
             subs+0, nonsilent+0, silent+0, maxlost+0, lossev+0, fmin+0, fmax+0
      printf "placement: %d checks, %d failures (every received second: playing room == capture room + D)\n", checks+0, fails+0
      pass = (subs>=1 && nonsilent>=10 && maxlost==0 && lossev==0 && checks>=12 && fails==0)
      print (pass ? "VERDICT=PASS" : "VERDICT=FAIL")
    }
  ' "$WORK/blockroom.txt" "$WORK/releases.txt" "$WORK/probe.log" | tee "$WORK/verdict.txt"
  fi
else
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
fi

if grep -q "VERDICT=PASS" "$WORK/verdict.txt"; then
  if [ "$MODE" = step ]; then
    log "PASS — intact, lossless audio + every received second verified at captured-room + D on the shared grid"
  else
    log "PASS — intact, in-order, lossless audio through the real Link Audio path"
  fi
  exit 0
fi
log "FAIL — see logs above (sender.log / receiver.log / relay.log in $WORK were removed on exit)"
echo "--- sender.log (tail) ---";   tail -20 "$WORK/sender.log"   2>/dev/null || true
echo "--- receiver.log (tail) ---"; tail -20 "$WORK/receiver.log" 2>/dev/null || true
exit 1
