package main

import (
	"context"
	"log"
	"math"
	"time"

	"gopkg.in/hraban/opus.v2"
)

const (
	toneSampleRate = 48000
	toneChannels   = 2
	toneBitrateKbps = 128
	toneFrameMs    = 20
	toneSamplesPerFrame = toneSampleRate * toneFrameMs / 1000 // 960
	// Switch frequency every 4 intervals
	toneFreqSwitchIntervals = 4
)

// IntervalBoundaryInfo is sent to the test tone task on interval boundaries.
type IntervalBoundaryInfo struct {
	Index   int64
	BPM     float64
	Bars    uint32
	Quantum float64
}

// FramesPerInterval calculates the number of 20ms frames in one interval.
func FramesPerInterval(bpm float64, bars uint32, quantum float64) uint32 {
	if bpm <= 0 {
		return 0
	}
	beatsPerInterval := float64(bars) * quantum
	intervalSec := beatsPerInterval / (bpm / 60.0)
	return uint32(math.Ceil(intervalSec / 0.02))
}

// GenerateSineFrame generates one 20ms frame of stereo sine wave.
// Phase is updated in-place for continuity across frames.
func GenerateSineFrame(freq float64, phase *float64, sampleRate uint32, channels uint16) []int16 {
	samplesPerChannel := int(sampleRate) * toneFrameMs / 1000
	samples := make([]int16, samplesPerChannel*int(channels))
	phaseInc := 2.0 * math.Pi * freq / float64(sampleRate)

	for i := 0; i < samplesPerChannel; i++ {
		val := int16(math.Sin(*phase) * 16384) // ~50% amplitude
		for ch := 0; ch < int(channels); ch++ {
			samples[i*int(channels)+ch] = val
		}
		*phase += phaseInc
		if *phase > 2*math.Pi {
			*phase -= 2 * math.Pi
		}
	}
	return samples
}

// TestToneTask runs a virtual send plugin that generates WAIF frames.
// It behaves like a real WAIL Send plugin but runs in-process.
func TestToneTask(
	ctx context.Context,
	streamIndex uint16,
	connID int,
	fromPluginCh chan<- ipcFrame,
	boundaryCh <-chan IntervalBoundaryInfo,
) {
	enc, err := opus.NewEncoder(toneSampleRate, toneChannels, opus.AppAudio)
	if err != nil {
		log.Printf("[test-tone] Failed to create encoder: %v", err)
		return
	}
	if err := enc.SetBitrate(toneBitrateKbps * 1000); err != nil {
		log.Printf("[test-tone] Failed to set bitrate: %v", err)
	}

	var phase float64
	var currentIdx int64 = -1
	var currentBPM float64 = 120.0
	var currentBars uint32 = 4
	var currentQuantum float64 = 4.0
	var frameNumber uint32
	var totalFrames uint32
	var frameSeq uint32
	var intervalStart *time.Time

	opusBuf := make([]byte, 4096)

	for {
		select {
		case <-ctx.Done():
			return
		case boundary := <-boundaryCh:
			// Force-send final frame of previous interval if incomplete
			if currentIdx >= 0 && frameNumber > 0 && frameNumber < totalFrames {
				freq := toneFrequency(currentIdx)
				samples := GenerateSineFrame(freq, &phase, toneSampleRate, toneChannels)
				if opusData, n, err := encodeFrame(enc, samples, opusBuf); err == nil {
					sendWAIFFrame(fromPluginCh, connID, streamIndex, currentIdx,
						totalFrames-1, frameSeq, opusData[:n], true, currentBPM, currentQuantum, currentBars, totalFrames)
					frameSeq++
				}
			}
			currentIdx = boundary.Index
			currentBPM = boundary.BPM
			currentBars = boundary.Bars
			currentQuantum = boundary.Quantum
			frameNumber = 0
			totalFrames = FramesPerInterval(currentBPM, currentBars, currentQuantum)
			now := time.Now()
			intervalStart = &now
		default:
		}

		if currentIdx < 0 || frameNumber >= totalFrames {
			time.Sleep(5 * time.Millisecond)
			continue
		}

		// Wall-clock pacing
		if intervalStart != nil {
			elapsedMs := time.Since(*intervalStart).Milliseconds()
			dueFrame := uint32(elapsedMs / toneFrameMs)
			if dueFrame > totalFrames {
				dueFrame = totalFrames
			}
			if frameNumber >= dueFrame {
				time.Sleep(1 * time.Millisecond)
				continue
			}
		}

		freq := toneFrequency(currentIdx)
		samples := GenerateSineFrame(freq, &phase, toneSampleRate, toneChannels)

		opusData, n, err := encodeFrame(enc, samples, opusBuf)
		if err != nil {
			log.Printf("[test-tone] Encode failed: %v", err)
			time.Sleep(20 * time.Millisecond)
			continue
		}

		isFinal := frameNumber == totalFrames-1
		sendWAIFFrame(fromPluginCh, connID, streamIndex, currentIdx,
			frameNumber, frameSeq, opusData[:n], isFinal, currentBPM, currentQuantum, currentBars, totalFrames)
		frameNumber++
		frameSeq++
	}
}

func toneFrequency(intervalIndex int64) float64 {
	if (intervalIndex/toneFreqSwitchIntervals)%2 == 0 {
		return 440.0
	}
	return 880.0
}

func encodeFrame(enc *opus.Encoder, samples []int16, buf []byte) ([]byte, int, error) {
	n, err := enc.Encode(samples, buf)
	if err != nil {
		return nil, 0, err
	}
	return buf, n, nil
}

func sendWAIFFrame(ch chan<- ipcFrame, connID int, streamIndex uint16, intervalIdx int64,
	frameNum uint32, frameSeq uint32, opusData []byte, isFinal bool, bpm, quantum float64, bars, totalFrames uint32) {

	frame := &AudioFrame{
		IntervalIndex: intervalIdx,
		StreamID:      streamIndex,
		FrameNumber:   frameNum,
		FrameSeq:      frameSeq,
		Channels:      toneChannels,
		OpusData:      opusData,
		IsFinal:       isFinal,
	}
	if isFinal {
		frame.SampleRate = toneSampleRate
		frame.TotalFrames = totalFrames
		frame.BPM = bpm
		frame.Quantum = quantum
		frame.Bars = bars
	}

	waif := EncodeAudioFrameWire(frame)
	ipcMsg := EncodeAudioFrameMsg(waif)

	select {
	case ch <- ipcFrame{connID: connID, data: ipcMsg}:
	default:
	}
}
