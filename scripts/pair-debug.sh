#!/usr/bin/env bash
#
# pair-debug — drive two headless WAIL instances for live debugging:
# one on this machine (studio) and one on a remote machine over Tailscale SSH
# (default: andrews-laptop, 100.105.127.32). Each side's DAW talks to its local
# WAIL via Link Audio; WAIL bridges the two machines through the relay.
#
# Subcommands:
#   start    Build, ship the binary, spawn both instances into a fresh room,
#            and gate on both sides logging a successful room join.
#   logs     Stream both instances' stdout, prefixed [local]/[remote].
#            Ctrl-C stops watching only — the instances keep running.
#   stop     SIGTERM the recorded PIDs on both sides (never pkill), then
#            archive both stdout captures + rotating wail.log files into the
#            run directory.
#   status   Show the current session: run dir, room, relay, PID liveness.
#
# Usage:
#   scripts/pair-debug.sh start [--room NAME] [--password PW] [--bpm N]
#                               [--relay prod|local|ws://...|wss://...]
#                               [--local-test-tone] [--remote-test-tone]
#                               [--local-wav FILE] [--remote-wav FILE] [--force]
#   scripts/pair-debug.sh logs | stop | status
#
# Remote prerequisites (one-time): enable Remote Login, authorize this
# machine's SSH key, and `brew install opus opusfile` (dylib deps of the
# locally-built cgo binary).
#
# Tunables (env):
#   PAIR_REMOTE          ssh target            (default 100.105.127.32)
#   PAIR_REMOTE_DIR      remote install dir    (default ~/wail-debug)
#   PAIR_LOCAL_INSTANCE  local -instance num   (default 90)
#   PAIR_REMOTE_INSTANCE remote -instance num  (default 91)
#   PAIR_RELAY_PORT      port for --relay local (default 8899)
#   PAIR_JOIN_TIMEOUT    readiness gate secs   (default 20)
#
# Instance numbers 90/91 keep the harness's data dirs (~/.wail-90, ~/.wail-91)
# and identities far away from any GUI WAIL you may also have running.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUNS_ROOT="$REPO_ROOT/debug-runs"
CURRENT_LINK="$RUNS_ROOT/.current"

REMOTE="${PAIR_REMOTE:-100.105.127.32}"
REMOTE_DIR="${PAIR_REMOTE_DIR:-wail-debug}"   # relative to remote $HOME
LOCAL_INSTANCE="${PAIR_LOCAL_INSTANCE:-90}"
REMOTE_INSTANCE="${PAIR_REMOTE_INSTANCE:-91}"
RELAY_PORT="${PAIR_RELAY_PORT:-8899}"
JOIN_TIMEOUT="${PAIR_JOIN_TIMEOUT:-20}"

PROD_RELAY="wss://wail-relay.fly.dev"

log()  { printf '\033[1;36m[pair]\033[0m %s\n' "$*"; }
err()  { printf '\033[1;31m[pair]\033[0m %s\n' "$*" >&2; }
die()  { err "$*"; exit 1; }

ssh_remote() { ssh -o BatchMode=yes -o ConnectTimeout=10 "$REMOTE" "$@"; }

current_run() {
  [[ -f "$CURRENT_LINK" ]] || die "no active session (nothing to ${1:-do}); 'start' first"
  cat "$CURRENT_LINK"
}

# --- state file helpers -------------------------------------------------------
# state file lives at <run>/state, simple KEY=VALUE lines.
state_get() { grep "^$2=" "$1/state" 2>/dev/null | head -1 | cut -d= -f2-; }

# Kill $2 (pid) only if its command line contains ALL of the remaining args
# (expected substrings). $1 is "local" or "remote".
kill_verified() {
  local where="$1" pid="$2" cmd="" expect
  shift 2
  [[ -n "$pid" ]] || return 0
  if [[ "$where" == local ]]; then
    cmd="$(ps -p "$pid" -o command= 2>/dev/null || true)"
  else
    cmd="$(ssh_remote "ps -p $pid -o command= 2>/dev/null" || true)"
  fi
  if [[ -z "$cmd" ]]; then
    log "$where pid $pid already gone"
    return 0
  fi
  for expect in "$@"; do
    if [[ "$cmd" != *"$expect"* ]]; then
      err "$where pid $pid cmdline missing '$expect' ($cmd) — NOT killing (safety)"
      return 1
    fi
  done
  if [[ "$where" == local ]]; then
    kill -TERM "$pid" 2>/dev/null || true
  else
    ssh_remote "kill -TERM $pid 2>/dev/null" || true
  fi
  log "$where pid $pid SIGTERM sent ($cmd)"
}

# --- start --------------------------------------------------------------------
cmd_start() {
  local room="" password="" bpm="120" relay="prod"
  local local_tone="" remote_tone="" local_wav="" remote_wav="" force=""

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --room)            room="$2"; shift 2;;
      --password)        password="$2"; shift 2;;
      --bpm)             bpm="$2"; shift 2;;
      --relay)           relay="$2"; shift 2;;
      --local-test-tone) local_tone=1; shift;;
      --remote-test-tone) remote_tone=1; shift;;
      --local-wav)       local_wav="$2"; shift 2;;
      --remote-wav)      remote_wav="$2"; shift 2;;
      --force)           force=1; shift;;
      *) die "unknown start flag: $1";;
    esac
  done

  if [[ -f "$CURRENT_LINK" ]]; then
    if [[ -n "$force" ]]; then
      log "--force: stopping existing session first"
      cmd_stop
    else
      die "a session is already active ($(cat "$CURRENT_LINK")); 'stop' it or use --force"
    fi
  fi

  local ts run
  ts="$(date +%Y%m%d-%H%M%S)"
  run="$RUNS_ROOT/$ts"
  mkdir -p "$run"
  [[ -n "$room" ]] || room="debug-$ts"

  log "run dir:  $run"
  log "room:     $room"

  # --- relay selection ---
  local relay_url="" relay_pid=""
  case "$relay" in
    prod) relay_url="$PROD_RELAY";;
    local)
      relay_url="ws://localhost:$RELAY_PORT"
      ;;
    ws://*|wss://*) relay_url="$relay";;
    *) die "--relay must be prod, local, or a ws(s):// URL";;
  esac

  # --- build ---
  log "building wail (cgo) → $run/wail"
  ( cd "$REPO_ROOT/wail-app" && CGO_ENABLED=1 go build -o "$run/wail" . )

  if [[ "$relay" == local ]]; then
    log "building relay → $run/relay"
    ( cd "$REPO_ROOT/signaling-server" && go build -o "$run/relay" . )
    log "starting local relay on :$RELAY_PORT"
    "$run/relay" >"$run/relay.stdout.log" 2>&1 &
    relay_pid=$!
    # remote must reach it via this machine's Tailscale IP
    local ts_ip
    ts_ip="$(/Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4 2>/dev/null | head -1 || true)"
    [[ -n "$ts_ip" ]] || die "--relay local needs Tailscale up on this machine (to advertise the relay to the remote)"
    relay_url="ws://$ts_ip:$RELAY_PORT"
  fi
  log "relay:    $relay_url"

  # --- ship binary ---
  log "shipping binary → $REMOTE:$REMOTE_DIR/"
  ssh_remote "mkdir -p '$REMOTE_DIR'"
  scp -q "$run/wail" "$REMOTE:$REMOTE_DIR/wail"

  # remote WAV must exist over there; ship it if given
  local remote_wav_arg=""
  if [[ -n "$remote_wav" ]]; then
    [[ -f "$remote_wav" ]] || die "--remote-wav: $remote_wav not found locally (it is shipped from here)"
    scp -q "$remote_wav" "$REMOTE:$REMOTE_DIR/inject.wav"
    remote_wav_arg="$REMOTE_DIR/inject.wav"
  fi

  # --- spawn local ---
  local -a largs=( -headless -room "$room" -bpm "$bpm" -name studio -instance "$LOCAL_INSTANCE" )
  [[ -n "$password" ]]  && largs+=( -password "$password" )
  [[ -n "$local_tone" ]] && largs+=( -test-tone )
  [[ -n "$local_wav" ]]  && largs+=( -wav "$local_wav" )
  log "starting local instance (instance $LOCAL_INSTANCE)"
  WAIL_SIGNAL_URL="$relay_url" "$run/wail" "${largs[@]}" >"$run/local.stdout.log" 2>&1 &
  local local_pid=$!

  # --- spawn remote ---
  local -a rargs=( -headless -room "$room" -bpm "$bpm" -name laptop -instance "$REMOTE_INSTANCE" )
  [[ -n "$password" ]]     && rargs+=( -password "$password" )
  [[ -n "$remote_tone" ]]  && rargs+=( -test-tone )
  [[ -n "$remote_wav_arg" ]] && rargs+=( -wav "$remote_wav_arg" )
  log "starting remote instance on $REMOTE (instance $REMOTE_INSTANCE)"
  local rargs_q remote_pid
  printf -v rargs_q '%q ' "${rargs[@]}"
  remote_pid="$(ssh_remote "cd '$REMOTE_DIR' && nohup env WAIL_SIGNAL_URL='$relay_url' ./wail $rargs_q >remote.stdout.log 2>&1 & echo \$!")"
  [[ "$remote_pid" =~ ^[0-9]+$ ]] || die "failed to get remote PID: $remote_pid"

  # --- record state ---
  cat >"$run/state" <<EOF
ROOM=$room
RELAY=$relay_url
LOCAL_PID=$local_pid
REMOTE_PID=$remote_pid
RELAY_PID=$relay_pid
LOCAL_INSTANCE=$LOCAL_INSTANCE
REMOTE_INSTANCE=$REMOTE_INSTANCE
EOF
  mkdir -p "$RUNS_ROOT"
  ln -sfn "$run" "$RUNS_ROOT/latest"
  echo "$run" >"$CURRENT_LINK"

  # --- readiness gate ---
  log "waiting up to ${JOIN_TIMEOUT}s for both sides to join the room..."
  local deadline=$(( $(date +%s) + JOIN_TIMEOUT )) local_ok="" remote_ok=""
  while (( $(date +%s) < deadline )); do
    [[ -z "$local_ok" ]]  && grep -q "Joined room" "$run/local.stdout.log" 2>/dev/null && local_ok=1
    if [[ -z "$remote_ok" ]]; then
      ssh_remote "grep -q 'Joined room' '$REMOTE_DIR/remote.stdout.log'" 2>/dev/null && remote_ok=1
    fi
    [[ -n "$local_ok" && -n "$remote_ok" ]] && break
    # bail early if either process died
    kill -0 "$local_pid" 2>/dev/null || break
    ssh_remote "kill -0 $remote_pid 2>/dev/null" || break
    sleep 1
  done

  if [[ -z "$local_ok" || -z "$remote_ok" ]]; then
    err "readiness gate FAILED (local: ${local_ok:-no}, remote: ${remote_ok:-no})"
    err "--- local tail ---";  tail -n 15 "$run/local.stdout.log" >&2 || true
    err "--- remote tail ---"; ssh_remote "tail -n 15 '$REMOTE_DIR/remote.stdout.log'" >&2 || true
    cmd_stop || true
    exit 1
  fi

  log "READY — both instances joined room '$room'"
  log "  local:  pid $local_pid  (instance $LOCAL_INSTANCE, name 'studio')"
  log "  remote: pid $remote_pid on $REMOTE (instance $REMOTE_INSTANCE, name 'laptop')"
  log "  relay:  $relay_url"
  log "next: scripts/pair-debug.sh logs   |   scripts/pair-debug.sh stop"
}

# --- logs ---------------------------------------------------------------------
cmd_logs() {
  local run; run="$(current_run logs)"
  log "streaming logs from $run (Ctrl-C stops watching only)"
  local pids=""
  cleanup() { [[ -n "$pids" ]] && kill $pids 2>/dev/null; wait 2>/dev/null; }
  trap cleanup INT TERM
  tail -n +1 -f "$run/local.stdout.log" | sed -u 's/^/[local]  /' &
  pids="$pids $!"
  ssh_remote "tail -n +1 -f '$REMOTE_DIR/remote.stdout.log'" | sed -u 's/^/[remote] /' &
  pids="$pids $!"
  wait
}

# --- stop ---------------------------------------------------------------------
cmd_stop() {
  local run; run="$(current_run stop)"
  local local_pid remote_pid relay_pid local_inst remote_inst
  local_pid="$(state_get "$run" LOCAL_PID)"
  remote_pid="$(state_get "$run" REMOTE_PID)"
  relay_pid="$(state_get "$run" RELAY_PID)"
  local_inst="$(state_get "$run" LOCAL_INSTANCE)";  local_inst="${local_inst:-$LOCAL_INSTANCE}"
  remote_inst="$(state_get "$run" REMOTE_INSTANCE)"; remote_inst="${remote_inst:-$REMOTE_INSTANCE}"

  log "stopping session $(basename "$run")"
  kill_verified local  "$local_pid"  "$run/wail" || true
  # remote cmdline is "./wail ... -instance N" (relative, no dir prefix), so
  # match the binary name plus the harness's per-side instance number
  kill_verified remote "$remote_pid" "wail" "-headless" "-instance $remote_inst" || true
  [[ -n "$relay_pid" ]] && kill_verified local "$relay_pid" "$run/relay" || true

  # give graceful shutdown a moment (headless handles SIGTERM → leave room)
  sleep 2

  log "collecting logs → $run"
  scp -q "$REMOTE:$REMOTE_DIR/remote.stdout.log" "$run/remote.stdout.log" 2>/dev/null \
    || err "could not fetch remote stdout (already have a partial stream?)"
  scp -q "$REMOTE:$HOME/.wail-$remote_inst/logs/wail.log" "$run/remote.wail.log" 2>/dev/null \
    || err "no remote rotating log at ~/.wail-$remote_inst/logs/wail.log"
  cp "$HOME/.wail-$local_inst/logs/wail.log" "$run/local.wail.log" 2>/dev/null \
    || err "no local rotating log at ~/.wail-$local_inst/logs/wail.log"

  rm -f "$CURRENT_LINK"
  log "done. artifacts:"
  ls -la "$run" | sed 's/^/    /'
}

# --- status -------------------------------------------------------------------
cmd_status() {
  local run; run="$(current_run status)"
  cat "$run/state" | sed 's/^/    /'
  local local_pid remote_pid
  local_pid="$(state_get "$run" LOCAL_PID)"
  remote_pid="$(state_get "$run" REMOTE_PID)"
  if kill -0 "$local_pid" 2>/dev/null; then log "local pid $local_pid: ALIVE"; else err "local pid $local_pid: DEAD"; fi
  if ssh_remote "kill -0 $remote_pid 2>/dev/null"; then log "remote pid $remote_pid: ALIVE"; else err "remote pid $remote_pid: DEAD"; fi
  log "run dir: $run"
}

# --- dispatch -----------------------------------------------------------------
case "${1:-}" in
  start)  shift; cmd_start "$@";;
  logs)   cmd_logs;;
  stop)   cmd_stop;;
  status) cmd_status;;
  *)      sed -n '2,40p' "$0"; exit 1;;
esac
