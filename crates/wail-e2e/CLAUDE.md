# WAIL E2E Test — Remote Machine Instructions

This binary validates the full WAIL network path between two real machines on separate networks.

## Quick Start

You are the **remote peer** in a two-machine end-to-end test. The other machine will give you a room name.

```sh
git checkout quasor/e2e-two-machine-tests
git pull
cargo run -p wail-e2e --release -- --room <ROOM_NAME>
```

Replace `<ROOM_NAME>` with the room name provided by the other machine.

## What It Tests

Six phases run sequentially. Each must pass before the next begins:

1. **ICE** — Fetches TURN credentials from Metered API (falls back to STUN-only if unreachable)
2. **Signaling** — Connects to `wss://wail-signal.fly.dev` via WebSocket
3. **Discovery** — Waits for the other peer to join the same room
4. **WebRTC** — Full ICE/DTLS/SCTP negotiation, waits for DataChannels to open
5. **Sync** — Exchanges Hello messages + Ping/Pong to measure RTT
6. **Audio** — Sends a 440Hz Opus-encoded test interval, validates received audio (wire format, Opus decode, non-silence check)

## Options

```
--room <NAME>       Room name (REQUIRED — must match the other machine)
--server <URL>      Signaling server [default: wss://wail-signal.fly.dev]
--timeout <SECS>    Global timeout [default: 120]
--verbose           Debug-level tracing output
```

## Troubleshooting

- **WebRTC timeout**: Both machines may be behind symmetric NATs. Add `--verbose` to see ICE candidate types. If only `host` candidates appear, TURN relay is needed (the binary fetches TURN credentials automatically, but corporate firewalls may block TURN ports).
- **Signaling timeout**: Check that `wss://wail-signal.fly.dev` is reachable. Try `curl -I https://wail-signal.fly.dev` — you should get a response.
- **No audio received**: The other machine must also reach the Audio phase. If one side is stuck on WebRTC, neither will exchange audio.
- **Build fails**: Requires Rust 1.75+, CMake 3.14+, C++ compiler, libopus-dev. Run `git submodule update --init --recursive` first.
