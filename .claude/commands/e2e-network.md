---
description: Run cross-network e2e test in Docker (isolated networks + WAN simulation)
allowed-tools: [Bash]
---

# Cross-Network E2E Test

Run the Docker-based cross-network end-to-end test: two peers on separate Docker networks communicate via the signaling server with simulated WAN conditions (latency, jitter, packet loss).

Three phases: happy path validation, chaos testing (disconnect/rejoin + transport stop/start), and 3-peer chaos isolation (verify stable peers are unaffected by a chaotic third peer).

## Arguments

The user may pass environment variable overrides, e.g. `/e2e-network WAN_DELAY=100ms WAN_LOSS=5%`.

Supported variables:
- `WAN_DELAY` (default 50ms), `WAN_JITTER` (default 10ms), `WAN_LOSS` (default 1%)
- `PEER_A_NOTES`, `PEER_B_NOTES`, `PEER_C_NOTES` — custom note scripts for each peer
- `PEER_A_CHAOS`, `PEER_B_CHAOS`, `PEER_C_CHAOS` — chaos scripts for each peer
- `VALIDATE_PEER` — display name of the peer to validate (for 3-peer tests, e.g. `peer-a`)
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

### Phase 3: 3-Peer Chaos Isolation (only if Phase 1 passes)

Verify that a chaotic third peer (disconnect/reconnect) does NOT disrupt A↔B audio streams.

peer-a sends 440Hz (stable), peer-b validates only peer-a's audio, peer-c sends 880Hz and runs chaos.

1. Run with peer-c doing chaos (two leave/rejoin cycles), peer-b filtering validation to peer-a only:
   ```bash
   ROOM_NAME="e2e-3peer-$(date +%s)" PEER_A_NOTES="440:4" PEER_C_NOTES="880:4" PEER_C_CHAOS="stable:1,leave:3s,rejoin,stable:1,leave:3s,rejoin,stable:20" VALIDATE_PEER="peer-a" VALIDATE_INTERVALS=4 VALIDATE_TIMEOUT=180 docker compose -f docker-compose.e2e.yml --profile three-peer up --build -d 2>&1
   timeout 180 docker wait e2e-cross-network-test-peer-b-1 2>&1
   EXIT_CODE=$?
   docker compose -f docker-compose.e2e.yml --profile three-peer logs peer-b 2>&1
   ```

2. Parse peer-b results — all 4 intervals should PASS for peer-a's 440Hz audio. Also check peer-c chaos actions ran:
   ```bash
   docker compose -f docker-compose.e2e.yml --profile three-peer logs peer-c 2>&1 | grep -E '\[chaos\]'
   ```

3. Report:
   - If peer-b shows all intervals PASS AND peer-c chaos log shows leave/rejoin actions: **PASSED** — stable peers unaffected by chaotic peer
   - If peer-c chaos actions didn't run: **INCONCLUSIVE** — chaos may not have overlapped with validation
   - If any interval FAIL: **FAILED** — peer-c's chaos disrupted A↔B stream

4. Clean up:
   `docker compose -f docker-compose.e2e.yml --profile three-peer down --volumes`
