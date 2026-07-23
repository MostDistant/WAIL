package main

import (
	"context"
	"log"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/metronome"
)

const (
	metSampleRate  = 48000
	metChannels    = 2
	metBitrateKbps = 256 // match the capture engine's quality (engineBitrateKbps)

	// metronomeBroadcastStreamID is the WAIF stream_id the broadcast metronome
	// sends under. Reserved high so it can't collide with capture channels
	// (numbered from 0 via linkAudioEngine.nextStreamID) or UI-picked test-tone /
	// WAV send slots; receivers key streams on (identity, stream_id).
	metronomeBroadcastStreamID uint16 = 0xFF01
)

// MetronomeSenderTask is an in-app sender (like TestToneTask) that broadcasts
// the WAIL Metronome click to the room as an audio stream. On each interval
// boundary it renders one interval of click PCM on the shared ROOM grid,
// Opus-encodes it into WAIF frames, and drips them onto the relay at a
// wall-clock 20ms cadence so they leave in real time during the interval and
// never burst the relay. Receivers decode it, hold it until room interval N+D,
// and republish it as a "{peer} · WAIL Metronome" Link Audio channel — the same
// click grid as the sender's local metronome, only one interval late.
func MetronomeSenderTask(
	ctx context.Context,
	streamID uint16,
	send func([]byte),
	boundaryCh <-chan IntervalBoundaryInfo,
) {
	enc, err := NewIntervalEncoder(metChannels, metSampleRate, metBitrateKbps)
	if err != nil {
		log.Printf("[metronome-send] encoder init failed: %v", err)
		return
	}

	var (
		frames        [][]byte // current interval's pre-encoded WAIF frames, in order
		sentFrames    int      // how many of frames have already gone out
		intervalStart time.Time
		seq           uint32
	)

	// flushRemaining ships any not-yet-dripped frames of the current interval at
	// once. They are already encoded (incl. the IsFinal trailer), so this
	// guarantees the receiver learns the interval's TotalFrames before the next
	// interval starts — mirrors the test tone's force-send-final-frame guard.
	flushRemaining := func() {
		for ; sentFrames < len(frames); sentFrames++ {
			send(frames[sentFrames])
		}
	}

	// A fine ticker paces the drip; precision is uncritical (there is a whole
	// interval of lead time before the receiver's N+D playout).
	t := time.NewTicker(2 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case b := <-boundaryCh:
			// New interval: flush the tail of the previous one, then render the
			// click on the room grid and encode the whole interval up front.
			flushRemaining()
			pcm := metronome.RenderInterval(b.Cfg, b.BPM, metSampleRate, metChannels, b.Index)
			fr, next, encErr := enc.EncodeInterval(pcm, b.Index, streamID, seq, b.BPM, b.Cfg.Quantum, b.Cfg.Bars)
			if encErr != nil {
				log.Printf("[metronome-send] encode failed: %v", encErr)
				frames, sentFrames = nil, 0
				continue
			}
			frames, seq, sentFrames = fr, next, 0
			intervalStart = time.Now()
		case <-t.C:
			if len(frames) == 0 {
				continue
			}
			// Wall-clock 20ms cadence: frame i is due once its 20ms window has
			// elapsed, i.e. at (i+1)*20ms into the interval (elapsedMs/20 > i).
			dueFrame := int(time.Since(intervalStart).Milliseconds() / waifFrameMs)
			if dueFrame > len(frames) {
				dueFrame = len(frames)
			}
			for sentFrames < dueFrame {
				send(frames[sentFrames])
				sentFrames++
			}
		}
	}
}
