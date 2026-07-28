package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// signalingURL is the relay endpoint. Override with WAIL_SIGNAL_URL (e.g.
// ws://localhost:8899) to point at a local or self-hosted relay — the Tier 2
// E2E harness (scripts/tier2-e2e.sh) uses this to run against a local server.
// wail-relay.fly.dev replaced wail-signal.fly.dev (fly apps can't be renamed);
// the old app was destroyed 2026-07-20 — clients older than 3.2.0 must upgrade.
var signalingURL = func() string {
	if u := os.Getenv("WAIL_SIGNAL_URL"); u != "" {
		return u
	}
	return "wss://wail-relay.fly.dev"
}()

// App is the Wails application backend. All exported methods are callable from the frontend.
type App struct {
	mu       sync.Mutex
	session  *SessionHandle
	emitter  EventEmitter
	identity string
	// streamNames is the live index-keyed override map the session works in;
	// streamNameStore is its channel-keyed persisted form. The session owns the
	// translation between them — only it sees the capture channel list.
	streamNames     map[uint16]string
	streamNameStore *StreamNameStore
	dataDir         string
	pluginErrors    []string // CLAP auto-install errors from startup, surfaced to the UI
	fileLog         *RotatingFileWriter
	wsLog           *WsLogWriter
	// rememberEnabled mirrors the frontend "Remember settings" checkbox (pushed
	// via SetRememberEnabled); it gates persisting captureEnabled to disk.
	rememberEnabled bool
	// captureEnabled is the remembered set of enabled capture channels, keyed
	// by (peer, channel) name; restored into each session's audio engine.
	captureEnabled []CaptureChannelKey
}

// NewApp creates a new App instance. Pass instance=0 for the default instance.
func NewApp(instance int) *App {
	dataDir := defaultDataDir()
	if instance > 0 {
		dataDir = fmt.Sprintf("%s-%d", dataDir, instance)
	}
	os.MkdirAll(dataDir, 0o755)
	identity := getOrCreateIdentity(dataDir)
	streamNameStore := LoadStreamNames(dataDir)

	// Auto-install the bundled CLAP plugins into the user's CLAP directory on first
	// launch (ADR-0007). They ship in the release archive, so this is a zero-friction
	// path for DAWs without Link Audio; any errors surface via GetPluginInstallErrors
	// so the UI can point at the manual-copy fallback.
	var pluginErrors []string
	if pluginDir := FindPluginDir(""); pluginDir != "" {
		pluginErrors = InstallPluginsIfMissing(pluginDir)
	}

	return &App{
		streamNames:     make(map[uint16]string),
		streamNameStore: streamNameStore,
		dataDir:         dataDir,
		identity:        identity,
		pluginErrors:    pluginErrors,
		rememberEnabled: true, // frontend default; corrected on settings load
		captureEnabled:  LoadCaptureEnabled(dataDir),
	}
}

// SetEmitter sets the event emitter (called during Wails setup).
func (a *App) SetEmitter(emitter EventEmitter) {
	a.emitter = emitter
}

// --- Frontend-callable methods (Wails bindings) ---

// GetAppVersion returns the app version string (kept in sync with Cargo.toml).
func (a *App) GetAppVersion() string {
	return appVersion
}

// GetPluginInstallErrors returns any CLAP plugin auto-install errors from startup,
// so the UI can surface them and point the user at the manual-install fallback.
func (a *App) GetPluginInstallErrors() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pluginErrors
}

type JoinResult struct {
	PeerID string  `json:"peer_id"`
	Room   string  `json:"room"`
	BPM    float64 `json:"bpm"`
}

type PublicRoomInfo struct {
	Room         string   `json:"room"`
	PeerCount    uint32   `json:"peer_count"`
	BPM          *float64 `json:"bpm,omitempty"`
	DisplayNames []string `json:"display_names"`
	CreatedAt    int64    `json:"created_at"`
}

// ListPublicRooms fetches public rooms from the signaling server.
func (a *App) ListPublicRooms() ([]PublicRoomInfo, error) {
	rooms, err := ListPublicRooms(signalingURL)
	if err != nil {
		return nil, err
	}
	result := make([]PublicRoomInfo, len(rooms))
	for i, r := range rooms {
		result[i] = PublicRoomInfo{
			Room: r.Room, PeerCount: r.PeerCount, BPM: r.BPM,
			DisplayNames: r.DisplayNames, CreatedAt: r.CreatedAt,
		}
	}
	return result, nil
}

// JoinRoom joins a room and starts a session.
func (a *App) JoinRoom(
	room string,
	password *string,
	displayName string,
	bpm *float64,
	bars *uint32,
	quantum *float64,
	recordingEnabled *bool,
	recordingDirectory *string,
	recordingStems *bool,
	recordingRetentionDays *uint32,
	streamCount *uint16,
	linkAudioName *string,
) (*JoinResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.session != nil {
		return nil, fmt.Errorf("already in a session — disconnect first")
	}

	actualBPM := 120.0
	if bpm != nil {
		actualBPM = *bpm
	}
	actualBars := uint32(4)
	if bars != nil {
		actualBars = *bars
	}
	actualQuantum := 4.0
	if quantum != nil {
		actualQuantum = *quantum
	}
	actualStreamCount := uint16(1)
	if streamCount != nil {
		actualStreamCount = *streamCount
	}
	// The Link Audio peer name is user-settable; empty/unset falls back to "WAIL".
	actualLinkAudioName := "WAIL"
	if linkAudioName != nil {
		if trimmed := strings.TrimSpace(*linkAudioName); trimmed != "" {
			actualLinkAudioName = trimmed
		}
	}

	var recording *RecordingConfig
	if recordingEnabled != nil && *recordingEnabled {
		dir := ""
		if recordingDirectory != nil {
			dir = *recordingDirectory
		} else {
			if d, err := DefaultRecordingDir(); err == nil {
				dir = d
			}
		}
		stems := false
		if recordingStems != nil {
			stems = *recordingStems
		}
		retention := uint32(30)
		if recordingRetentionDays != nil {
			retention = *recordingRetentionDays
		}
		recording = &RecordingConfig{
			Enabled: true, Directory: dir, Stems: stems, RetentionDays: retention,
		}
	}

	config := SessionConfig{
		Server:            signalingURL,
		Room:              room,
		Password:          password,
		DisplayName:       displayName,
		LinkAudioName:     actualLinkAudioName,
		Identity:          a.identity,
		BPM:               actualBPM,
		Bars:              actualBars,
		Quantum:           actualQuantum,
		Recording:         recording,
		StreamCount:       actualStreamCount,
		LogSource:         a.logSource(),
		CaptureRestore:    a.captureEnabled,
		StreamNameRestore: a.streamNameStore,
		OnCaptureEnabledChanged: func(keys []CaptureChannelKey) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.captureEnabled = keys
			if a.rememberEnabled {
				SaveCaptureEnabled(a.dataDir, keys)
			}
		},
		OnStreamNamesChanged: func(entries []StreamNameEntry, byIndex map[uint16]string) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.streamNames = byIndex
			SaveStreamNames(a.dataDir, entries)
		},
	}

	handle, err := SpawnSession(a.emitter, config)
	if err != nil {
		return nil, err
	}
	a.session = handle

	return &JoinResult{PeerID: handle.PeerID, Room: handle.Room, BPM: actualBPM}, nil
}

// Disconnect ends the current session and waits for cleanup to finish.
func (a *App) Disconnect() error {
	a.mu.Lock()
	session := a.session
	a.session = nil
	a.mu.Unlock()

	if session == nil {
		return nil
	}

	session.CmdCh <- SessionCommand{Type: "Disconnect"}
	session.cancel()

	select {
	case <-session.done:
		log.Println("[app] Session goroutine finished cleanly")
	case <-time.After(5 * time.Second):
		log.Println("[app] Session goroutine did not finish in 5s, proceeding")
	}

	log.Println("[app] Disconnect complete")
	return nil
}

// Shutdown disconnects any active session and disables frontend event emission.
// Called after the Wails app exits to ensure clean teardown.
func (a *App) Shutdown() {
	if we, ok := a.emitter.(*WailsEmitter); ok {
		we.Shutdown()
	}
	a.Disconnect()
}

// ChangeBPM sends a BPM change command.
func (a *App) ChangeBPM(bpm float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "ChangeBpm", BPM: bpm}
	}
	return nil
}

// SetInterval changes the room's interval length (ADR-0004). The relay
// reanchors the room clock, so the change applies at the next interval boundary.
func (a *App) SetInterval(bars uint32, quantum float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		return fmt.Errorf("not in a session")
	}
	if bars == 0 || quantum <= 0 {
		return fmt.Errorf("bars and beats per bar must be positive")
	}
	a.session.CmdCh <- SessionCommand{Type: "SetInterval", Bars: bars, Quantum: quantum}
	return nil
}

// SendChat sends a chat message.
func (a *App) SendChat(text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SendChat", Text: text}
	}
	return nil
}

// SetTestTone controls the test tone generator.
func (a *App) SetTestTone(streamIndex *uint16) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetTestTone", StreamIndex: streamIndex}
	}
	return nil
}

// SetWavSender starts or stops the WAV file sender on a stream.
func (a *App) SetWavSender(streamIndex *uint16, wavFile string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetWavSender", StreamIndex: streamIndex, WavFile: wavFile}
	}
	return nil
}

// GetDefaultRecordingDir returns the default recording directory.
func (a *App) GetDefaultRecordingDir() (string, error) {
	return DefaultRecordingDir()
}

type CleanupResult struct {
	DeletedCount uint32 `json:"deleted_count"`
	FreedBytes   uint64 `json:"freed_bytes"`
}

// CleanupRecordings deletes old recording sessions.
func (a *App) CleanupRecordings(directory string, retentionDays uint32) (*CleanupResult, error) {
	deleted, freed, err := CleanupOldSessions(directory, retentionDays)
	if err != nil {
		return nil, err
	}
	return &CleanupResult{DeletedCount: deleted, FreedBytes: freed}, nil
}

// GetActiveSession returns the current session info, if any.
func (a *App) GetActiveSession() *JoinResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		return nil
	}
	return &JoinResult{PeerID: a.session.PeerID, Room: a.session.Room, BPM: 120.0}
}

// SetRememberEnabled mirrors the frontend "Remember settings" checkbox. When
// off, the persisted enabled-capture-channel set is deleted; when on, the
// current in-memory set is written out.
func (a *App) SetRememberEnabled(enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rememberEnabled = enabled
	if enabled {
		SaveCaptureEnabled(a.dataDir, a.captureEnabled)
	} else {
		DeleteCaptureEnabled(a.dataDir)
	}
	return nil
}

// SetTelemetry toggles file logging (telemetry).
func (a *App) SetTelemetry(enabled bool) error {
	if a.fileLog != nil {
		a.fileLog.SetEnabled(enabled)
		log.Printf("[app] Telemetry toggled: %v", enabled)
	}
	return nil
}

// DebugRoom joins the shared "wail-debug" room with all diagnostics armed:
// the WAIL Metronome broadcast (a grid-rendered reference click every peer
// can measure against), server-echo loopback, and peer log sharing (the room
// collates everyone's logs). Built for offset/latency hunts: have the other
// peer press it too, then compare their content against their own metronome
// (linkaudio-probe -offset-ref).
func (a *App) DebugRoom(displayName string, linkAudioName *string) (*JoinResult, error) {
	res, err := a.JoinRoom("wail-debug", nil, displayName, nil, nil, nil, nil, nil, nil, nil, nil, linkAudioName)
	if err != nil {
		return nil, err
	}
	if err := a.SetMetronomeBroadcast(true); err != nil {
		log.Printf("[app] debug room: metronome broadcast failed: %v", err)
	}
	if err := a.SetLoopback(true); err != nil {
		log.Printf("[app] debug room: loopback failed: %v", err)
	}
	if err := a.SetLogSharing(true); err != nil {
		log.Printf("[app] debug room: log sharing failed: %v", err)
	}
	log.Printf("[app] debug room joined: metronome broadcast + loopback + log sharing armed")
	return res, nil
}

// logSource returns the session's log entry source when the writer exists
// (nil otherwise) — the session pumps it to the room as LogBroadcast.
func (a *App) logSource() <-chan WsLogEntry {
	if a.wsLog == nil {
		return nil
	}
	return a.wsLog.Subscribe()
}

// SetLogSharing toggles WebSocket log broadcasting to peers.
func (a *App) SetLogSharing(enabled bool) error {
	if a.wsLog != nil {
		a.wsLog.SetEnabled(enabled)
		log.Printf("[app] Peer log sharing toggled: %v", enabled)
	}
	return nil
}

// SetCaptureEnabled toggles whether a discovered local Link Audio channel is
// bridged (the capture send-mixer). channelID is the hex id from a status
// update's capture_channels.
func (a *App) SetCaptureEnabled(channelID string, enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetCaptureEnabled", ChannelID: channelID, Enabled: enabled}
	}
	return nil
}

// SetCaptureDump toggles the debug capture-to-WAV dump. While on, each enabled
// capture channel writes a pre-Opus and a post-Opus WAV under ~/.wail/dumps/
// (the destination is logged); used to diagnose where transmitted audio degrades.
func (a *App) SetCaptureDump(enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetCaptureDump", Enabled: enabled}
	}
	return nil
}

// SetLoopback toggles the server-echo loopback: the relay sends our own audio
// frames back to us and they are republished as a "(loopback)" Link Audio
// channel one interval late — a live monitor of exactly what peers hear.
func (a *App) SetLoopback(enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetLoopback", Enabled: enabled}
	}
	return nil
}

// SetMetronome toggles the room metronome Link Audio channel ("WAIL · {peer} · Metronome"): a click on
// every beat (accented on bar downbeats) on the local Link grid, published so
// the user can subscribe in their DAW and align it against the DAW's metronome.
func (a *App) SetMetronome(enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetMetronome", Enabled: enabled}
	}
	return nil
}

// SetMetronomeBroadcast toggles broadcasting the WAIL Metronome click to the
// room as an audio stream: peers auto-publish it as a "WAIL · {peer} · Metronome"
// Link Audio channel and hear it one interval late. Independent of SetMetronome
// (the local-only channel) — both can run at once.
func (a *App) SetMetronomeBroadcast(enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetMetronomeBroadcast", Enabled: enabled}
	}
	return nil
}

// SetCushionMs live-adjusts the emit cushion (feed-ahead depth, ms) — how far
// ahead of the playhead each Link Audio sink is kept fed. Clamped to 10..500.
func (a *App) SetCushionMs(ms int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetCushionMs", Value: ms}
	}
	return nil
}

// SetIntervalOffset live-adjusts the receive playout offset D (intervals) —
// the NINJAM latency/reliability knob. Clamped to 0..4; default 1.
func (a *App) SetIntervalOffset(d int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetIntervalOffset", Value: d}
	}
	return nil
}

// SetGridAlign toggles active grid alignment (ADR-0006): the entry
// conformance snap and the steady-state grid slew. Default on; disabling
// restores the exact room tempo if a slew is in progress.
func (a *App) SetGridAlign(enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		a.session.CmdCh <- SessionCommand{Type: "SetGridAlign", Enabled: enabled}
	}
	return nil
}

// RenameStream updates a stream name.
func (a *App) RenameStream(streamIndex uint16, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	trimmed := name
	if len(trimmed) > 32 {
		trimmed = trimmed[:32]
	}
	if trimmed == "" {
		delete(a.streamNames, streamIndex)
	} else {
		a.streamNames[streamIndex] = trimmed
	}
	// Persisting is the session's job: it re-keys these indices onto channels
	// (OnStreamNamesChanged). Renaming with no session is a no-op on disk.
	if a.session != nil {
		snapshot := make(map[uint16]string, len(a.streamNames))
		for k, v := range a.streamNames {
			snapshot[k] = v
		}
		a.session.CmdCh <- SessionCommand{Type: "StreamNamesChanged", Names: snapshot}
	}
	return nil
}

// --- Identity management ---

func getOrCreateIdentity(dataDir string) string {
	idPath := filepath.Join(dataDir, "identity")
	if data, err := os.ReadFile(idPath); err == nil {
		trimmed := strings.TrimSpace(string(data))
		if trimmed != "" {
			// Validate it looks like a UUID; if not, regenerate
			if _, err := uuid.Parse(trimmed); err == nil {
				log.Printf("[identity] Loaded persistent identity: %s", trimmed)
				return trimmed
			}
			log.Printf("[identity] Existing identity is not a valid UUID, regenerating")
		}
	}

	id := uuid.New().String()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Printf("[identity] Failed to create data dir: %v — using ephemeral identity", err)
		return id
	}
	if err := os.WriteFile(idPath, []byte(id), 0o644); err != nil {
		log.Printf("[identity] Failed to persist identity: %v — using ephemeral identity", err)
	} else {
		log.Printf("[identity] Created new persistent identity: %s", id)
	}
	return id
}

func defaultDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".wail")
}
