# WAIL — WAN Audio Interchange for Link

WAIL synchronizes [Ableton Link](https://www.ableton.com/link/) sessions across the internet using a WebSocket relay server. Musicians on different networks can sync tempo, phase, and interval boundaries as if they were on the same LAN, with intervalic audio (NINJAM-style) captured, Opus-encoded, and transmitted via the server.

WAIL is an Ableton **Link Audio** peer — it captures and plays audio directly over Link, so there are no WAIL plugins to install. Ableton Live 12.3+ supports Link Audio natively; with any other DAW you can bridge audio in and out of Link Audio using the third-party [VoidLinkAudio VST](https://structurevoid.gumroad.com/l/voidlinkaudio-vst).

## Install

Download the latest release from the [Releases page](https://github.com/MostDistant/WAIL/releases).

**macOS (Homebrew, from source)** — Build and install directly from source:

```sh
brew tap MostDistant/wail
brew install MostDistant/wail/wail
```

This builds and installs the WAIL binary from source. WAIL captures and plays audio as an Ableton Link Audio peer, so there are no plugins to install.

**Windows** — Download `wail-windows-x64-<version>.zip` from the Releases page, extract it, and run `bin\wail.exe`. The binary is unsigned, so SmartScreen will warn on first launch — click "More info" → "Run anyway".

**Linux** — Download `wail-linux-x64-<version>.tar.gz` from the Releases page. Install the runtime dependencies, extract, and run:

```sh
sudo apt install libwebkit2gtk-4.1-0 libopus0   # Debian/Ubuntu runtime deps
tar -xzf wail-linux-x64-*.tar.gz
./wail-*/bin/wail
```

## Getting Started

1. **Launch the WAIL app.**

2. **Enable Ableton Link (tempo/phase) and Link Audio (the audio exchange) in your DAW.** These are two separate things: Link sync is widely supported; Link Audio is newer.
   - *Ableton Live 12.3+* — the only DAW with **native Link Audio** today. Preferences > Link, Tempo, MIDI > turn on "Show Link Toggle", then enable Link in the transport bar.
   - *Any other DAW (Bitwig, REAPER, etc.)* — enable Ableton **Link** for tempo/phase sync (Bitwig: Settings > Synchronization; REAPER: install [ReaBlink](https://github.com/ak5k/reablink)), and to send/receive **Link Audio** use the third-party [VoidLinkAudio VST](https://structurevoid.gumroad.com/l/voidlinkaudio-vst), which bridges your DAW's audio to and from Link Audio channels that WAIL can capture and publish.

3. **Route audio to Link Audio.** Send the tracks or busses you want to share to Link Audio output channels in your DAW. WAIL captures those channels, so anything you route there is streamed to your peers. You can share several independent streams (e.g. drums on one channel, synth on another).

4. **Bring in remote peers.** WAIL republishes each remote peer's audio as a Link Audio channel. Add a track in your DAW whose input is a Link Audio channel to hear them — one channel per peer/stream.

5. **Join a room** in the WAIL app. On first launch, you'll be prompted to enter a display name (you can change it later via the settings gear icon). Enter a room name and optionally set a password to create a private room, or leave it blank for a public room. You can also browse existing public rooms from the "Public Rooms" tab.

6. **Play.** Audio is recorded for the duration of each interval (default: 16 beats — 4 bars), then transmitted to all connected peers. Playback runs one interval behind — this latency-by-design is how NINJAM-style sync works.

## Beats per interval (BPI)

Two numbers control the timing of every jam, NINJAM-style:

- **BPM** (beats per minute) — the tempo, shared via Ableton Link
- **BPI** (beats per interval) — how many beats fit in one interval

Together they set the interval length: (BPI ÷ BPM) × 60 = seconds. At 120 BPM and 16 BPI, each interval lasts 8 seconds.

The first peer in a room sets its BPI; everyone who joins adopts it. Anyone can change it mid-jam from the session screen — the change applies at the next interval boundary. BPI must divide evenly into whole bars at the room's beats per bar (default 4), e.g. 4, 8, 16, or 32.

**Match your DAW's launch quantization to the room interval.** WAIL can't read or set your DAW's launch quantization (Ableton Link doesn't carry it), so it tells you instead: when you join a room, WAIL shows the room's interval and asks you to set your DAW to match (e.g. Live's Global Quantization → 4 Bars). This keeps everyone's clip launches aligned with the interval grid.

## Headless CLI Mode

WAIL can run without the GUI for scripted or automated use. The `-headless` flag starts the app in CLI mode, and `-wav` streams a WAV file to peers in the room, looping continuously until stopped.

```sh
./wail-app -headless -room=myroom -wav=song.wav -bpm=120 -name="wav-bot"
```

| Flag | Description |
|------|-------------|
| `-headless` | Run without GUI (required for CLI mode) |
| `-room` | Room to join (required in headless mode) |
| `-wav` | WAV file to send (loaded into memory, resampled to 48kHz stereo) |
| `-bpm` | Tempo in BPM (default: 120) |
| `-name` | Display name (auto-generated if empty) |
| `-password` | Room password (optional) |
| `-loopback` | Relay echoes our own audio back; republished as a `(loopback)` Link Audio channel |

Stop with Ctrl+C or SIGTERM for clean shutdown.

By default WAIL connects to the hosted relay. Set the `WAIL_SIGNAL_URL` environment variable (e.g. `WAIL_SIGNAL_URL=ws://localhost:8899`) to point at a self-hosted or local relay instead.

## Components

WAIL has two components that work together:

- **WAIL app** — The desktop app you run alongside your DAW. It joins your local Ableton Link session, captures the Link Audio channels you route to it, Opus-encodes them per interval, and relays them to your peers. Incoming audio is decoded, held one interval, and republished as Link Audio channels for your DAW to play back.

- **Signaling server** — A lightweight WebSocket relay that connects rooms of peers, forwarding sync messages and audio between everyone in the room.

## Settings

- **Display name** — Shown to other peers in the session.
- **Save debug log locally** — Writes structured logs to a rotating file in the app data directory. Useful for diagnosing connection issues.
- **Peer log streaming** — When enabled, your app's INFO-level logs are broadcast to all other peers in the session via the signaling server, and their logs are shown in your session log panel with a peer name prefix. Useful for collaborative debugging. Both sending and receiving are controlled by this single toggle.
- **Remember settings** — Persists room name, password, and display name in localStorage.

## Troubleshooting

**No sync / peers not connecting** — Make sure Ableton Link is enabled in your DAW. WAIL relies on Link for tempo and phase sync.

**No audio from remote peers** — Make sure Link Audio is available in your DAW (native in Ableton Live 12.3+, or via the [VoidLinkAudio VST](https://structurevoid.gumroad.com/l/voidlinkaudio-vst) in other DAWs), that you've routed audio to a Link Audio output channel and added a track that takes its input from WAIL's published Link Audio channels, and that the WAIL app is running and connected to the same room.

**Changing tempo mid-jam** — Not recommended. WAIL uses NINJAM-style intervals, so audio is recorded and played back in full interval chunks. If you change the tempo, the current interval must finish before the new tempo takes effect. If you do need to change tempo, agree on it beforehand and have one person change it — Link will propagate it to all peers within a few seconds.

**Debugging what you're sending** — Two live toggles under *Capture channels*: **Dump capture to WAV** writes each enabled channel's audio to pre-Opus and post-Opus WAV files under `~/.wail/dumps` (A/B them to localize where audio degrades). **Loopback my audio via server** asks the relay to echo your own audio back; it reappears one interval late as a `(loopback)` Link Audio channel — subscribe to it in your DAW to hear exactly what remote peers hear, including the full encode → relay → decode round trip.

## Development

See [DEVELOPMENT.md](DEVELOPMENT.md) for build instructions, project structure, and testing.

## Thanks

WAIL's intervalic audio model is directly inspired by [NINJAM](https://www.ninjam.com/), created by Justin Frankel at [Cockos](https://www.cockos.com/). The idea that you can jam with anyone in the world by accepting one interval of latency changed everything.

Built on the shoulders of great open-source projects:
[Ableton Link](https://www.ableton.com/link/) (tempo/phase sync and Link Audio),
[Opus](https://opus-codec.org/) (audio codec),
[Wails](https://wails.io/) (desktop app framework).

Thanks to early supporters [Jeff Hopkins](https://www.youtube.com/@JeffHopkinsMusic) and [Geren M](https://www.youtube.com/@GerenM63) for testing, feedback, and encouragement.

## License

MIT
