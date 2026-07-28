# WAIL — WAN Audio Interchange for Link

WAIL synchronizes [Ableton Link](https://www.ableton.com/link/) sessions across the internet using a WebSocket relay server. Musicians on different networks can sync tempo, phase, and interval boundaries as if they were on the same LAN, with intervalic audio (NINJAM-style) captured, Opus-encoded, and transmitted via the server.

WAIL is an Ableton **Link Audio** peer — it captures and plays audio directly over Link, so with a Link-Audio-capable DAW there are no WAIL plugins to install. Ableton Live 12.3+ supports Link Audio natively. For any other CLAP-capable DAW, load the first-party **WAIL Link Bridge** CLAP plugins (bundled with the app — see below), which bring Link Audio into hosts that don't speak it yet.

## Install

Download the latest release from the [Releases page](https://github.com/MostDistant/WAIL/releases).

**macOS (Homebrew, from source)** — Build and install directly from source:

```sh
brew tap MostDistant/wail
brew install MostDistant/wail/wail
```

This builds and installs the WAIL binary (and the CLAP plugins) from source. With a Link-Audio DAW you need no plugins; for a DAW without Link Audio, run `wail-install-plugins` afterward to copy the WAIL CLAP plugins into your CLAP folder, then rescan in your DAW.

**Windows** — Download `wail-windows-x64-<version>.zip` from the Releases page, extract it, and run `bin\wail.exe`. The binary is unsigned, so SmartScreen will warn on first launch — click "More info" → "Run anyway".

**Linux** — Download `wail-linux-x64-<version>.tar.gz` from the Releases page. Install the runtime dependencies, extract, and run:

```sh
sudo apt install libwebkit2gtk-4.1-0 libopus0   # Debian/Ubuntu runtime deps
tar -xzf wail-linux-x64-*.tar.gz
./wail-*/bin/wail
```

### DAW plugins (only for DAWs without Link Audio)

For DAWs that don't support Ableton Link Audio, load the **WAIL Link Bridge Send** and **WAIL Link Bridge Recv** CLAP plugins. Each is a Link Audio peer in its own right: put Link Bridge Send on a track to publish it to the LAN, and Link Bridge Recv on a track to hear the room's streams on its output ports. Ableton Live 12.3+ users don't need either.

Two requirements: your DAW must load **CLAP** plugins (Bitwig, REAPER, Studio One, Qtractor; not Logic or Pro Tools), and the project must run at **48 kHz** — Link Audio is 48 kHz only, so at any other rate Send publishes nothing and Recv outputs silence.

On the Windows and Linux release builds the plugins auto-install on first launch into your per-user CLAP folder (`%LOCALAPPDATA%\Programs\Common\CLAP` / `~/.clap`); if that's blocked, copy the `.clap` bundles from the release's `lib/` folder there yourself and rescan. On Homebrew, run `wail-install-plugins`.

## Getting Started

1. **Launch the WAIL app.**

2. **Enable Ableton Link (tempo/phase) and Link Audio (the audio exchange) in your DAW.** These are two separate things: Link sync is widely supported; Link Audio is newer.
   - *Ableton Live 12.3+* — the only DAW with **native Link Audio** today. Preferences > Link, Tempo, MIDI > turn on "Show Link Toggle", then enable Link in the transport bar.
   - *Any other DAW (Bitwig, REAPER, etc.)* — enable Ableton **Link** for tempo/phase sync (Bitwig: Settings > Synchronization; REAPER: install [ReaBlink](https://github.com/ak5k/reablink)). For audio, load the first-party **WAIL Link Bridge Send / WAIL Link Bridge Recv** CLAP plugins (see Install) — Link Bridge Send publishes a track as a Link Audio channel WAIL captures, Link Bridge Recv plays the room's streams on its output ports.

3. **Route audio to Link Audio.** Send the tracks or busses you want to share to Link Audio output channels in your DAW. WAIL captures those channels, so anything you route there is streamed to your peers. You can share several independent streams (e.g. drums on one channel, synth on another).

4. **Bring in remote peers.** WAIL republishes each remote peer's audio as a Link Audio channel. Add a track in your DAW whose input is a Link Audio channel to hear them — one channel per peer/stream.

5. **Join a room** in the WAIL app. On first launch, you'll be prompted to enter a display name (you can change it later via the settings gear icon). Enter a room name and optionally set a password to create a private room, or leave it blank for a public room. You can also browse existing public rooms from the "Public Rooms" tab.

6. **Play.** Audio is recorded for the duration of each interval (default: 16 beats — 4 bars), then transmitted to all connected peers. Playback runs one interval behind — this latency-by-design is how NINJAM-style sync works.

## Beats per interval (BPI)

Two numbers control the timing of every jam, NINJAM-style:

- **BPM** (beats per minute) — the tempo, set in your DAW and shared via Ableton Link; WAIL shows it read-only
- **BPI** (beats per interval) — how many beats fit in one interval

Together they set the interval length: (BPI ÷ BPM) × 60 = seconds. At 120 BPM and 16 BPI, each interval lasts 8 seconds.

The first peer in a room sets its BPI; everyone who joins adopts it. Anyone can change it mid-jam from the session screen — the change applies at the next interval boundary. BPI must divide evenly into whole bars at the room's beats per bar (default 4), e.g. 4, 8, 16, or 32.

**Match your DAW's launch quantization to the room interval.** WAIL can't read or set your DAW's launch quantization (Ableton Link doesn't carry it), so it tells you instead: when you join a room, WAIL shows an in-session callout with the room's interval and reminds you to set your DAW to match (e.g. Live's Global Quantization → 4 Bars), enable Ableton Link, then start your transport — the callout clears once a Link peer appears. This keeps everyone's clip launches aligned with the interval grid. Join the room first, then start your DAW's transport — that gives you a clean first interval (WAIL still handles a mid-interval start, so it's a nicety, not a hard requirement).

The session header shows a small **interval clock** — a pie that starts full at each interval boundary and sweeps clockwise as the interval counts down, so you can see at a glance when the next one flips.

Next to it, a **grid alignment badge** shows how closely your local interval grid matches the room's (ADR-0006). When you join, WAIL aligns your Link session to the room grid — a one-time snap if you're materially off, invisible if nothing is playing — and then gently keeps it there (a badge reading `grid ✓` means you're within ~10 ms; `aligning` means WAIL is nudging; `drifted` means the error is past the audible threshold and being corrected). This is what keeps every peer's interval *content* landing on the same downbeats, not just the interval numbering.

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
- **Link Audio Name** — The peer name WAIL advertises on Link Audio, so its published channels are easy to spot in your DAW's peer list. Defaults to `WAIL`.
- **Save debug log locally** — Writes structured logs to a rotating file in the app data directory. Useful for diagnosing connection issues.
- **Peer log sharing** — Always on: in a session, your app's INFO-level logs are broadcast to the room via the relay, and peers' logs appear in your Debug tab's log panel with a peer name prefix — one client collates the whole room's logs, which is how rooms debug themselves. (The `wail-logtail` CLI can also tail a room's logs without joining as an audio peer.)
- **Remember settings** — Persists room name, password, and display name in localStorage, plus your enabled capture channels (by app and channel name) so they re-enable automatically in future sessions.

## Troubleshooting

**No sync / peers not connecting** — Make sure Ableton Link is enabled in your DAW. WAIL relies on Link for tempo and phase sync.

**No audio from remote peers** — Make sure the WAIL app is running and connected to the same room, and that your project is at 48 kHz. With native Link Audio (Ableton Live 12.3+), check you've routed audio to a Link Audio output channel and added a track taking its input from WAIL's published Link Audio channels. With the WAIL Link Bridge plugins, the plugins are that routing — check Link Bridge Send is on the track you want to share and Link Bridge Recv is on a track you can hear.

**Changing tempo mid-jam** — Not recommended. WAIL uses NINJAM-style intervals, so audio is recorded and played back in full interval chunks. If you change the tempo, the current interval must finish before the new tempo takes effect. If you do need to change tempo, agree on it beforehand and have one person change it **in their DAW** (WAIL shows BPM read-only and just follows Link) — Link will propagate it to all peers within a few seconds.

**Debugging what you're sending** — The *Debug* tab is hidden by default; turn on **Developer mode** in Settings to reveal it. Two live toggles there: **Dump capture to WAV** writes each enabled channel's audio to pre-Opus and post-Opus WAV files under `~/.wail/dumps` (A/B them to localize where audio degrades). **Loopback my audio via server** asks the relay to echo your own audio back; it reappears one interval late as a `(loopback)` Link Audio channel — subscribe to it in your DAW to hear exactly what remote peers hear, including the full encode → relay → decode round trip.

**Checking playback alignment** — The Debug tab's *Playback* section has four more controls. **Publish WAIL Metronome channel** exposes a `WAIL · <you> · Metronome` Link Audio channel — a click on every beat, accented on bar downbeats, on WAIL's own beat grid; subscribe to it in your DAW and run your DAW's metronome to confirm the two line up (a flam means the grids disagree, and a glitchy click means the playback path is struggling). **Broadcast metronome to room** additionally streams that same click to everyone in the room as audio: each peer auto-publishes a `WAIL · <you> · Metronome` Link Audio channel and hears the identical grid one interval late. It's independent of the local channel above — run either or both. **Emit cushion** is a slider (100–500 ms, default 100) for how far ahead of the playhead WAIL keeps each Link Audio sink fed — below 100 the emit loop has no jitter tolerance and underruns audibly; if your receiver runs dry (sink underruns climbing in the health counters below), raise it until the dropouts stop. **Interval offset D** is a slider (0–4 intervals, default 1) for the NINJAM delay — how many intervals remote audio is held before it plays; raise it on lossy WAN paths for more slack, lower it for tighter turn-taking. The Debug tab also carries audio stats, per-peer relay RTT, local audio-health counters, and the session log.

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
