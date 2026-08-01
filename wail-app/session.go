package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"maps"
	"math"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

// SessionConfig holds configuration for a session.
type SessionConfig struct {
	Server      string
	Room        string
	Password    *string
	DisplayName string
	// LinkAudioName is the peer name WAIL advertises to Link Audio (and the
	// own-channel filter key); defaults to "WAIL" upstream in JoinRoom.
	LinkAudioName string
	Identity      string
	BPM           float64
	Bars          uint32
	Quantum       float64
	Recording     *RecordingConfig
	StreamCount   uint16
	// CaptureRestore is the remembered set of enabled capture channels (keyed
	// by peer/channel name), restored into the audio engine at session start.
	CaptureRestore []CaptureChannelKey
	// OnCaptureEnabledChanged fires after a user capture-toggle with the full
	// enabled set, so the app can persist it (gated on "Remember settings").
	OnCaptureEnabledChanged func([]CaptureChannelKey)
	// LogSource, when set, is pumped to the room as LogBroadcast messages
	// (peer log sharing, #440). Entries flow only when the writer is armed
	// (debug room / -log-sharing / GUI toggle).
	LogSource <-chan WsLogEntry
}

// SessionCommand represents commands from the UI to the session.
type SessionCommand struct {
	Type        string // "ChangeBpm", "SendChat", "StreamNamesChanged", "SetTestTone", "SetWavSender", "SetCaptureEnabled", "SetCaptureDump", "SetLoopback", "SetMetronome", "SetMetronomeBroadcast", "SetCushionMs", "SetIntervalOffset", "SetInterval", "Disconnect"
	BPM         float64
	Text        string
	Names       map[uint16]string
	StreamIndex *uint16
	WavFile     string
	ChannelID   string  // SetCaptureEnabled
	Enabled     bool    // SetCaptureEnabled, SetCaptureDump, SetLoopback, SetMetronome, SetMetronomeBroadcast
	Bars        uint32  // SetInterval
	Quantum     float64 // SetInterval
	Value       int     // SetCushionMs, SetIntervalOffset
}

// SessionHandle represents a running session.
type SessionHandle struct {
	CmdCh  chan SessionCommand
	PeerID string
	Room   string
	cancel context.CancelFunc
	done   chan struct{} // closed when session goroutine exits
}

// EventEmitter abstracts frontend event emission.
type EventEmitter interface {
	Emit(event string, data any)
}

// SpawnSession starts a new session in a goroutine.
func SpawnSession(emitter EventEmitter, config SessionConfig) (*SessionHandle, error) {
	cmdCh := make(chan SessionCommand, 64)
	peerID := generateShortID()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	handle := &SessionHandle{
		CmdCh:  cmdCh,
		PeerID: peerID,
		Room:   config.Room,
		cancel: cancel,
		done:   done,
	}

	go func() {
		defer close(done)
		if err := sessionLoop(ctx, emitter, config, peerID, cmdCh); err != nil {
			log.Printf("[session] Error: %v", err)
			emitter.Emit("session:error", SessionError{Message: err.Error()})
		}
		emitter.Emit("session:ended", SessionEnded{})
	}()

	return handle, nil
}

func generateShortID() string {
	return uuid.New().String()[:8]
}

func sessionLoop(
	ctx context.Context,
	emitter EventEmitter,
	config SessionConfig,
	peerID string,
	cmdCh <-chan SessionCommand,
) error {
	displayName := config.DisplayName
	identity := config.Identity
	room := config.Room
	bpm := config.BPM
	bars := config.Bars
	quantum := config.Quantum

	logInfo := func(msg string, args ...any) {
		formatted := fmt.Sprintf(msg, args...)
		log.Printf("[session] %s", formatted)
		emitter.Emit("log:entry", LogEntry{Level: "info", Message: formatted})
	}
	logWarn := func(msg string, args ...any) {
		formatted := fmt.Sprintf(msg, args...)
		log.Printf("[session] WARN: %s", formatted)
		emitter.Emit("log:entry", LogEntry{Level: "warn", Message: formatted})
	}

	logInfo("Starting peer %s as %s in room %s (BPM %.0f, %d bars, quantum %.0f)", peerID, displayName, room, bpm, bars, quantum)

	// Initialize Ableton Link
	intervalCfg := interval.Config{Bars: bars, Quantum: quantum}
	link := NewLinkBridge(bpm, quantum)
	link.SetIntervalQuantum(intervalCfg.BeatsPerInterval())
	link.Enable()
	linkCmdCh, linkEventCh := link.SpawnPoller(ctx)
	logInfo("Ableton Link enabled")

	// Connect to signaling server
	mesh, syncRx, audioRx, err := connectMesh(ctx, config, peerID)
	if err == nil && config.LogSource != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case entry, ok := <-config.LogSource:
					if !ok {
						return
					}
					mesh.SendLog(entry.Level, entry.Target, entry.Message, entry.TimestampUs)
				}
			}
		}()
	}
	if err != nil {
		return fmt.Errorf("signaling connect: %w", err)
	}
	logInfo("Connected to signaling server at %s", config.Server)

	emitter.Emit("session:started", SessionStarted{PeerID: peerID, Room: room, BPM: bpm})

	// Link Audio engine (ADR-0001/0002) — the only audio path: capture subscribes
	// to local Link Audio channels and playback republishes remote streams a
	// round late, adaptively (ADR-0009; a no-op stub under -tags linkstub).
	// Engine sends happen on pacer goroutines, not the session loop, so they are
	// counted atomically and folded into the per-interval sent= log line.
	var engineFramesSent atomic.Uint64
	engineSend := func(waif []byte) {
		engineFramesSent.Add(1)
		mesh.BroadcastAudio(waif)
	}
	// Advertise the user's Link Audio name (defaults to "WAIL") so WAIL reads
	// clearly in a DAW's peer list. This is also the own-channel filter key
	// (audio_engine_real.go), so the engine uses it end to end; it is separate
	// from the room display name.
	audioEngine := newAudioEngine(link, config.LinkAudioName, engineSend)
	audioEngine.SetCaptureRestore(config.CaptureRestore)
	if err := audioEngine.Start(); err != nil {
		logWarn("Link Audio engine failed to start: %v", err)
	}
	defer audioEngine.Stop()
	logInfo("Link Audio engine enabled (adaptive playout)")

	// State
	clock := NewClockSync()
	peers := NewPeerRegistry()
	if names := mesh.TakeInitialPeerNames(); names != nil {
		peers.SeedNames(names)
	}

	var lastIntervalIndex *int64
	// foundedRoom tracks whether this peer anchored an empty room (ADR-0004):
	// joiners — not founders — get the launch-quantization prompt.
	foundedRoom := false
	intervalPromptSent := false
	localStreamNames := make(map[uint16]string)

	// The tempo the session last committed to (adopted, declared, or seeded).
	// Grid alignment is retired (ADR-0009): grids are never physically aligned
	// across LANs — playout re-quantizes every round onto the local grid, so
	// cross-LAN phase never reaches the ear. This plain record is all that
	// remains of the steerer's committed-tempo bookkeeping.
	currentBPM := bpm
	// adoptedRoomTempo: whether this (re)connect has adopted the room's tempo
	// yet — a joiner takes it from the first anchor, once, and declarations
	// own it from there.
	adoptedRoomTempo := false

	// Receivers label our republished channels with these names (StreamNames
	// sync): enabled capture channels default to their discovered channel name,
	// overridden by user-set names. Recomputed on the status tick (discovery
	// changes don't raise commands) and after any command that affects it;
	// broadcast only when the map actually changes.
	var effStreamNames, sentStreamNames map[uint16]string
	syncStreamNames := func() {
		effStreamNames = effectiveStreamNames(audioEngine.CaptureChannels(), localStreamNames)
		if maps.Equal(effStreamNames, sentStreamNames) {
			return
		}
		sentStreamNames = effStreamNames
		mesh.Broadcast(NewStreamNames(StreamNamesToWire(effStreamNames)))
	}

	// Audio stats
	var audioIntervalsSent, audioIntervalsReceived uint64
	var audioBytesSent, audioBytesRecv uint64
	var audioStatusSeq uint64
	var intervalFramesSent, intervalFramesRecv uint64
	var intervalBytesSent, intervalBytesRecv uint64
	var localDropCount atomic.Uint64
	var boundaryDriftUs *int64

	// Local send tracking: localSendStreams is the persistent set of in-app
	// senders (test tone / WAV) for the UI; localSendActive flags which sent a
	// frame in the current status tick and is reset each tick.
	localSendStreams := make(map[uint16]bool)
	localSendActive := make(map[uint16]bool)
	loggedFirstFrameSent := false

	// Relay rate-limit bookkeeping: the relay scales our per-peer binary token
	// bucket by the stream count declared at join, but streams open and close
	// all session long (capture toggles, restore-set auto-enable, in-app
	// senders). The status tick pushes an update_streams whenever the live
	// count drifts from what we last declared. Initialized to the join value.
	// If the relay rejects a declaration (room_full), streamUpdateRejected
	// arms a retry after streamUpdateRetryBackoff — otherwise a rejection
	// would leave the bucket undersized until the next drift.
	lastDeclaredStreams := int(config.StreamCount)
	streamUpdateRejected := false
	streamUpdateRetryAt := time.Time{}

	// Test tone state
	var testToneBoundaryCh chan IntervalBoundaryInfo
	var testToneCancelFn context.CancelFunc
	var testToneStream *uint16

	// WAV sender state
	var wavSenderBoundaryCh chan IntervalBoundaryInfo
	var wavSenderCancelFn context.CancelFunc
	var wavSenderStream *uint16

	// Broadcast-metronome sender state: an in-app sender that streams the WAIL
	// Metronome click to the room as audio (nil channel = off).
	var metronomeSendBoundaryCh chan IntervalBoundaryInfo
	var metronomeSendCancelFn context.CancelFunc

	// Server-echo loopback (debug): the relay echoes our own audio frames back
	// and we republish them as a "(loopback)" Link Audio channel. loopbackState
	// is a detached PeerState for loss tracking only — self is not a peer.
	loopbackEnabled := false
	loopbackState := NewPeerState(nil)

	// Engine diagnostics: diffed each status tick so every likely-audible event
	// (capture loss, re-anchor, incomplete interval, …) surfaces in the log panel.
	var lastHealth EngineHealth

	// In-app senders (test tone / WAV) push completed WAIF frames here; the loop
	// forwards them to the relay. Real capture goes straight through the Link
	// Audio engine (audio_engine_real.go).
	localWaifCh := make(chan []byte, 64)
	sendWaif := func(w []byte) {
		select {
		case localWaifCh <- w:
		default:
			localDropCount.Add(1)
		}
	}

	// Recording
	var recorder *SessionRecorder
	if config.Recording != nil && config.Recording.Enabled {
		r, err := StartRecording(*config.Recording, room)
		if err != nil {
			logWarn("Failed to start recording: %v", err)
		} else {
			recorder = r
			logInfo("Recording enabled: %s", config.Recording.Directory)
		}
	}

	// Timers
	pingTicker := time.NewTicker(time.Duration(PingIntervalMs) * time.Millisecond)
	defer pingTicker.Stop()
	statusTicker := time.NewTicker(2 * time.Second)
	defer statusTicker.Stop()
	livenessTicker := time.NewTicker(5 * time.Second)
	defer livenessTicker.Stop()
	gridJumpTicker := time.NewTicker(1 * time.Second)
	defer gridJumpTicker.Stop()

	var lastBoundaryTime *time.Time

	// Signaling reconnect state
	type sigReconnect struct {
		attempt uint32
		nextTry time.Time
	}
	var reconnect *sigReconnect
	reconnectTimer := time.NewTimer(time.Hour)
	reconnectTimer.Stop()

	// Tempo declarations (ADR-0009): the room's tempo state is a Link-style
	// timeline — the value plus the priority stamp that arbitrates it. Ours
	// updates on every declaration we make or adopt; conflicts resolve by
	// tempoDeclareAdopts (strictly-greater origin, owner tie-break).
	var tempoOrigin int64
	var tempoOwner string
	// declareTempo broadcasts a tempo the local musician (or their DAW, after
	// de-noising) chose: intent by construction, no threshold, no hold-down.
	// Dual-send during the compat cycle (#509): old clients adopt via the
	// legacy TempoChange, and the relay room clock still re-anchors from it.
	declareTempo := func(bpm float64) {
		origin := time.Now().UnixMicro()
		if origin <= tempoOrigin {
			origin = tempoOrigin + 1
		}
		tempoOrigin, tempoOwner = origin, identity
		mesh.Broadcast(NewTempoDeclare(bpm, origin, identity))
		mesh.Broadcast(NewTempoChange(bpm, intervalCfg.Quantum, time.Now().UnixMicro()))
	}

	// handleBoundary fires on each Link event: if the given beat crossed into a
	// new local interval, it logs the boundary, records timing drift, and hands
	// the boundary to the in-app senders (test tone / WAV). It closes over
	// sessionLoop state (interval config, frame counters, drift, sender
	// channels) so the call sites pass only the beat.
	handleBoundary := func(beat float64) {
		idx := intervalCfg.IndexAtBeat(beat)
		if lastIntervalIndex != nil && idx <= *lastIntervalIndex {
			return
		}
		newIdx := idx
		lastIntervalIndex = &newIdx

		log.Printf("[session] >>> INTERVAL local=%d <<< beat=%.1f sent=%d recv=%d", idx, beat, intervalFramesSent+engineFramesSent.Swap(0), intervalFramesRecv)
		intervalFramesSent = 0
		intervalFramesRecv = 0
		intervalBytesSent = 0
		intervalBytesRecv = 0

		if lastBoundaryTime != nil && currentBPM > 0 {
			gap := time.Since(*lastBoundaryTime)
			expectedUs := int64(intervalCfg.BeatsPerInterval() / (currentBPM / 60.0) * 1_000_000.0)
			drift := gap.Microseconds() - expectedUs
			boundaryDriftUs = &drift
		}
		now := time.Now()
		lastBoundaryTime = &now

		// In-app senders stamp the LOCAL interval index (ADR-0009): rounds are
		// sender-relative, so no room mapping exists or is needed.
		info := IntervalBoundaryInfo{Index: idx, BPM: currentBPM, Cfg: intervalCfg}
		if testToneBoundaryCh != nil {
			select {
			case testToneBoundaryCh <- info:
			default:
			}
		}
		if wavSenderBoundaryCh != nil {
			select {
			case wavSenderBoundaryCh <- info:
			default:
			}
		}
		if metronomeSendBoundaryCh != nil {
			select {
			case metronomeSendBoundaryCh <- info:
			default:
			}
		}
	}

	// Signaling event goroutine → channel
	sigEventCh := make(chan *MeshEvent, 64)
	sigClosedCh := make(chan struct{})
	var sigMu sync.Mutex
	currentSyncRx := syncRx
	currentAudioRx := audioRx

	go func() {
		for {
			ev, ok := mesh.PollSignaling()
			if !ok {
				close(sigClosedCh)
				return
			}
			if ev != nil {
				sigEventCh <- ev
			}
		}
	}()

	logInfo("Waiting for peers...")

	for {
		select {
		case <-ctx.Done():
			goto cleanup

		// --- UI commands ---
		case cmd := <-cmdCh:
			switch cmd.Type {
			case "ChangeBpm":
				// WAIL's own tempo control: intent by construction (ADR-0009).
				// Broadcast it — this path used to stop at the local session,
				// so a UI tempo change never reached the room at all.
				logInfo("BPM changed to %.1f (declared)", cmd.BPM)
				currentBPM = cmd.BPM
				linkCmdCh <- LinkCommand{Type: "SetTempo", BPM: cmd.BPM}
				declareTempo(cmd.BPM)
				emitter.Emit("tempo:changed", TempoChangedEvent{BPM: cmd.BPM, Source: "local"})
			case "SetInterval":
				// ADR-0004: anyone may change the room interval; the relay
				// reanchors at the next boundary, so the current interval
				// finishes under the old config.
				if cmd.Bars > 0 && cmd.Quantum > 0 {
					intervalCfg = interval.Config{Bars: cmd.Bars, Quantum: cmd.Quantum}
					link.SetIntervalQuantum(intervalCfg.BeatsPerInterval())
					logInfo("Interval changed to %d bars x %.0f beats (%d BPI) — applies at the next interval boundary",
						cmd.Bars, cmd.Quantum, uint32(float64(cmd.Bars)*cmd.Quantum))
					mesh.Broadcast(NewIntervalConfig(cmd.Bars, cmd.Quantum))
				}
			case "SendChat":
				msg := NewChatMessage(displayName, cmd.Text)
				mesh.Broadcast(msg)
				emitter.Emit("chat:message", ChatMessageEvent{SenderName: displayName, IsOwn: true, Text: cmd.Text})
			case "StreamNamesChanged":
				localStreamNames = cmd.Names
				syncStreamNames()
			case "SetTestTone":
				// Stop existing test tone
				if testToneCancelFn != nil {
					testToneCancelFn()
					testToneCancelFn = nil
				}
				testToneBoundaryCh = nil
				if testToneStream != nil {
					delete(localStreamNames, *testToneStream)
					delete(localSendStreams, *testToneStream)
				}
				testToneStream = nil

				if cmd.StreamIndex != nil {
					si := *cmd.StreamIndex
					toneCtx, cancelFn := context.WithCancel(ctx)
					testToneCancelFn = cancelFn
					boundaryCh := make(chan IntervalBoundaryInfo, 4)
					testToneBoundaryCh = boundaryCh
					testToneStream = &si
					localSendStreams[si] = true

					toneName := "Test Tone"
					if displayName != "" {
						toneName = displayName + "'s Test Tone"
					}
					localStreamNames[si] = toneName

					go TestToneTask(toneCtx, si, sendWaif, boundaryCh)
					logInfo("[TEST] Test tone started on Send %d", si)
				} else {
					logInfo("[TEST] Test tone stopped")
				}
				syncStreamNames()
			case "SetWavSender":
				// Stop existing WAV sender
				if wavSenderCancelFn != nil {
					wavSenderCancelFn()
					wavSenderCancelFn = nil
				}
				wavSenderBoundaryCh = nil
				if wavSenderStream != nil {
					delete(localStreamNames, *wavSenderStream)
					delete(localSendStreams, *wavSenderStream)
				}
				wavSenderStream = nil

				if cmd.StreamIndex != nil && cmd.WavFile != "" {
					si := *cmd.StreamIndex
					wavCtx, cancelFn := context.WithCancel(ctx)
					wavSenderCancelFn = cancelFn
					boundaryCh := make(chan IntervalBoundaryInfo, 4)
					wavSenderBoundaryCh = boundaryCh
					wavSenderStream = &si
					localSendStreams[si] = true

					wavName := "WAV Sender"
					if displayName != "" {
						wavName = displayName + "'s WAV"
					}
					localStreamNames[si] = wavName

					go WavSenderTask(wavCtx, si, sendWaif, boundaryCh, cmd.WavFile)
					logInfo("[WAV] WAV sender started on Send %d: %s", si, cmd.WavFile)
				} else {
					logInfo("[WAV] WAV sender stopped")
				}
				syncStreamNames()
			case "SetCaptureEnabled":
				audioEngine.SetCaptureEnabled(cmd.ChannelID, cmd.Enabled)
				logInfo("[capture] channel %s enabled=%v", cmd.ChannelID, cmd.Enabled)
				if config.OnCaptureEnabledChanged != nil {
					var keys []CaptureChannelKey
					for _, cc := range audioEngine.CaptureChannels() {
						if cc.Enabled {
							keys = append(keys, CaptureChannelKey{PeerName: cc.PeerName, ChannelName: cc.Name})
						}
					}
					config.OnCaptureEnabledChanged(keys)
				}
				syncStreamNames()
			case "SetCaptureDump":
				audioEngine.SetCaptureDump(cmd.Enabled)
				logInfo("[capture] dump enabled=%v", cmd.Enabled)
			case "SetLoopback":
				loopbackEnabled = cmd.Enabled
				mesh.SendLoopback(cmd.Enabled)
				if cmd.Enabled {
					// Undo any earlier drop: nothing else ever will, since the echo
					// identity never sends StreamNames, and a drop left in force
					// retires the monitor channels the next time frames pause.
					audioEngine.ClearPeerIntent(identity + loopbackIdentitySuffix)
				} else {
					// The echo stops, so no StreamNames or PeerLeft will ever reach
					// these — retire them explicitly or the monitor channels stay
					// published for the rest of the session.
					audioEngine.DropPeer(identity + loopbackIdentitySuffix)
				}
				logInfo("[loopback] server echo enabled=%v", cmd.Enabled)
			case "SetMetronome":
				audioEngine.SetMetronome(cmd.Enabled)
				logInfo("[metronome] enabled=%v", cmd.Enabled)
			case "SetMetronomeBroadcast":
				// Stop any running broadcast sender (also the teardown path).
				if metronomeSendCancelFn != nil {
					metronomeSendCancelFn()
					metronomeSendCancelFn = nil
				}
				metronomeSendBoundaryCh = nil
				delete(localStreamNames, metronomeBroadcastStreamID)
				delete(localSendStreams, metronomeBroadcastStreamID)
				if cmd.Enabled {
					metCtx, cancelFn := context.WithCancel(ctx)
					metronomeSendCancelFn = cancelFn
					boundaryCh := make(chan IntervalBoundaryInfo, 4)
					metronomeSendBoundaryCh = boundaryCh
					localSendStreams[metronomeBroadcastStreamID] = true
					// Plain "Metronome": the republish path prepends "WAIL · {peer} · ",
					// so receivers show "WAIL · {peer} · Metronome" (no doubled "WAIL").
					localStreamNames[metronomeBroadcastStreamID] = "Metronome"
					go MetronomeSenderTask(metCtx, metronomeBroadcastStreamID, sendWaif, boundaryCh)
					logInfo("[metronome] broadcast to room started")
				} else {
					logInfo("[metronome] broadcast to room stopped")
				}
				syncStreamNames()
			case "SetCushionMs":
				eff := audioEngine.SetCushionMs(cmd.Value)
				logInfo("[audio] emit cushion set to %dms", eff)
			case "SetIntervalOffset":
				audioEngine.SetIntervalOffset(cmd.Value) // retired (ADR-0009); logs and ignores
			case "Disconnect":
				logInfo("Disconnecting...")
				goto cleanup
			}

		// --- Signaling events ---
		case ev := <-sigEventCh:
			switch ev.Type {
			case "PeerJoined":
				display := ev.PeerID
				if ev.DisplayName != nil {
					display = *ev.DisplayName
				}
				logInfo("Peer %s joined room", display)
				peers.Add(ev.PeerID, ev.DisplayName)
				emitter.Emit("peer:joined", PeerJoinedEvent{PeerID: ev.PeerID, DisplayName: ev.DisplayName})

				hello := NewHello(peerID, &displayName, &identity)
				mesh.Broadcast(hello)
				// ADR-0004: re-broadcast the ADOPTED room config, not our
				// join-time preference — the relay's last-writer-wins gossip
				// would otherwise flap the room clock mid-jam.
				mesh.Broadcast(NewIntervalConfig(intervalCfg.Bars, intervalCfg.Quantum))
				mesh.Broadcast(NewAudioCapabilities([]uint32{48000}, []uint16{1, 2}, true, true))
				sentStreamNames = nil // new peer: re-broadcast even if unchanged
				syncStreamNames()

			case "LogBroadcast":
				// Peer log sharing: fold a peer's log line into our log (and the
				// GUI panel) with their name, so one client collates the room.
				name := ev.From
				peers.WithPeer(ev.From, func(p *PeerState) {
					if p.DisplayName != nil {
						name = *p.DisplayName
					}
				})
				log.Printf("[%s] %s", name, ev.Message)

			case "PeerLeft":
				var name, goneIdentity string
				peers.WithPeer(ev.PeerID, func(p *PeerState) {
					if p.DisplayName != nil {
						name = *p.DisplayName
					}
					if p.Identity != nil {
						goneIdentity = *p.Identity
					}
				})
				if name == "" {
					name = ev.PeerID
				}
				if goneIdentity == "" {
					goneIdentity = ev.PeerID // pre-Hello peers publish under their peer id
				}
				logInfo("Peer %s left", name)
				// Their channels stay published through the retirement grace, so a
				// rejoin inside it keeps the same channels (affinity). Drop the peer
				// id too: their queued frames arrive on a different channel than this
				// event, and once the registry entry is gone those frames publish
				// under the peer-id fallback instead of the identity.
				audioEngine.DropPeer(goneIdentity)
				if goneIdentity != ev.PeerID {
					audioEngine.DropPeer(ev.PeerID)
				}
				peers.Remove(ev.PeerID)
				emitter.Emit("peer:left", PeerLeftEvent{PeerID: ev.PeerID})

			case "PeerListReceived":
				peers.SeedLastSeen()

			case "UpdateStreamsError":
				// The relay kept the OLD count; arm a retry so a freed slot heals
				// the undersized rate-limit bucket without waiting for drift.
				streamUpdateRejected = true
				streamUpdateRetryAt = time.Now().Add(streamUpdateRetryBackoff)
				logWarn("[signaling] relay rejected stream update (%s, %d slots available) — retrying in %s", ev.Code, ev.SlotsAvailable, streamUpdateRetryBackoff)
				logInfo("Joined room with %d peer(s)", ev.PeerCount)
				if ev.PeerCount == 0 {
					// Founding an empty room: assert tempo + interval config so the
					// relay can anchor the room clock now. Without an anchor the
					// engine has no room labels and releases nothing — a solo peer
					// monitoring their own loopback would hear silence.
					foundedRoom = true
					declareTempo(bpm)
					mesh.Broadcast(NewIntervalConfig(intervalCfg.Bars, intervalCfg.Quantum))
				}
			}

		case <-sigClosedCh:
			if reconnect == nil {
				logWarn("Signaling connection closed — attempting reconnection")
				emitter.Emit("session:reconnecting", nil)
				reconnect = &sigReconnect{attempt: 1, nextTry: time.Now().Add(time.Second)}
				reconnectTimer.Reset(time.Second)
			}

		// --- Signaling reconnection ---
		case <-reconnectTimer.C:
			if reconnect == nil {
				continue
			}
			attempt := reconnect.attempt
			if attempt == 10 {
				logWarn("Signaling reconnection stale after %d attempts", attempt)
				emitter.Emit("session:stale", SessionStale{Attempts: attempt})
			}
			logInfo("Signaling reconnect attempt %d...", attempt)

			newChannels, newNames, err := mesh.ReconnectSignaling(ctx, config.Server, room, config.Password, &displayName)
			if err != nil {
				logWarn("Signaling reconnect failed: %v", err)
				nextAttempt := attempt + 1
				backoffMs := min64(1000*pow2(nextAttempt-1), 30000)
				reconnect.attempt = nextAttempt
				reconnect.nextTry = time.Now().Add(time.Duration(backoffMs) * time.Millisecond)
				reconnectTimer.Reset(time.Duration(backoffMs) * time.Millisecond)
			} else {
				if newNames != nil {
					peers.SeedNames(newNames)
				}
				sigMu.Lock()
				currentSyncRx = newChannels.SyncCh
				currentAudioRx = newChannels.AudioCh
				sigMu.Unlock()

				// The relay resets loopback on rejoin; re-arm it.
				if loopbackEnabled {
					mesh.SendLoopback(true)
				}

				// Restart signaling poll goroutine
				sigEventCh2 := make(chan *MeshEvent, 64)
				sigClosedCh2 := make(chan struct{})
				sigEventCh = sigEventCh2
				sigClosedCh = sigClosedCh2
				go func() {
					for {
						ev, ok := mesh.PollSignaling()
						if !ok {
							close(sigClosedCh2)
							return
						}
						if ev != nil {
							sigEventCh2 <- ev
						}
					}
				}()

				reconnect = nil
				// The rejoin re-declared the configured stream count; the next
				// status tick pushes update_streams if the live count differs.
				lastDeclaredStreams = int(config.StreamCount)
				// The room's tempo may have moved while we were gone: take it
				// from the next anchor, once, as a join does (ADR-0009).
				adoptedRoomTempo = false
				logInfo("Signaling reconnected (attempt %d)", attempt)
				emitter.Emit("session:reconnected", nil)
			}

		// --- Sync messages from peers ---
		case fps := <-currentSyncRx:
			from := fps.From
			msg := fps.Msg
			peers.WithPeer(from, func(p *PeerState) {
				p.LastSeen = time.Now()
				p.EverReceivedMessage = true
			})

			switch msg.Type {
			case "Hello":
				nameDisplay := "(anonymous)"
				if msg.DisplayName != nil {
					nameDisplay = *msg.DisplayName
				}
				logInfo("Hello from %s (%s)", nameDisplay, msg.PeerID)

				peers.WithPeer(msg.PeerID, func(p *PeerState) {
					p.DisplayName = msg.DisplayName
				})
				if peers.Get(msg.PeerID) == nil {
					peers.Add(msg.PeerID, msg.DisplayName)
				}

				if msg.Identity != nil {
					rid := *msg.Identity
					// Evict stale peer
					if oldPID, found := peers.FindByIdentity(rid); found && oldPID != msg.PeerID {
						logInfo("Peer %s reconnected (old=%s, new=%s) — evicting stale", nameDisplay, oldPID, msg.PeerID)
						peers.Remove(oldPID)
						mesh.RemovePeer(oldPID)
						emitter.Emit("peer:left", PeerLeftEvent{PeerID: oldPID})
					}

					peers.WithPeer(msg.PeerID, func(p *PeerState) {
						p.Identity = msg.Identity
					})
					// Audio that beat this Hello was keyed by peer id (the
					// identity fallback), so it published under a key nothing
					// will ever refer to again — retire it, or it lingers beside
					// the identity-keyed channel as a duplicate.
					if rid != msg.PeerID {
						audioEngine.DropPeer(msg.PeerID)
					}
					peers.RekeyPeerSlots(msg.PeerID, rid)
					peers.AssignSlot(msg.PeerID, 0)
				}

				if peers.MarkHelloSent(from) {
					reply := NewHello(peerID, &displayName, &identity)
					mesh.SendTo(from, reply)
					if len(effStreamNames) > 0 {
						mesh.SendTo(from, NewStreamNames(StreamNamesToWire(effStreamNames)))
					}
				}

				emitter.Emit("peer:joined", PeerJoinedEvent{PeerID: msg.PeerID, DisplayName: msg.DisplayName})

			case "Ping":
				pong := clock.HandlePing(msg.ID, msg.SentAtUs)
				mesh.SendTo(from, pong)

			case "Pong":
				if from == "" {
					// Relay time service (ADR-0006): the relay's own pong feeds the
					// relay RTT estimate and the grid steer's server↔local offset.
					if msg.ServerNowMicros != 0 {
						// Relay RTT estimate (diagnostics). The server-time offset
						// used to feed grid alignment; retired (ADR-0009).
						clock.HandleServerPong(msg.PingSentAtUs)
					}
				} else {
					clock.HandlePong(from, msg.PingSentAtUs, msg.PongSentAtUs)
				}

			case "IntervalAnchor":
				// The anchor's remaining job (ADR-0009): carry the room's tempo
				// and interval config to joiners. The room index it also carries
				// is ignored — rounds are sender-relative now. Once every client
				// in the field ignores it, the relay's clock retires too.
				audioEngine.SetRoomConfig(msg.BPM, msg.Bars, msg.Quantum)
				// Adopt the room's config for session-side boundary math and the
				// bridge's interval-beat lens.
				if msg.Bars > 0 && msg.Quantum > 0 {
					intervalCfg = interval.Config{Bars: msg.Bars, Quantum: msg.Quantum}
				}
				link.SetIntervalQuantum(intervalCfg.BeatsPerInterval())
				// Join-time tempo adoption, once per (re)connect: a joiner takes
				// the room's tempo; declarations own it from there. This was the
				// steerer's entry-conformance adoption, minus the grid snap.
				if !adoptedRoomTempo && msg.BPM > 0 {
					adoptedRoomTempo = true
					if math.Abs(msg.BPM-currentBPM) > 0.01 {
						logInfo("Adopting room tempo %.1f BPM", msg.BPM)
						currentBPM = msg.BPM
						linkCmdCh <- LinkCommand{Type: "SetTempo", BPM: msg.BPM}
						emitter.Emit("tempo:changed", TempoChangedEvent{BPM: msg.BPM, Source: "remote"})
					}
				}
				// ADR-0004: the room interval is communicated, never enforced —
				// prompt joiners (once) to match their DAW's launch quantization.
				if !foundedRoom && !intervalPromptSent && msg.Bars > 0 {
					intervalPromptSent = true
					emitter.Emit("interval:prompt", IntervalPromptEvent{Bars: msg.Bars, Quantum: msg.Quantum, BPM: msg.BPM})
				}

			case "TempoDeclare":
				// ADR-0009: a declared tempo, arbitrated by priority — no
				// threshold, no hold-down. Our own dual-sent declaration echoes
				// back with an equal (origin, owner) and is inert by the rule.
				if tempoDeclareAdopts(msg.OriginMicros, msg.Owner, tempoOrigin, tempoOwner) {
					tempoOrigin, tempoOwner = msg.OriginMicros, msg.Owner
					if math.Abs(msg.BPM-currentBPM) > 0.01 {
						logInfo("Tempo declared by %s: %.1f BPM", msg.Owner, msg.BPM)
						currentBPM = msg.BPM
						linkCmdCh <- LinkCommand{Type: "SetTempo", BPM: msg.BPM}
						emitter.Emit("tempo:changed", TempoChangedEvent{BPM: msg.BPM, Source: "remote"})
					}
				}

			case "TempoChange":
				// Legacy path (pre-ADR-0009 clients). A new client's dual-send
				// arrives here too, already adopted via its TempoDeclare — the
				// equal-BPM guard keeps it from double-applying or inflating
				// the origin with a receipt-time stamp.
				if math.Abs(msg.BPM-currentBPM) <= 0.01 {
					break
				}
				var name string
				peers.WithPeer(from, func(p *PeerState) {
					if p.DisplayName != nil {
						name = *p.DisplayName
					}
				})
				if name == "" {
					name = from
				}
				logInfo("Tempo change from %s: %.1f BPM (legacy)", name, msg.BPM)
				if now := time.Now().UnixMicro(); now > tempoOrigin {
					tempoOrigin, tempoOwner = now, from
				}
				currentBPM = msg.BPM
				linkCmdCh <- LinkCommand{Type: "SetTempo", BPM: msg.BPM}
				emitter.Emit("tempo:changed", TempoChangedEvent{BPM: msg.BPM, Source: "remote"})

			case "StateSnapshot":
				// Tempo adoption from snapshots is retired (ADR-0009): tempo
				// crosses the WAN only as a declaration. Snapshots still flow
				// for old receivers; nothing here reads them anymore.

			case "IntervalConfig":
				logInfo("Remote interval config: %d bars, quantum %.0f", msg.Bars, msg.Quantum)
				intervalCfg = interval.Config{Bars: msg.Bars, Quantum: msg.Quantum}
				link.SetIntervalQuantum(intervalCfg.BeatsPerInterval())

			case "AudioStatus":
				peers.WithPeer(from, func(p *PeerState) {
					p.RemoteIntervalsSent = msg.IntervalsSent
				})

			case "ChatMessage":
				emitter.Emit("chat:message", ChatMessageEvent{SenderName: msg.SenderName, IsOwn: false, Text: msg.Text})

			case "StreamNames":
				// A nil Names is "I send nothing", not "no news": the field is
				// omitempty and syncStreamNames is its only sender, so a peer
				// dropping to zero streams arrives with the field absent.
				// Discarding it left their channels published forever.
				parsed := StreamNamesFromWire(msg.Names)
				var senderIdentity string
				peers.WithPeer(from, func(p *PeerState) {
					p.StreamNames = parsed
					if p.Identity != nil {
						senderIdentity = *p.Identity
					}
				})
				if senderIdentity == "" {
					senderIdentity = from
				}
				// The declared set is authoritative: anything of theirs we
				// publish that is missing from it gets retired once drained.
				keep := make(map[uint16]bool, len(parsed))
				for id := range parsed {
					keep[id] = true
				}
				audioEngine.SetPeerStreams(senderIdentity, keep)
			}

		// --- Audio from peers ---
		case fpa := <-currentAudioRx:
			from := fpa.From
			data := fpa.Data

			// Server-echo loopback: our own frames round-tripped through the
			// relay. Self is not a peer — skip the registry/slots/recorder and
			// hand straight to the engine under a distinct identity so it
			// republishes as a "(loopback)" monitor channel.
			if from == peerID {
				loopStreamName := ""
				if header := PeekWaifHeader(data); header != nil {
					if loss := recordFrame(loopbackState, header); loss != nil {
						logWarn("loopback packet_loss stream=%d lost=%d expected_seq=%d got_seq=%d interval=%d",
							loss.StreamID, loss.Lost, loss.ExpectedSeq, loss.GotSeq, loss.IntervalIdx)
					}
					loopStreamName = effStreamNames[header.StreamID]
				}
				audioIntervalsReceived++
				audioBytesRecv += uint64(len(data))
				intervalFramesRecv++
				intervalBytesRecv += uint64(len(data))
				audioEngine.HandleRemoteAudio(identity+loopbackIdentitySuffix, displayName+" (loopback)", loopStreamName, data)
				continue
			}

			peers.WithPeer(from, func(p *PeerState) {
				p.LastSeen = time.Now()
				p.EverReceivedMessage = true
				p.AudioRecvCount++
			})

			// Assign slot
			if len(data) >= 7 && data[0] == 'W' && data[1] == 'A' && data[2] == 'I' && data[3] == 'F' {
				streamID := binary.LittleEndian.Uint16(data[5:7])
				peers.AssignSlot(from, streamID)
			}

			// Track frame metrics and detect packet loss via sequence numbers.
			header := PeekWaifHeader(data)
			if header != nil {
				var loss *LossEvent
				var displayName string
				peers.WithPeer(from, func(p *PeerState) {
					loss = recordFrame(p, header)
					if p.DisplayName != nil {
						displayName = *p.DisplayName
					}
				})
				if loss != nil {
					shortID := from
					if len(shortID) > 8 {
						shortID = shortID[:8]
					}
					logWarn("packet_loss peer=%s name=%q stream=%d lost=%d expected_seq=%d got_seq=%d interval=%d",
						shortID, displayName, loss.StreamID, loss.Lost, loss.ExpectedSeq, loss.GotSeq, loss.IntervalIdx)
				}
			}

			audioIntervalsReceived++
			audioBytesRecv += uint64(len(data))
			intervalFramesRecv++
			intervalBytesRecv += uint64(len(data))

			if recorder != nil {
				var name *string
				peers.WithPeer(from, func(p *PeerState) { name = p.DisplayName })
				recorder.RecordPeer(from, name, data)
			}

			// Link Audio playback: hand the frame to the engine, keyed on the
			// sender's persistent identity. The room index already rides the frame
			// (relay clock), so there is no per-peer remap. streamName (from the
			// StreamNames sync) labels the republished channel "{peer} · {stream}".
			var identity, name, streamName string
			if header != nil {
				peers.WithPeer(from, func(p *PeerState) {
					if p.Identity != nil {
						identity = *p.Identity
					}
					if p.DisplayName != nil {
						name = *p.DisplayName
					}
					streamName = p.StreamNames[header.StreamID]
				})
			}
			if identity == "" {
				identity = from
			}
			audioEngine.HandleRemoteAudio(identity, name, streamName, data)

		// --- Audio from in-app senders (test tone / WAV) → relay ---
		case wireData := <-localWaifCh:
			if len(wireData) >= 7 && wireData[0] == 'W' && wireData[1] == 'A' {
				streamID := binary.LittleEndian.Uint16(wireData[5:7])
				localSendActive[streamID] = true
			}
			mesh.BroadcastAudio(wireData)
			audioBytesSent += uint64(len(wireData))
			audioIntervalsSent++
			intervalBytesSent += uint64(len(wireData))
			intervalFramesSent++
			if !loggedFirstFrameSent {
				loggedFirstFrameSent = true
				logInfo("audio: first WAIF frame sent (%d bytes)", len(wireData))
			}

		// --- Link events ---
		case ev := <-linkEventCh:
			switch ev.Type {
			case "TempoChanged":
				if math.Abs(ev.BPM-currentBPM) > 0.01 {
					// An observed DAW change that survived the detector's
					// de-noising: declared on the musician's behalf (ADR-0009).
					// declareTempo dual-sends the legacy TempoChange with the
					// ADOPTED room quantum (ADR-0004: the relay treats its
					// quantum as authoritative — a joiner's own would flap it).
					logInfo("Local tempo changed to %.1f BPM (declaring)", ev.BPM)
					declareTempo(ev.BPM)
					currentBPM = ev.BPM
					emitter.Emit("tempo:changed", TempoChangedEvent{BPM: ev.BPM, Source: "local"})
				}
				handleBoundary(ev.Beat)

			case "StateUpdate":
				mesh.Broadcast(NewStateSnapshot(ev.BPM, ev.Beat, ev.Phase, ev.Quantum, ev.TimestampUs))
				handleBoundary(ev.Beat)
			}

		// --- Grid-jump observability. A Link session merge or transport reset
		// moves the local beat timeline bodily; the engine detects and
		// attributes it. Nothing corrects it anymore (ADR-0009: playout
		// re-quantizes every round onto the local grid, wherever it sits), but
		// a musician whose bar lines just moved deserves the explanation, and a
		// merge hits every peer on that LAN — so the cause goes to the relay
		// log too, not just whichever machine noticed.
		case <-gridJumpTicker.C:
			if jump, jumped := audioEngine.TakeGridJump(); jumped {
				msg := fmt.Sprintf("grid jumped %+.2f beats (%+.0f ms, ≈%+d intervals) — cause: %s; %s",
					jump.Beats, jump.Ms, jump.Intervals, jump.Cause, jump.Detail)
				logWarn("%s", msg)
				mesh.SendLog("warn", "align", msg, uint64(time.Now().UnixMicro()))
			}

		// --- Ping timer ---
		case <-pingTicker.C:
			ping := clock.MakePing()
			mesh.Broadcast(ping)

		// --- Liveness watchdog ---
		case <-livenessTicker.C:
			for _, deadID := range peers.TimedOutPeers(30 * time.Second) {
				var name, deadIdentity string
				peers.WithPeer(deadID, func(p *PeerState) {
					if p.DisplayName != nil {
						name = *p.DisplayName
					}
					if p.Identity != nil {
						deadIdentity = *p.Identity
					}
				})
				if name == "" {
					name = deadID
				}
				if deadIdentity == "" {
					deadIdentity = deadID
				}
				logWarn("Peer %s timed out", name)
				audioEngine.DropPeer(deadIdentity)
				if deadIdentity != deadID {
					audioEngine.DropPeer(deadID) // late frames fall back to the peer id
				}
				peers.Remove(deadID)
				mesh.RemovePeer(deadID)
				emitter.Emit("peer:left", PeerLeftEvent{PeerID: deadID})
			}

			// Hello completion watchdog
			softPeers, hardPeers := peers.NoIdentityActivePeers(5*time.Second, 15*time.Second)
			helloMsg := NewHello(peerID, &displayName, &identity)
			for _, pid := range softPeers {
				logWarn("Peer %s active but Hello not received — re-sending", pid)
				mesh.SendTo(pid, helloMsg)
				peers.MarkHelloRetrySent(pid)
			}
			for _, pid := range hardPeers {
				logWarn("Peer %s no identity after 15s — removing", pid)
				// No Hello ever arrived, so anything they published is keyed by
				// their peer id (the identity fallback on the audio path).
				audioEngine.DropPeer(pid)
				peers.Remove(pid)
				mesh.RemovePeer(pid)
				emitter.Emit("peer:left", PeerLeftEvent{PeerID: pid})
			}

		// --- Status update ---
		case <-statusTicker.C:
			stateCh := make(chan LinkState, 1)
			linkCmdCh <- LinkCommand{Type: "GetState", StateCh: stateCh}
			state, ok := <-stateCh
			if !ok {
				continue
			}

			connected := mesh.ConnectedPeers()
			dcOpen := mesh.AnyPeersConnected()

			// Build peer infos
			peerInfos := make([]PeerInfo, 0, len(connected))
			for _, p := range connected {
				var dn *string
				var recvNow, recvPrev uint64
				peers.WithPeer(p, func(ps *PeerState) {
					dn = ps.DisplayName
					recvNow = ps.AudioRecvCount
					recvPrev = ps.AudioRecvPrev
				})
				isRecv := recvNow > recvPrev
				isSend := dcOpen && mesh.IsPeerConnected(p)
				status := peers.DeriveStatus(p)
				var rttMs *float64
				if rtt := clock.RTTUs(p); rtt != nil {
					v := float64(*rtt) / 1000.0
					rttMs = &v
				}
				var slot *uint32
				if s := peers.SlotFor(p, 0); s >= 0 {
					v := uint32(s + 1)
					slot = &v
				}
				peerInfos = append(peerInfos, PeerInfo{
					PeerID: p, DisplayName: dn, RTTMs: rttMs, Slot: slot,
					Status: status, IsSending: isSend, IsReceiving: isRecv,
				})
			}

			// Build slot infos
			mappings := peers.ActiveMappings()
			slotInfos := make([]SlotInfo, 0, len(mappings))
			for _, m := range mappings {
				var dn *string
				var isSend, isRecv bool
				var streamName *string
				pid, found := peers.FindByIdentity(m.ClientID)
				if found {
					peers.WithPeer(pid, func(ps *PeerState) {
						dn = ps.DisplayName
						isRecv = ps.AudioRecvCount > ps.AudioRecvPrev
						if n, ok := ps.StreamNames[m.ChannelIndex]; ok {
							streamName = &n
						}
					})
					isSend = dcOpen && mesh.IsPeerConnected(pid)
				}
				status := "connecting"
				if found {
					status = peers.DeriveStatus(pid)
				}
				var rttMs *float64
				if found {
					if rtt := clock.RTTUs(pid); rtt != nil {
						v := float64(*rtt) / 1000.0
						rttMs = &v
					}
				}
				slotInfos = append(slotInfos, SlotInfo{
					Slot: uint32(m.SlotIndex + 1), ShortID: m.ShortID(), ClientID: m.ClientID,
					PeerID:       pid,
					ChannelIndex: m.ChannelIndex, DisplayName: dn, Status: &status,
					RTTMs: rttMs, IsSending: isSend, IsReceiving: isRecv, StreamName: streamName,
				})
			}

			// Build local sends: in-app senders (test tone / WAV) plus every
			// enabled capture channel — the unified peers tree renders both in
			// its "you" node. Capture sends carry their effective (override-aware)
			// names so the tree matches what receivers see.
			captureInfos := audioEngine.CaptureChannels()
			effNames := effectiveStreamNames(captureInfos, localStreamNames)
			localSends := make([]LocalSendInfo, 0, len(localSendStreams)+len(captureInfos))
			for streamIdx := range localSendStreams {
				var sn *string
				if n, ok := localStreamNames[streamIdx]; ok {
					sn = &n
				}
				localSends = append(localSends, LocalSendInfo{
					StreamIndex: streamIdx,
					IsSending:   localSendActive[streamIdx],
					StreamName:  sn,
				})
			}
			for _, cc := range captureInfos {
				if !cc.Enabled {
					continue
				}
				n := effNames[cc.StreamID]
				localSends = append(localSends, LocalSendInfo{
					StreamIndex: cc.StreamID,
					IsSending:   true, // enabled = bridged = sending
					StreamName:  &n,
				})
			}
			sort.Slice(localSends, func(i, j int) bool { return localSends[i].StreamIndex < localSends[j].StreamIndex })
			localSendActive = make(map[uint16]bool)

			peers.FlushAudioRecvPrev()

			// Capture discovery changes (channels appearing, toggling, renames)
			// don't raise commands; refresh the advertised stream names here.
			syncStreamNames()

			// Relay RTT readout for the debug panel (grid alignment and its
			// badge are retired, ADR-0009).
			var relayRttMs *float64
			if rtt, rok := clock.RelayRTTUs(); rok {
				rv := float64(rtt) / 1000.0
				relayRttMs = &rv
			}

			// Engine health snapshot (also carries the debug-room stream offsets).
			// Fetched before the emit so the diff log follows in the same tick.
			health := audioEngine.Health()

			emitter.Emit("status:update", StatusUpdate{
				BPM: state.BPM, Beat: state.Beat, Phase: state.Phase,
				LinkPeers: state.NumPeers, Peers: peerInfos, Slots: slotInfos,
				LocalSends: localSends, IntervalBars: intervalCfg.Bars,
				IntervalQuantum: intervalCfg.Quantum,
				AudioSent:       audioIntervalsSent, AudioRecv: audioIntervalsReceived,
				AudioBytesSent: audioBytesSent, AudioBytesRecv: audioBytesRecv,
				AudioDCOpen: dcOpen, PluginConnected: true,
				RelayRTTMs:    relayRttMs,
				StreamOffsets: health.StreamOffsets,
				Recording:     recorder != nil,
				RecordingSizeBytes: func() uint64 {
					if recorder != nil {
						return recorder.BytesWritten()
					}
					return 0
				}(),
				CaptureChannels: audioEngine.CaptureChannels(),
			})

			// Broadcast audio status
			audioStatusSeq++
			mesh.Broadcast(NewAudioStatus(dcOpen, audioIntervalsSent, audioIntervalsReceived, true, audioStatusSeq))

			// Engine health: log any counter that moved (each increment is a
			// likely-audible event). StreamOffsets is a slice — compare the
			// snapshot with it zeroed out (offsets are informational only).
			hcNow, hcPrev := health, lastHealth
			hcNow.StreamOffsets, hcPrev.StreamOffsets = nil, nil
			if !reflect.DeepEqual(hcNow, hcPrev) {
				delta := func(logf func(string, ...any), what string, prev, now uint64) {
					if now > prev {
						logf("[audio] %s +%d (total %d)", what, now-prev, now)
					}
				}
				delta(logWarn, "capture ring dropped buffers (drain stalled — audible gap)", lastHealth.CaptureRingDropped, health.CaptureRingDropped)
				delta(logWarn, "capture LAN loss: buffers lost on the Link Audio hop", lastHealth.CaptureLANLostBuffers, health.CaptureLANLostBuffers)
				delta(logWarn, "capture LAN loss events", lastHealth.CaptureLANGapEvents, health.CaptureLANGapEvents)
				delta(logWarn, "capture re-anchor (stamp discontinuity — audible splice)", lastHealth.CaptureResnaps, health.CaptureResnaps)
				delta(logInfo, "capture drift micro-slew frames (inaudible correction)", lastHealth.CaptureSlews, health.CaptureSlews)
				delta(logWarn, "capture dropped late buffers", lastHealth.CaptureDroppedLate, health.CaptureDroppedLate)
				delta(logWarn, "capture dropped backfill buffers", lastHealth.CaptureDroppedBackfill, health.CaptureDroppedBackfill)
				delta(logWarn, "sink underrun: paced audio late past the cushion (audible dropout)", lastHealth.EmitSinkUnderrunEvents, health.EmitSinkUnderrunEvents)
				delta(logWarn, "sink write rejected mid-stream (chunk lost — audible hole for listeners)", lastHealth.EmitSinkWriteRejected, health.EmitSinkWriteRejected)
				delta(logWarn, "frames never arrived by playout retirement (played as silence)", lastHealth.EmitFramesMissingAtPlay, health.EmitFramesMissingAtPlay)
				delta(logWarn, "frames dropped as too-late (sender labels behind our room clock — anchor mismatch)", lastHealth.EmitFramesTooLate, health.EmitFramesTooLate)
				delta(logInfo, "frames concealed by Opus PLC (masked loss)", lastHealth.EmitFramesConcealed, health.EmitFramesConcealed)
				// EmitIntervalsIncomplete is deliberately not logged: it fires once
				// per interval in normal operation and never aligned with an audible
				// glitch in the field. Still counted and shipped in the health snapshot.
				delta(logWarn, "WAIF wire decode failures", lastHealth.WireDecodeFailures, health.WireDecodeFailures)
				delta(logWarn, "Opus decode failures", lastHealth.OpusDecodeFailures, health.OpusDecodeFailures)
				lastHealth = health
			}

			// Send metrics + build network event
			perPeer := make(map[string]PeerFrameReport)
			networkInfos := make([]PeerNetworkInfo, 0, len(connected)+1)
			for _, p := range connected {
				var fr, lost, lossEvents, reorderEvents, audioRecv uint64
				peers.WithPeer(p, func(ps *PeerState) {
					fr = ps.TotalFramesReceived
					lost = ps.PacketsLost
					lossEvents = ps.LossEvents
					reorderEvents = ps.ReorderEvents
					audioRecv = ps.AudioRecvCount
				})
				perPeer[p] = PeerFrameReport{
					FramesExpected: fr + lost, FramesReceived: fr,
					RTTUs: clock.RTTUs(p), JitterUs: clock.JitterUs(p),
					LateFrames: reorderEvents,
				}
				var dn *string
				var rttMs *float64
				var slot *uint32
				for _, pi := range peerInfos {
					if pi.PeerID == p {
						dn = pi.DisplayName
						rttMs = pi.RTTMs
						slot = pi.Slot
						break
					}
				}
				status := peers.DeriveStatus(p)
				networkInfos = append(networkInfos, PeerNetworkInfo{
					PeerID: p, DisplayName: dn, Slot: slot,
					ConnectionState: status,
					RTTMs:           rttMs, AudioRecv: audioRecv,
					FramesReceived: fr, PacketsLost: lost, LossEvents: lossEvents,
				})
			}
			// The loopback echo is not a peer, but its receive/loss stats are —
			// show them as their own row while it has seen traffic.
			if loopbackState.TotalFramesReceived > 0 {
				name := displayName + " (loopback)"
				networkInfos = append(networkInfos, PeerNetworkInfo{
					PeerID: "loopback", DisplayName: &name,
					ConnectionState: "loopback",
					AudioRecv:       loopbackState.TotalFramesReceived,
					FramesReceived:  loopbackState.TotalFramesReceived,
					PacketsLost:     loopbackState.PacketsLost,
					LossEvents:      loopbackState.LossEvents,
				})
			}
			emitter.Emit("peers:network", PeersNetwork{Peers: networkInfos, Health: health})
			mesh.SendMetricsReport(dcOpen, true, perPeer, localDropCount.Load(), boundaryDriftUs)

			// Keep the relay's rate limit honest about how many streams we're
			// actually sending (computed from ground truth, so engine-side
			// restore auto-enables are covered too).
			captureEnabled := 0
			for _, cc := range audioEngine.CaptureChannels() {
				if cc.Enabled {
					captureEnabled++
				}
			}
			sendStreams := activeSendStreamCount(captureEnabled,
				testToneStream != nil, wavSenderStream != nil, metronomeSendCancelFn != nil)
			if shouldDeclareStreams(sendStreams, lastDeclaredStreams, streamUpdateRejected, streamUpdateRetryAt, time.Now()) {
				logInfo("[signaling] declaring %d send streams to relay (update_streams, was %d)", sendStreams, lastDeclaredStreams)
				mesh.SendUpdateStreams(uint16(sendStreams))
				lastDeclaredStreams = sendStreams
				streamUpdateRejected = false
			}
		}
	}

cleanup:
	if recorder != nil {
		recorder.Finalize()
		logInfo("Recording finalized")
	}
	return nil
}

func connectMesh(ctx context.Context, config SessionConfig, peerID string) (*PeerMesh, <-chan FromPeerSync, <-chan FromPeerAudio, error) {
	client, channels, peerNames, err := ConnectSignaling(
		ctx, config.Server, config.Room, peerID,
		config.Password, config.StreamCount, &config.DisplayName,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	mesh := NewPeerMesh(peerID, client, channels, config.StreamCount, peerNames)
	return mesh, channels.SyncCh, channels.AudioCh, nil
}

// streamUpdateRetryBackoff is how long the session waits before re-declaring
// its stream count after the relay rejects an update (room_full) — long
// enough not to spam a full room, short enough to heal when slots free up.
const streamUpdateRetryBackoff = 30 * time.Second

// shouldDeclareStreams reports whether an update_streams declaration is due
// this tick: the desired count drifted from the last declaration (always
// immediate), or the relay rejected the previous declaration and the retry
// backoff has elapsed.
func shouldDeclareStreams(desired, lastDeclared int, rejected bool, retryAt, now time.Time) bool {
	if desired != lastDeclared {
		return true
	}
	return rejected && !now.Before(retryAt)
}

// activeSendStreamCount computes how many concurrent WAIF audio streams this
// peer is (or will be) sending to the relay: enabled Link Audio capture
// channels plus any in-app senders (test tone, WAV, metronome broadcast).
// The relay scales its per-peer binary rate limit by the declared count, so
// it must stay honest or the relay drops frames (and eventually disconnects
// us). Minimum 1 — an idle peer still occupies one room slot.
func activeSendStreamCount(captureEnabled int, testTone, wavSender, metronome bool) int {
	n := captureEnabled
	for _, on := range []bool{testTone, wavSender, metronome} {
		if on {
			n++
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

func min64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func pow2(n uint32) uint64 {
	if n > 30 {
		n = 30
	}
	return 1 << n
}
