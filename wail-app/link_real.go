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

// ForceBeat snaps the local beat clock to the given position.
// rttUs compensates for one-way network transit time.
func (lb *LinkBridge) ForceBeat(beat float64, rttUs *int64) {
	lb.mu.Lock()
	t := lb.link.ClockMicros()
	lb.link.CaptureAppSessionState(lb.sessionState)
	bpm := lb.sessionState.Tempo()
	var compensation float64
	if rttUs != nil {
		compensation = float64(*rttUs) / 2_000_000.0 * bpm / 60.0
	}
	compensated := beat + compensation
	lb.sessionState.ForceBeatAtTime(compensated, t, lb.quantum)
	lb.link.CommitAppSessionState(lb.sessionState)
	lb.mu.Unlock()
	lb.detector.ArmEchoGuard(time.Now().Add(echoGuardDuration))
	log.Printf("[link] Forced beat to %.2f (compensated=%.2f, rtt=%v)", beat, compensated, rttUs)
}

// SnapGrid shifts the local interval grid earlier by deltaUs (positive = the
// local grid runs late vs the room grid): the current beat is force-mapped to
// now−delta at the interval quantum, so every grid boundary moves by exactly
// deltaUs. ADR-0006 entry conformance — consented, transition-moment only.
func (lb *LinkBridge) SnapGrid(deltaUs int64) {
	lb.mu.Lock()
	t := lb.link.ClockMicros()
	lb.link.CaptureAppSessionState(lb.sessionState)
	beat := lb.sessionState.BeatAtTime(t, lb.intervalQuantum)
	lb.sessionState.ForceBeatAtTime(beat, t-deltaUs, lb.intervalQuantum)
	lb.link.CommitAppSessionState(lb.sessionState)
	lb.mu.Unlock()
	lb.detector.ArmEchoGuard(time.Now().Add(echoGuardDuration))
	log.Printf("[link] grid snap: shifted interval grid %+.1f ms onto the room grid", float64(deltaUs)/1000)
}

// TimeAtBeat returns the Link-clock time at which the given beat occurs, at
// the interval quantum (the lens State().Beat is phase-encoded at).
func (lb *LinkBridge) TimeAtBeat(beat float64) int64 {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.link.CaptureAppSessionState(lb.sessionState)
	return lb.sessionState.TimeAtBeat(beat, lb.intervalQuantum)
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
