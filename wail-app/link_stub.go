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

func (lb *LinkBridge) SpawnPoller(ctx context.Context) (chan<- LinkCommand, <-chan LinkEvent) {
	return SpawnLinkPoller(ctx, lb)
}
