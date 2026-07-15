# Two-Machine Link Audio Test — Local LAN + WAN

This is the manual hardware validation for the Link Audio engine: it exercises the
full path (signaling → sync → capture → relay → playback) between two machines,
which cannot be tested in CI (it needs real Ableton Link peers and, for the full
audio path, a Link-Audio-capable app such as Ableton Live 12.3+).

## Prerequisites

- Two machines that can reach a signaling server (same LAN, or both to a hosted relay).
- Each with the repo checked out, Go 1.26+, a C++ toolchain, libopus + pkg-config,
  and the `vendor/link` submodule initialized (`bin/setup`).
- For the full audio path: a Link-Audio-capable app (e.g. Ableton Live 12.3+) on
  each machine. For a quick smoke test with no DAW, use the built-in test tone.

## Roles

- **Leader**: runs the signaling server + a WAIL app.
- **Follower**: runs a WAIL app pointed at the leader's server.

---

## Leader

### 1. LAN IP
```sh
ipconfig getifaddr en0        # macOS
hostname -I | awk '{print $1}'  # Linux
```

### 2. Signaling server
```sh
cd signaling-server
DB_PATH=/tmp/wail-e2e.db go run .
```
Serves on `:8080`. Leave it running.

### 3. WAIL app

The signaling server URL is the `signalingURL` const in `wail-app/app.go` (default
`wss://wail-signal.fly.dev`). To test against the local server, point it at the
leader: `const signalingURL = "ws://<LEADER_LAN_IP>:8080"` (both machines), then:
```sh
cd wail-app && go run .    # or: ../bin/dev
```
Join a room (note the room name for the follower). Interval offset D defaults to 1;
override with `WAIL_INTERVAL_OFFSET=N`. (For a WAN test against the hosted relay,
leave `signalingURL` at its default and skip the local server.)

---

## Follower

With the same `signalingURL` as the leader:
```sh
cd wail-app && go run .
```
Join the **same** room name.

---

## What to verify

1. **Discovery/sync** — both apps show each other as peers; tempo changes on one
   are reflected on the other (Link sync + relay).
2. **Relay interval clock** — both receive `interval_anchor`; the room interval
   index advances in step (check logs).
3. **Capture** — tick a local Link Audio channel in each app's capture send-mixer
   (or enable the test tone). Confirm the app logs "first WAIF frame sent" and the
   peer's `AudioRecv` count rises.
4. **Playback** — each app republishes the remote stream as a Link Audio channel
   named `"{peer} · {stream}"`; a Link-Audio app on the LAN can subscribe and hear
   the remote peer's **previous** interval (offset D), beat-aligned to the local grid.
5. **Degradation** — pull the network briefly on one side and restore it; the
   reconnecting peer's channels reappear (affinity), late audio live-appends, and
   the metrics dashboard (`/metrics/dashboard` on the server) shows LAN loss /
   interval-incomplete counters rather than a crash.

## Troubleshooting

- **No channels in the send-mixer** — the Link-Audio app must be publishing a
  channel on the LAN and WAIL must have Link Audio enabled (automatic on join).
- **Signaling timeout** — `curl http://<LEADER_IP>:8080/health` should return `ok`.
- **No audio** — confirm both machines agree on tempo/interval config and that the
  room interval index is advancing (anchor logs); check the metrics dashboard.
