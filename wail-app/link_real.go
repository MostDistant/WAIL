//go:build !linkstub

package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
)

// LinkBridge wraps the Ableton Link session via our own abl_link cgo binding
// (internal/abllink), compiled directly against vendor/link. One abl_link handle
// carries both Link sync and Link Audio.
type LinkBridge struct {
	mu           sync.Mutex
	link         *abllink.Link
	sessionState *abllink.SessionState
	quantum      float64
	// intervalQuantum is the room BPI (bars × quantum). State().Beat is phase-
	// encoded at it: Link pins beat phase only mod the quantum asked for, and
	// interval math needs beat mod BPI pinned — the bar lens left which bar of
	// the interval per-peer arbitrary.
	intervalQuantum float64
	detector        *TempoChangeDetector
}

// NewLinkBridge creates a new Link bridge with the given initial BPM and quantum.
func NewLinkBridge(initialBPM, quantum float64) *LinkBridge {
	link := abllink.New(initialBPM)
	ss := abllink.NewSessionState()
	return &LinkBridge{
		link:            link,
		sessionState:    ss,
		quantum:         quantum,
		intervalQuantum: quantum,
		detector:        NewTempoChangeDetector(initialBPM),
	}
}

// Enable activates the Link session.
func (lb *LinkBridge) Enable() {
	lb.link.Enable(true)
	log.Printf("[link] Ableton Link enabled at %.1f BPM", lb.detector.LastTempo())
}

// Disable deactivates the Link session.
func (lb *LinkBridge) Disable() {
	lb.link.Enable(false)
	log.Printf("[link] Ableton Link disabled")
}

// SetTempo applies a remote tempo change to the local Link session.
func (lb *LinkBridge) SetTempo(bpm float64) {
	lb.mu.Lock()
	t := lb.link.ClockMicros()
	lb.link.CaptureAppSessionState(lb.sessionState)
	lb.sessionState.SetTempo(bpm, t)
	lb.link.CommitAppSessionState(lb.sessionState)
	lb.mu.Unlock()
	lb.detector.SetLastTempo(bpm)
	lb.detector.ArmEchoGuard(time.Now().Add(echoGuardDuration))
	// Neutral wording: EVERY SetTempo logs here — remote applies, slew
	// nudges/restores, and entry adoptions alike. Remote applies are already
	// attributed at session level ("Tempo change from <peer>"); claiming
	// "remote" here misattributes self-steering and has already caused one
	// wrong root-cause in the field (the 2026-07-25 "slew ping-pong"
	// misdiagnosis — the slew was exonerated by the logs).
	log.Printf("[link] Set tempo %.1f BPM", bpm)
}

// State returns the current Link state. Beat is phase-encoded at the interval
// quantum (BPI) so interval bucketing of it lands on the session-shared
// interval grid; Phase stays the within-bar lens.
func (lb *LinkBridge) State() LinkState {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	t := lb.link.ClockMicros()
	lb.link.CaptureAppSessionState(lb.sessionState)
	return LinkState{
		BPM:         lb.sessionState.Tempo(),
		Beat:        lb.sessionState.BeatAtTime(t, lb.intervalQuantum),
		Phase:       lb.sessionState.PhaseAtTime(t, lb.quantum),
		Quantum:     lb.quantum,
		TimestampUs: t,
		NumPeers:    lb.link.NumPeers(),
	}
}

// Detector returns the tempo change detector.
func (lb *LinkBridge) Detector() *TempoChangeDetector {
	return lb.detector
}

// Link returns the underlying abl_link handle so the Link Audio engine can
// create sources/sinks on the one shared peer (sync + audio share it).
func (lb *LinkBridge) Link() *abllink.Link {
	return lb.link
}

// EnableLinkAudio enables or disables Link Audio on the shared handle.
func (lb *LinkBridge) EnableLinkAudio(on bool) {
	lb.link.EnableLinkAudio(on)
}

// SetPeerName sets the Link Audio peer name used to label WAIL's channels.
func (lb *LinkBridge) SetPeerName(name string) {
	lb.link.SetPeerName(name)
}

// Quantum returns the Link session quantum.
func (lb *LinkBridge) Quantum() float64 {
	return lb.quantum
}

// SetIntervalQuantum updates the room BPI used as State().Beat's phase lens.
func (lb *LinkBridge) SetIntervalQuantum(q float64) {
	if q <= 0 {
		return
	}
	lb.mu.Lock()
	lb.intervalQuantum = q
	lb.mu.Unlock()
}

// SpawnPoller starts a polling goroutine that monitors the Link session.
func (lb *LinkBridge) SpawnPoller(ctx context.Context) (chan<- LinkCommand, <-chan LinkEvent) {
	return SpawnLinkPoller(ctx, lb)
}
