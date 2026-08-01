package main

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/interval"
)

const (
	// tempoSteadyBand is how tightly a candidate reading must hold during the
	// hold-down to count as steady. Tight on purpose: this asks "has it stopped
	// moving?", not "is it different enough to matter".
	tempoSteadyBand = 0.01 // BPM
	// tempoReportFloor is the lower bound on the reporting bar, for tempos slow
	// enough that the slew's authority shrinks below anything meaningful (at
	// 60 BPM the authority is 0.03). The bar itself is the slew's authority —
	// see reportBar.
	tempoReportFloor = 0.02 // BPM
	// tempoIntegerSnap pulls a reported tempo onto a whole number when it is
	// this close to one. The room tempo is the reference every peer's grid
	// math derives from, so it should carry the intended value rather than
	// whichever fraction the first reporter happened to sample. Deliberately
	// narrow: 128.5 is someone's actual project tempo and must survive.
	tempoIntegerSnap      = 0.1 // BPM
	echoGuardDuration     = 150 * time.Millisecond
	linkPollInterval      = 20 * time.Millisecond // 50 Hz
	snapshotIntervalTicks = 10                    // ~200ms at 50Hz
	// tempoHoldDown is how long a detected local tempo change must hold
	// before it is reported as user intent. Link session convergence nudges
	// (join merges, phase re-lock — up to ±2%) are transient, lasting a few
	// polls each; a human's knob turn persists. Without this, convergence
	// noise is broadcast to the room as tempo changes (field finding:
	// a transient 119.6 re-anchored a 122 room; two insistent LANs then
	// fed a 120↔122 flap at the 200ms snapshot cadence).
	tempoHoldDown = 1 * time.Second
	// tempoMeanWindow is how far back the running mean of the observed session
	// tempo reaches. The mean, not the instantaneous reading, is what the grid
	// steer's tempo-settling gate judges: a clock wandering 119.9↔120 averages
	// to 119.95, which is inside the slew's authority, so the slew keeps
	// correcting drift straight through the wobble instead of going dormant on
	// every excursion (the failure sameRateBand's own comment described).
	tempoMeanWindow = 4 * time.Second
	// tempoMeanRegime is the divergence above which the instantaneous reading is
	// believed directly instead of the mean. Averaging costs its window in
	// detection lag, and lag is phase: a 10 BPM change judged on a 4s mean leaks
	// 417ms before the room hears it. Below this bar the divergence is small
	// enough that noise is a live explanation and averaging is worth its lag;
	// above it, nothing observed in the field is a candidate — the wobble that
	// motivated all of this was 0.1 BPM, and half a BPM at 120 is 4167ppm, which
	// is a peer at a different tempo rather than a clock wandering.
	tempoMeanRegime = 0.5 // BPM
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

// tempoDetectorConfig is the detector's tuning, split out from the constants so
// a simulation can sweep it (ADR-0009: these are measured, not chosen — and the
// pre-#499 bar has to stay reachable, since reproducing the wobble bug is how
// the simulation proves it models reality). Zero fields take the package
// default, so production constructs it empty and behaves exactly as before.
type tempoDetectorConfig struct {
	reportFloor float64
	// reportBarFixed pins the reporting bar to a flat value instead of deriving
	// it from the slew's authority. Only historical replays use it — the
	// simulation reproduces the pre-#499 wobble bug by setting it to 0.01.
	reportBarFixed float64
	steadyBand     float64
	integerSnap    float64
	holdDown       time.Duration
	meanWindow     time.Duration
	meanRegime     float64
	// noIntegerSnap turns the snap off outright, which a zero integerSnap
	// cannot express (zero means "use the default"). Only the pre-#499 replay
	// needs it: the snap and the raised bar shipped together.
	noIntegerSnap bool
}

func (c tempoDetectorConfig) withDefaults() tempoDetectorConfig {
	if c.reportFloor <= 0 {
		c.reportFloor = tempoReportFloor
	}
	if c.steadyBand <= 0 {
		c.steadyBand = tempoSteadyBand
	}
	if c.noIntegerSnap {
		c.integerSnap = 0
	} else if c.integerSnap <= 0 {
		c.integerSnap = tempoIntegerSnap
	}
	if c.holdDown <= 0 {
		c.holdDown = tempoHoldDown
	}
	if c.meanWindow <= 0 {
		c.meanWindow = tempoMeanWindow
	}
	if c.meanRegime <= 0 {
		c.meanRegime = tempoMeanRegime
	}
	return c
}

// TempoChangeDetector is a pure-logic tempo change detector with echo guard
// and hold-down. Extracted so it can be tested without the Link C FFI.
type TempoChangeDetector struct {
	mu             sync.Mutex
	cfg            tempoDetectorConfig
	lastTempo      float64
	echoGuardUntil *time.Time
	// Hold-down state: a changed reading is a candidate until it holds
	// continuously for tempoHoldDown; only then is it reported.
	candidate      float64
	candidateSince time.Time
	hasCandidate   bool
	// Running mean of every observed reading over cfg.meanWindow. Kept as a
	// timestamped FIFO rather than a fixed-length ring so it stays correct if
	// the poll cadence ever changes; the sum is recomputed each call (a few
	// hundred adds) so it cannot drift over an hour-long session.
	samples []tempoSample
}

type tempoSample struct {
	at  time.Time
	bpm float64
}

// NewTempoChangeDetector creates a new detector with the given initial tempo.
func NewTempoChangeDetector(initialTempo float64) *TempoChangeDetector {
	return newTunedTempoChangeDetector(initialTempo, tempoDetectorConfig{})
}

// newTunedTempoChangeDetector creates a detector with non-default tuning, for
// simulations that sweep the thresholds.
func newTunedTempoChangeDetector(initialTempo float64, cfg tempoDetectorConfig) *TempoChangeDetector {
	return &TempoChangeDetector{lastTempo: initialTempo, cfg: cfg.withDefaults()}
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
	// Observe first, unconditionally: the mean is a record of what the session
	// did, and the guards below decide what to make of it, not whether it
	// happened. Skipping samples during an echo guard would leave the mean
	// blind to exactly the moments around our own writes.
	d.observe(bpm, now)

	if d.echoGuardUntil != nil {
		if now.Before(*d.echoGuardUntil) {
			return 0, false
		}
		d.echoGuardUntil = nil
	}

	// Small divergence is judged on the windowed mean, large divergence on the
	// reading itself. A clock wandering 119.9↔120 has a mean of 119.95, inside
	// the slew's authority and so nothing the room needs to hear, while every
	// individual excursion is outside it — magnitude alone cannot tell that
	// wobble from a deliberate 0.1 nudge, because they are the same size, and
	// the mean can (ADR-0009). But averaging costs its window in lag, and lag is
	// phase, so it is spent only where noise is a live explanation.
	observed := bpm
	if math.Abs(bpm-d.lastTempo) <= d.cfg.meanRegime {
		observed = d.meanLocked()
	}
	if math.Abs(observed-d.lastTempo) <= d.reportBar() {
		// Inside what the slew can hold: not a change the room needs told.
		d.hasCandidate = false
		return 0, false
	}

	// Hold-down: the mean is a candidate until it stops moving. This is what
	// keeps a ramp (Link converging on a merge) from being reported while it is
	// still in progress — only its settled value is intent.
	if !d.hasCandidate || math.Abs(observed-d.candidate) > d.cfg.steadyBand {
		d.candidate = observed
		d.candidateSince = now
		d.hasCandidate = true
		return 0, false
	}
	if now.Sub(d.candidateSince) < d.cfg.holdDown {
		return 0, false
	}
	reported := snapToIntegerTempo(observed, d.cfg.integerSnap)
	d.lastTempo = reported
	d.hasCandidate = false
	return reported, true
}

// snapToIntegerTempo rounds a tempo onto a whole number when it is within
// radius of one — recovering the value a human typed from the value Link
// reported. Anything further out is left exactly as observed.
func snapToIntegerTempo(bpm, radius float64) float64 {
	if r := math.Round(bpm); math.Abs(bpm-r) <= radius && r > 0 {
		return r
	}
	return bpm
}

// reportBar is how far the mean must sit from the last reported tempo to count
// as a change worth telling the room: exactly the slew's authority at that
// tempo (0.06 BPM at 120). The slew silently holds everything inside it, so the
// room only needs to hear what the slew cannot — the two tile with no band
// between them that is neither reported nor correctable, which is the fault
// ADR-0009 exists to fix. Caller holds the lock.
func (d *TempoChangeDetector) reportBar() float64 {
	if d.cfg.reportBarFixed > 0 {
		return d.cfg.reportBarFixed
	}
	return math.Max(d.cfg.reportFloor, interval.SlewAuthorityBPM(d.lastTempo))
}

// meanLocked returns the running mean; caller holds the lock.
func (d *TempoChangeDetector) meanLocked() float64 {
	if len(d.samples) == 0 {
		return d.lastTempo
	}
	var sum float64
	for _, s := range d.samples {
		sum += s.bpm
	}
	return sum / float64(len(d.samples))
}

// observe folds one reading into the running mean, dropping anything older than
// the window. Caller holds the lock.
func (d *TempoChangeDetector) observe(bpm float64, now time.Time) {
	d.samples = append(d.samples, tempoSample{at: now, bpm: bpm})
	cut := now.Add(-d.cfg.meanWindow)
	drop := 0
	for drop < len(d.samples) && d.samples[drop].at.Before(cut) {
		drop++
	}
	d.samples = d.samples[drop:]
}

// MeanTempo returns the mean observed session tempo over the window, or the
// last reported tempo when nothing has been observed yet. This is what the grid
// steer gates on: a wobble's mean sits inside the slew's authority even when
// every individual excursion does not.
func (d *TempoChangeDetector) MeanTempo() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.meanLocked()
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
