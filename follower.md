# E2E Test — Follower Machine Notes

## Status

Waiting for the leader to post their LAN IP and room name.

## What I've synced

Branch: `quasor/e2e-two-machine-tests` (at `ae2e12c`)

Key commits merged:
- `d87e7b6` — initial wail-e2e crate (phases 1–6: ICE, Signaling, Discovery, WebRTC, Sync, Audio)
- `ab5e6d8` — added phases 7 (Sustained audio) and 8 (Reconnection)
- `3b10feb` — reconnection now coordinated by peer ID ordering (lower = initiator, higher = waiter)
- `98d6b4e` — suppress leave message during signaling reconnect (server + client fix)
- `ae2e12c` — handle signaling reconnection race in server and e2e test

## My role

As follower (waiter or initiator depending on peer ID):
- I will connect to the leader's local signaling server (`ws://<LEADER_IP>:8080`)
- I must use the same room name the leader prints at startup
- All 8 phases run on both machines simultaneously

## Command to run (once I have leader info)

```sh
cargo run -p wail-e2e --release -- \
  --server ws://<LEADER_LAN_IP>:8080 \
  --room <ROOM_NAME> \
  --verbose 2>&1 | tee e2e-follower.log
```

## Phase 8 (Reconnection) — my role depends on peer ID comparison

- If my peer ID < leader's peer ID → I am the **initiator** (disconnect + reconnect)
- If my peer ID > leader's peer ID → I am the **waiter** (stay alive, watch for disconnect, wait for rejoin)

Peer IDs are random UUIDs prefixed `e2e-`, assigned at runtime, so the role assignment is non-deterministic.

## Test Run Results

Leader IP: `192.168.7.141`, room: `e2e-34dc7c33`

**Phase 1 (ICE): PASS** — 5 Metered servers fetched
**Phase 2 (Signaling): FAIL** — `IO error: No route to host (os error 65)` in ~350µs

### Diagnosis

- `ping 192.168.7.141` works (31–117ms RTT, same LAN)
- `curl http://192.168.7.141:8080/health` returns `ok`
- `nc -zv 192.168.7.141 8080` connects immediately
- But tokio-tungstenite WebSocket fails in ~350µs (before hitting the network)

The error fires faster than a LAN round trip, indicating the OS is rejecting the socket call locally. This machine has **two interfaces on the same subnet** (en0: 192.168.7.186, en1: 192.168.7.107), and the routing table shows 192.168.7.141 reachable via both. tokio's async TCP socket appears to be hitting `EHOSTUNREACH` (errno 65) due to the duplicate route/interface conflict on macOS — curl and netcat succeed because they use synchronous/blocking I/O which handles this differently.
