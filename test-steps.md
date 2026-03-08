# E2E Two-Machine Test — Local LAN

This test validates the full WAIL network path (signaling → WebRTC → sync → audio → reconnection) between two machines on the same LAN, using a local signaling server with the reconnection fix.

## Prerequisites

- Both machines on the same LAN
- Both machines have this repo checked out on `quasor/e2e-two-machine-tests`
- Rust 1.75+, CMake, C++ compiler on both machines
- Go 1.22+ on the **leader** machine (runs the signaling server)

## Machine Roles

- **Leader** (this machine): Runs the local signaling server + the e2e test binary
- **Follower** (other machine): Runs only the e2e test binary, pointed at the leader's server

---

## Leader Instructions

### Step 1: Find your LAN IP

```sh
# macOS
ipconfig getifaddr en0
# Linux
hostname -I | awk '{print $1}'
```

Note your IP (e.g. `192.168.1.100`). The follower needs this.

### Step 2: Start the local signaling server

```sh
cd signaling-server
DB_PATH=/tmp/wail-e2e-test.db go run main.go
```

This starts the server on port 8080. Leave this running in a separate terminal.

### Step 3: Build and run the e2e test

In a new terminal:

```sh
cargo run -p wail-e2e --release -- \
  --server ws://<YOUR_LAN_IP>:8080 \
  --verbose 2>&1 | tee e2e-leader.log
```

The test will print a room name and wait for the follower to join. Tell the follower the room name and your LAN IP.

---

## Follower Instructions

You are the remote peer in a two-machine e2e test. The leader will give you:
1. Their LAN IP address (e.g. `192.168.1.100`)
2. A room name (e.g. `e2e-a1b2c3d4`)

### Step 1: Sync the branch

```sh
git checkout quasor/e2e-two-machine-tests
git pull
```

### Step 2: Run the e2e test

```sh
cargo run -p wail-e2e --release -- \
  --server ws://<LEADER_LAN_IP>:8080 \
  --room <ROOM_NAME> \
  --verbose 2>&1 | tee e2e-follower.log
```

Replace `<LEADER_LAN_IP>` and `<ROOM_NAME>` with the values from the leader.

---

## What to Expect

Eight phases run sequentially on both machines:

| Phase | What it does |
|-------|-------------|
| 1. ICE | Fetches TURN servers (falls back to STUN if Metered unreachable) |
| 2. Signaling | Connects to the local signaling server via WebSocket |
| 3. Discovery | Waits for the other peer to join the same room |
| 4. WebRTC | Full ICE/DTLS/SCTP negotiation, DataChannels open |
| 5. Sync | Exchanges Hello + Ping/Pong, measures RTT |
| 6. Audio | Sends one 440Hz Opus-encoded test interval, validates receipt |
| 7. Sustained | Sends 10 intervals back-to-back, measures throughput and gaps |
| 8. Reconnect | One peer disconnects WebRTC and reconnects signaling, verifies recovery |

All 8 phases should show `[PASS]`. If any fail, the logs (`e2e-leader.log` / `e2e-follower.log`) have full debug output.

## Troubleshooting

- **Signaling timeout**: Verify the server is running (`curl http://<LEADER_IP>:8080/health` should return `ok`)
- **WebRTC timeout on LAN**: Should not happen — LAN peers connect via host candidates without TURN. If it does, check firewall rules (UDP must be open between the machines).
- **Reconnection fails**: Check both logs around the "Reconnection test" output. The lower peer_id is the reconnector, the higher is the waiter.
