//go:build linkstub

package main

import (
	"context"
	"log"
	"math"
	"sync"
	"time"
)

// LinkBridge stub implementation that simulates Link behavior without the C++ SDK.
// Build with -tags=linkstub to use this instead of the real abletonlink-go bridge.
type LinkBridge struct {
	mu      sync.Mutex
	bpm     float64
	quantum float64
	// intervalQuantum mirrors the real bridge's BPI phase lens for State().Beat.
	// The stub's beat free-runs from origin 0, which is already zero-phase at
	// every quantum, so only the API needs mirroring.
	intervalQuantum float64
	beat            float64
	enabled         bool
	startTime       time.Time
	detector        *TempoChangeDetector
}

func NewLinkBridge(initialBPM, quantum float64) *LinkBridge {
	return &LinkBridge{
		bpm:             initialBPM,
		quantum:         quantum,
		intervalQuantum: quantum,
		startTime:       time.Now(),
		detector:        NewTempoChangeDetector(initialBPM),
	}
}

func (lb *LinkBridge) Enable() {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.enabled = true
	log.Printf("[link-stub] Ableton Link enabled at %.1f BPM", lb.bpm)
}

func (lb *LinkBridge) Disable() {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.enabled = false
	log.Printf("[link-stub] Ableton Link disabled")
}

func (lb *LinkBridge) SetTempo(bpm float64) {
	lb.mu.Lock()
	// Snapshot current beat before changing BPM to avoid retroactive recalculation
	elapsed := time.Since(lb.startTime).Seconds()
	lb.beat += elapsed * lb.bpm / 60.0
	lb.startTime = time.Now()
	lb.bpm = bpm
	lb.mu.Unlock()
	lb.detector.SetLastTempo(bpm)
	lb.detector.ArmEchoGuard(time.Now().Add(echoGuardDuration))
}

func (lb *LinkBridge) ForceBeat(beat float64, rttUs *int64) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	var compensation float64
	if rttUs != nil {
		compensation = float64(*rttUs) / 2_000_000.0 * lb.bpm / 60.0
	}
	lb.beat = beat + compensation
	lb.startTime = time.Now()
	lb.detector.ArmEchoGuard(time.Now().Add(echoGuardDuration))
	log.Printf("[link-stub] Forced beat to %.2f (compensated=%.2f)", beat, lb.beat)
}

func (lb *LinkBridge) State() LinkState {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	elapsed := time.Since(lb.startTime).Seconds()
	beatsElapsed := elapsed * lb.bpm / 60.0
	beat := lb.beat + beatsElapsed
	phase := math.Mod(beat, lb.quantum)
	if phase < 0 {
		phase += lb.quantum
	}
	return LinkState{
		BPM:         lb.bpm,
		Beat:        beat,
		Phase:       phase,
		Quantum:     lb.quantum,
		TimestampUs: time.Since(lb.startTime).Microseconds(),
		NumPeers:    0,
	}
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

func (lb *LinkBridge) Detector() *TempoChangeDetector {
	return lb.detector
}

// SnapGrid shifts the stub grid earlier by deltaUs (beat equivalent at the
// current tempo), mirroring the real bridge's entry-conformance snap.
func (lb *LinkBridge) SnapGrid(deltaUs int64) {
	lb.mu.Lock()
	lb.beat += float64(deltaUs) / 1e6 * lb.bpm / 60.0
	lb.mu.Unlock()
	lb.detector.ArmEchoGuard(time.Now().Add(echoGuardDuration))
	log.Printf("[link-stub] grid snap: shifted %+.1f ms", float64(deltaUs)/1000)
}

// TimeAtBeat converts a beat to the stub's clock domain (µs since startTime).
func (lb *LinkBridge) TimeAtBeat(beat float64) int64 {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return int64((beat - lb.beat) * 60.0 / lb.bpm * 1e6)
}

func (lb *LinkBridge) SpawnPoller(ctx context.Context) (chan<- LinkCommand, <-chan LinkEvent) {
	return SpawnLinkPoller(ctx, lb)
}
