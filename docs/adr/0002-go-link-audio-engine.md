# The audio engine is Go, binding Link Audio directly

Following ADR-0001 (Link Audio is WAIL's only audio interface), the engine — Link sync, Link Audio capture/playback, the interval pipeline, Opus, and the relay client — lives entirely in **Go**, inside the existing `wail-app`. There are no plugins and no IPC: audio enters and leaves the process through Link Audio, and the WAN side (relay, WAIF wire format, sync protocol, rooms, reconnect) is unchanged.

## Considered options

- **Rust core engine, Go/Wails GUI shell** — GC never touches the audio path, and it reuses the tested Rust interval DSP. Rejected: it means porting the session orchestration to Rust and a Go↔Rust FFI boundary, and the accepted risk (GC) is designed around cheaply (below).
- **Go engine (chosen)** — one language, keeps the existing Go orchestration, relay, and Wails GUI. Accepts GC on the audio path as a risk to engineer around rather than eliminate.

## Decisions

- **One unified cgo binding to `abl_link` (sync + audio) compiled against `vendor/link`** (now 4.0). This retires `abletonlink-go` and the DatanoiseTV-fork clone/patch dance in CI and the Homebrew formula; `vendor/link` becomes the SDK we actually compile, and the MinGW `Channels.hpp` fix moves into our own C. One `abl_link` handle per process (Link is one LAN peer) — sync and audio must share it, which is why the binding is unified.
- **Pure per-channel pass-through, no mixing.** Capture: the user explicitly ticks which discovered local Link Audio channels to bridge (a send-mixer); each becomes its own Opus/WAIF stream. Emit: each remote (peer, stream) is republished as its own Link Audio channel. No summing, no crossfade-on-mix, no slot table, no 15-slot cap — all of which retire with the plugins.
- **Single WAIL Link peer**, not one proxy peer per remote musician. Remote streams are channels under the one WAIL peer, named `"{peer} · {stream}"`, keyed on `(persistent identity, stream index)` so they survive reconnects. Link Audio channel ownership is tied to the publishing peer, so per-musician *peer* attribution would require N Link instances per process — rejected because two machines on one LAN both running WAIL would each spray N phantom Link peers onto that LAN, which is confusing and is non-standard Link usage. Named channels give enough attribution.
- **Minimal-C real-time boundary.** The Link Audio capture callback is a **pure C** function (a C function pointer handed to `abl_link`, never a Go `//export`) that `memcpy`s into a C-owned preallocated ring and returns; a Go goroutine drains it off-thread. Emit tops up the sink's deep internal queue from Go (deep-queue, not a C-side pacing thread).
- **Big-bang migration.** Plugins, IPC, `plugin_install.go`, plugin xtask/CI/packaging, and the **entire Rust workspace** are removed on one branch; the test client is reimplemented in Go against the app's own WAIF/relay code. The internal build order is: get the binding linking → capture → emit → delete. No working intermediate.

## The pure-C-callback invariant

The capture callback **must remain pure C and never enter the Go runtime.** Go's GC stop-the-world pauses Go-managed threads but not a C thread executing C; a pure-C callback therefore runs through a GC pause, `memcpy`s into the C ring, and returns, and the Go drainer catches up afterward — so **GC-induced capture loss is structurally eliminated** (given the ring is sized for the worst-case pause, which is cheap). If the callback were instead a Go function, a GC pause would block Link's audio thread as it tried to enter Go, backpressure the SDK's socket reader, and the kernel would drop incoming UDP (Link Audio has no retransmission) — real, permanent capture loss. This is why the binding is ours (Option A) rather than one exposing a Go-level callback. A future "simplify the cgo layer" refactor must not turn this into a Go callback.

## Consequences

- GC is mitigated, not eliminated, and asymmetrically: **capture loss is structurally impossible** (pure-C callback + C ring); **emit is deep-queue-mitigated** (the SDK sender thread is GC-immune; only the Go producer can pause, bounded by the sink queue's hundreds-of-ms depth, and a rare underrun is a far-LAN playout blip, not loss of delivered data). The irreversible risk is the one made impossible.
- WAIL interoperates only with Link-Audio-capable apps.
- Capture is best-effort on the LAN (Link Audio UDP, no retransmission); the loss-free guarantee is the WAN leg only (see CONTEXT.md pillar 3 and ADR-0003).
- Timing/sync decisions live in ADR-0003.
