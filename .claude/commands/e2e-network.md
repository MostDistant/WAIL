---
description: Run cross-network e2e test in Docker (isolated networks + TURN + WAN simulation)
allowed-tools: [Bash]
---

# Cross-Network E2E Test

Run the Docker-based cross-network end-to-end test: two peers on separate Docker networks communicate via a local TURN server with simulated WAN conditions (latency, jitter, packet loss).

Two phases: happy path validation, then chaos testing (disconnect/rejoin + transport stop/start).

## Arguments

The user may pass environment variable overrides, e.g. `/e2e-network WAN_DELAY=100ms WAN_LOSS=5%`.

Supported variables:
- `WAN_DELAY` (default 50ms), `WAN_JITTER` (default 10ms), `WAN_LOSS` (default 1%)
- `PEER_A_NOTES`, `PEER_B_NOTES` — custom note scripts for each peer
- `PEER_A_CHAOS`, `PEER_B_CHAOS` — chaos scripts for each peer
- `VALIDATE_INTERVALS` (default 4), `VALIDATE_TIMEOUT` (default 120)

## Instructions

### Pre-flight

1. Ensure git submodules are initialized:
   `git submodule update --init --recursive`

2. Clean up any leftover state:
   `docker compose -f docker-compose.e2e.yml down --volumes --remove-orphans 2>&1; docker network prune -f 2>&1`

### Phase 1: Happy Path

1. Generate a unique room name and run in detached mode, then wait for peer-b to exit:
   ```bash
   ROOM_NAME="e2e-$(date +%s)" docker compose -f docker-compose.e2e.yml up --build -d 2>&1
   timeout ${VALIDATE_TIMEOUT:-120} docker wait e2e-cross-network-test-peer-b-1 2>&1
   EXIT_CODE=$?
   docker compose -f docker-compose.e2e.yml logs peer-b 2>&1
   ```

2. Parse peer-b's stdout for JSON validation results (look for the JSON block starting with `{`).

3. Report:
   - If exit code 0 and JSON shows `"pass": true`: report **PASSED** with per-interval summary
   - If exit code non-zero or `"pass": false`: report **FAILED** with details on which bars/intervals failed and why

4. Clean up:
   `docker compose -f docker-compose.e2e.yml down --volumes`

### Phase 2: Chaos Testing (only if Phase 1 passes)

1. Run again with chaos scripts on peer-a. The chaos script `stable:4,leave:5s,rejoin,stable:4,transport-stop:5s,resume,stable:4` yields ~7 validated intervals (some are lost during transitions). Set VALIDATE_INTERVALS=7:
   ```bash
   ROOM_NAME="e2e-chaos-$(date +%s)" PEER_A_CHAOS="stable:4,leave:5s,rejoin,stable:4,transport-stop:5s,resume,stable:4" VALIDATE_INTERVALS=7 VALIDATE_TIMEOUT=180 docker compose -f docker-compose.e2e.yml up --build -d 2>&1
   timeout 180 docker wait e2e-cross-network-test-peer-b-1 2>&1
   EXIT_CODE=$?
   docker compose -f docker-compose.e2e.yml logs peer-b 2>&1
   ```

2. Parse results, report which chaos phases passed/failed. Also check peer-a logs to confirm all chaos actions ran:
   ```bash
   docker compose -f docker-compose.e2e.yml logs peer-a 2>&1 | grep -E '\[chaos\]'
   ```

3. Clean up:
   `docker compose -f docker-compose.e2e.yml down --volumes`
