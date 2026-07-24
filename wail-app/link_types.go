package main

import (
	"context"
	"math"
	"sync"
	"time"
)

const (
	tempoChangeThreshold  = 0.01 // BPM
	echoGuardDuration     = 150 * time.Millisecond
	linkPollInterval      = 20 * time.Millisecond // 50 Hz
	snapshotIntervalTicks = 10                     // ~200ms at 50Hz
	// tempoHoldDown is how long a detected local tempo change must hold
	// before it is reported as user intent. Link session convergence nudges
	// (join merges, phase re-lock — up to ±2%) are transient, lasting a few
	// polls each; a human's knob turn persists. Without this, convergence
	// noise is broadcast to the room as tempo changes (field finding:
	// a transient 119.6 re-anchored a 122 room; two insistent LANs then
	// fed a 120↔122 flap at the 200ms snapshot cadence).
	tempoHoldDown = 1 * time.Second
)

// LinkEvent represents events emitted by the Link bridge.
type LinkEvent struct {
	Type        string // "TempoChanged" or "StateUpdate"
	BPM         float64
	Beat        float64
	Phase       float64
	Quantum     float64
	TimestampUs int64
}

// LinkCommand represents commands sent to the Link bridge.
type LinkCommand struct {
	Type    string // "SetTempo", "ForceBeat", "GetState"
	BPM     float64
	Beat    float64
	RTTUs   *int64
	StateCh chan LinkState // for GetState
}

// LinkState is a snapshot of the current Link session state.
type LinkState struct {
	BPM         float64
	Beat        float64
	Phase       float64
	Quantum     float64
	TimestampUs int64
	NumPeers    uint64
}

// TempoChangeDetector is a pure-logic tempo change detector with echo guard
// and hold-down. Extracted so it can be tested without the Link C FFI.
type TempoChangeDetector struct {
	mu             sync.Mutex
	lastTempo      float64
	echoGuardUntil *time.Time
	// Hold-down state: a changed reading is a candidate until it holds
	// continuously for tempoHoldDown; only then is it reported.
	candidate      float64
	candidateSince time.Time
	hasCandidate   bool
}

// NewTempoChangeDetector creates a new detector with the given initial tempo.
func NewTempoChangeDetector(initialTempo float64) *TempoChangeDetector {
	return &TempoChangeDetector{lastTempo: initialTempo}
}

// ArmEchoGuard sets the echo guard expiry (called after applying a remote tempo change).
func (d *TempoChangeDetector) ArmEchoGuard(until time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.echoGuardUntil = &until
	d.hasCandidate = false
}

// Check determines if a tempo reading is a reportable change.
// Returns the BPM if a change exceeds threshold, has held for tempoHoldDown,
// and the echo guard is not active; otherwise 0.
func (d *TempoChangeDetector) Check(bpm float64, now time.Time) (float64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if math.IsNaN(bpm) || math.IsInf(bpm, 0) || bpm <= 0.0 {
		return 0, false
	}

	if d.echoGuardUntil != nil {
		if now.Before(*d.echoGuardUntil) {
			return 0, false
		}
		d.echoGuardUntil = nil
	}

	if math.Abs(bpm-d.lastTempo) <= tempoChangeThreshold {
		// Settled at (or back to) the reported tempo: nothing pending.
		d.hasCandidate = false
		return 0, false
	}

	// Hold-down: the reading is a candidate until it holds for tempoHoldDown.
	if !d.hasCandidate || math.Abs(bpm-d.candidate) > tempoChangeThreshold {
		d.candidate = bpm
		d.candidateSince = now
		d.hasCandidate = true
		return 0, false
	}
	if now.Sub(d.candidateSince) < tempoHoldDown {
		return 0, false
	}
	d.lastTempo = bpm
	d.hasCandidate = false
	return bpm, true
}

// LastTempo returns the last known tempo.
func (d *TempoChangeDetector) LastTempo() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastTempo
}

// SetLastTempo updates the baseline tempo. Rejects NaN, zero, and negative values.
func (d *TempoChangeDetector) SetLastTempo(bpm float64) {
	if math.IsNaN(bpm) || math.IsInf(bpm, 0) || bpm <= 0.0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastTempo = bpm
	d.hasCandidate = false
}

// SpawnLinkPoller starts a polling goroutine shared by both stub and real implementations.
func SpawnLinkPoller(ctx context.Context, lb LinkBridgeInterface) (chan<- LinkCommand, <-chan LinkEvent) {
	cmdCh := make(chan LinkCommand, 64)
	eventCh := make(chan LinkEvent, 64)

	go func() {
		ticker := time.NewTicker(linkPollInterval)
		defer ticker.Stop()
		var snapshotCounter uint32

		for {
			select {
			case <-ctx.Done():
				return
			case cmd := <-cmdCh:
				switch cmd.Type {
				case "SetTempo":
					lb.SetTempo(cmd.BPM)
				case "ForceBeat":
					lb.ForceBeat(cmd.Beat, cmd.RTTUs)
				case "GetState":
					if cmd.StateCh != nil {
						cmd.StateCh <- lb.State()
					}
				}
			case <-ticker.C:
				state := lb.State()
				if bpm, changed := lb.Detector().Check(state.BPM, time.Now()); changed {
					select {
					case eventCh <- LinkEvent{
						Type: "TempoChanged", BPM: bpm,
						Beat: state.Beat, TimestampUs: state.TimestampUs,
					}:
					default:
					}
				}

				snapshotCounter++
				if snapshotCounter >= snapshotIntervalTicks {
					snapshotCounter = 0
					select {
					case eventCh <- LinkEvent{
						Type: "StateUpdate", BPM: state.BPM,
						Beat: state.Beat, Phase: state.Phase,
						Quantum: state.Quantum, TimestampUs: state.TimestampUs,
					}:
					default:
					}
				}
			}
		}
	}()

	return cmdCh, eventCh
}

// LinkBridgeInterface defines the methods needed by the poller.
type LinkBridgeInterface interface {
	Enable()
	Disable()
	SetTempo(bpm float64)
	ForceBeat(beat float64, rttUs *int64)
	State() LinkState
	Detector() *TempoChangeDetector
	SpawnPoller(ctx context.Context) (chan<- LinkCommand, <-chan LinkEvent)
	// SnapGrid shifts the local interval grid earlier by deltaUs (positive =
	// local grid runs late vs the room grid). ADR-0006 entry conformance;
	// confined to join/rejoin, never steady state.
	SnapGrid(deltaUs int64)
	// TimeAtBeat returns the Link-clock time at which the given
	// interval-quantum phase-encoded beat occurs (grid boundary math).
	TimeAtBeat(beat float64) int64
}
