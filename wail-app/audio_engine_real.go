//go:build !linkstub

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicholasgasior/wail/wail-app/internal/abllink"
	"github.com/nicholasgasior/wail/wail-app/internal/affinity"
	"github.com/nicholasgasior/wail/wail-app/internal/capture"
	"github.com/nicholasgasior/wail/wail-app/internal/dsp"
	"github.com/nicholasgasior/wail/wail-app/internal/emit"
	"github.com/nicholasgasior/wail/wail-app/internal/interval"
	"github.com/nicholasgasior/wail/wail-app/internal/lanloss"
	"github.com/nicholasgasior/wail/wail-app/internal/playout"
)

// linkAudioEngine implements AudioEngine against the cgo Link Audio binding.
//
// Threading: one drain goroutine per bridged capture channel (draining the
// pure-C ring off the RT thread), one emit loop goroutine (boundary detection +
// paced sink writes), and one discovery ticker. Shared state is guarded by mu.
// No allocation/locking happens on Link's RT capture thread — that is the pure-C
// callback in capture.c; these goroutines are all off-thread (ADR-0002).
//
// This driver is compile-verified only: the capture/emit *logic* it wraps is
// unit-tested (internal/*, interval_codec_test.go), but the Source/Sink data
// path and real-time behaviour need Link peers + a DAW on a LAN to exercise.
const (
	engineInternalRate = 48000
	engineTickInterval = 5 * time.Millisecond
	engineBitrateKbps  = 128
	discoveryInterval  = 1 * time.Second
	emitChunkFrames    = engineInternalRate * 5 / 1000 // ~5ms per paced write
)

type linkAudioEngine struct {
	lb       *LinkBridge
	link     *abllink.Link
	peerName string
	send     func(waif []byte)
	offsetD  int

	quantum float64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	wireDecodeFailures atomic.Uint64 // WAIF wire-decode errors (pre-Opus)

	mu       sync.Mutex
	labeler  interval.RoomLabeler
	cfg      interval.Config
	tempoBPM float64

	capture      map[string]*captureChannel // by channel-id hex
	emit         map[affinity.Key]*emitStream
	sinks        *affinity.Registry[*abllink.Sink]
	nextStreamID uint16
}

type captureChannel struct {
	id       abllink.ChannelID
	name     string
	peerName string
	streamID uint16
	enabled  bool

	source *abllink.Source
	asm    *capture.Assembler
	enc    *IntervalEncoder
	loss   lanloss.Tracker
	seq    uint32
	drain  context.CancelFunc
}

type emitStream struct {
	key      affinity.Key
	channels int
	dec      *IntervalDecoder
	reasm    *emit.Reassembler
	sched    *playout.Scheduler
	// Last raw name inputs; recompute + rename the channel only when these change
	// (avoids per-frame name formatting on the hot path).
	lastDisplayName string
	lastStreamName  string

	reader *emit.PacedReader

	// Observability (ADR-0003 / pillar 8), currently surfaced only via
	// rate-limited logs. Atomic so a future Stats() sampler on another goroutine
	// can read them race-free: decodeFailures is bumped on the session goroutine,
	// intervalsIncomplete on the emit-loop goroutine.
	decodeFailures      atomic.Uint64 // Opus decode errors
	intervalsIncomplete atomic.Uint64 // intervals not fully delivered by their N+D boundary
}

func newAudioEngine(lb *LinkBridge, peerName string, send func(waif []byte), offsetD int) AudioEngine {
	return &linkAudioEngine{
		lb:       lb,
		link:     lb.Link(),
		peerName: peerName,
		send:     send,
		offsetD:  offsetD,
		quantum:  lb.Quantum(),
		cfg:      interval.Config{Bars: 4, Quantum: lb.Quantum()},
		tempoBPM: 120,
		capture:  make(map[string]*captureChannel),
		emit:     make(map[affinity.Key]*emitStream),
		sinks:    affinity.New[*abllink.Sink](),
	}
}

func (e *linkAudioEngine) Start() error {
	e.link.SetPeerName(e.peerName)
	e.link.EnableLinkAudio(true)
	e.ctx, e.cancel = context.WithCancel(context.Background())

	e.wg.Add(2)
	go e.discoveryLoop()
	go e.emitLoop()
	log.Printf("[audio] Link Audio engine started (peer %q, offset D=%d)", e.peerName, e.offsetD)
	return nil
}

func (e *linkAudioEngine) Stop() {
	if e.cancel != nil {
		e.cancel()
	}
	e.wg.Wait()

	e.mu.Lock()
	for _, ch := range e.capture {
		if ch.drain != nil {
			ch.drain()
		}
		if ch.source != nil {
			ch.source.Close()
		}
	}
	e.capture = make(map[string]*captureChannel)
	for _, k := range e.sinks.Keys() {
		if h, ok := e.sinks.Remove(k); ok && h != nil {
			h.Close()
		}
	}
	e.emit = make(map[affinity.Key]*emitStream)
	e.mu.Unlock()

	e.link.EnableLinkAudio(false)
	log.Printf("[audio] Link Audio engine stopped")
}

func (e *linkAudioEngine) SetRoomAnchor(currentIndex int64, bpm float64, bars uint32, quantum float64) {
	// Sample our local interval index now and align the labeler to the room index.
	ss := abllink.NewSessionState()
	defer ss.Close()
	e.link.CaptureAppSessionState(ss)
	localBeat := ss.BeatAtTime(e.link.ClockMicros(), quantum)

	e.mu.Lock()
	defer e.mu.Unlock()
	if bpm > 0 {
		e.tempoBPM = bpm
	}
	if bars > 0 {
		e.cfg.Bars = bars
	}
	if quantum > 0 {
		e.cfg.Quantum = quantum
		e.quantum = quantum
	}
	localIdx := e.cfg.IndexAtBeat(localBeat)
	e.labeler.Align(currentIndex, localIdx)
}

func (e *linkAudioEngine) CaptureChannels() []CaptureChannelInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]CaptureChannelInfo, 0, len(e.capture))
	for id, ch := range e.capture {
		out = append(out, CaptureChannelInfo{
			ChannelID: id, Name: ch.name, PeerName: ch.peerName, Enabled: ch.enabled,
		})
	}
	// Grouped by app then alphabetical: primary key is the peer (app) name,
	// secondary is the channel name — both case-insensitive. Stable so the
	// send-mixer never reshuffles across status ticks.
	sort.Slice(out, func(i, j int) bool {
		pi, pj := strings.ToLower(out[i].PeerName), strings.ToLower(out[j].PeerName)
		if pi != pj {
			return pi < pj
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (e *linkAudioEngine) SetCaptureEnabled(channelID string, on bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.capture[channelID]
	if !ok {
		return
	}
	if on && !ch.enabled {
		e.startCaptureLocked(ch)
	} else if !on && ch.enabled {
		e.stopCaptureLocked(ch)
	}
}

// --- capture ---

// discoveryLoop periodically reconciles discovered local Link Audio channels
// against our capture map, excluding our own published channels (best-effort, by
// peer name) to avoid a feedback loop. Discovered channels start disabled — the
// user opts in per channel via the send-mixer (explicit opt-in, plan Step 1).
func (e *linkAudioEngine) discoveryLoop() {
	defer e.wg.Done()
	t := time.NewTicker(discoveryInterval)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			e.reconcileChannels(e.link.Channels())
		}
	}
}

func (e *linkAudioEngine) reconcileChannels(chans []abllink.Channel) {
	e.mu.Lock()
	defer e.mu.Unlock()
	seen := make(map[string]bool, len(chans))
	for _, c := range chans {
		if c.PeerName == e.peerName {
			continue // our own republished channel — never capture it
		}
		id := hex.EncodeToString(c.ID[:])
		seen[id] = true
		ch, ok := e.capture[id]
		if !ok {
			// Register discovered channel as available but disabled; the user
			// enables it via SetCaptureEnabled (explicit opt-in send-mixer).
			ch = &captureChannel{id: c.ID, name: c.Name, peerName: c.PeerName, streamID: e.nextStreamID}
			e.nextStreamID++
			e.capture[id] = ch
		} else {
			ch.name = c.Name
			ch.peerName = c.PeerName
		}
	}
	// Drop channels that disappeared.
	for id, ch := range e.capture {
		if !seen[id] {
			e.stopCaptureLocked(ch)
			delete(e.capture, id)
		}
	}
}

// startCaptureLocked opens a source + drain goroutine for a channel. mu held.
func (e *linkAudioEngine) startCaptureLocked(ch *captureChannel) {
	if ch.enabled {
		return
	}
	src := e.link.NewSource(ch.id, 0, 0)
	if src == nil {
		log.Printf("[audio] failed to create source for channel %q", ch.name)
		return
	}
	ch.source = src
	ch.enabled = true
	// Channel count is learned from the first buffer; default stereo encoder.
	enc, err := NewIntervalEncoder(2, engineInternalRate, engineBitrateKbps)
	if err != nil {
		log.Printf("[audio] encoder init failed for %q: %v", ch.name, err)
		ch.enabled = false
		src.Close()
		ch.source = nil
		return
	}
	ch.enc = enc
	ch.asm = capture.New(e.cfg, 2, engineInternalRate)

	dctx, dcancel := context.WithCancel(e.ctx)
	ch.drain = dcancel
	e.wg.Add(1)
	go e.drainCapture(dctx, ch)
}

// stopCaptureLocked tears down a channel's source + drain. mu held.
func (e *linkAudioEngine) stopCaptureLocked(ch *captureChannel) {
	if !ch.enabled {
		return
	}
	ch.enabled = false
	if ch.drain != nil {
		ch.drain()
		ch.drain = nil
	}
	if ch.source != nil {
		ch.source.Close()
		ch.source = nil
	}
}

func (e *linkAudioEngine) drainCapture(ctx context.Context, ch *captureChannel) {
	defer e.wg.Done()
	ss := abllink.NewSessionState()
	defer ss.Close()
	t := time.NewTicker(engineTickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.link.CaptureAppSessionState(ss)
			quantum := e.quantumSnapshot() // stable within a tick; snapshot once, not per buffer
			for {
				buf, ok := ch.source.Pop()
				if !ok {
					break
				}
				if gap := ch.loss.Observe(buf.Count); gap != nil {
					log.Printf("[audio] LAN loss on %q: %d buffers (count %d→%d)",
						ch.name, gap.LostBuffers, gap.ExpectedCount, gap.GotCount)
				}
				beat, mapped := ch.source.BeginBeats(&buf, ss, quantum)
				if !mapped {
					continue // cross-session buffer; can't place it
				}
				pcm := buf.Samples
				if buf.SampleRate != engineInternalRate {
					pcm = dsp.ResampleLinearInterleaved(pcm, buf.NumChannels, int(buf.SampleRate), engineInternalRate)
				}
				nFrames := len(pcm) / max1(buf.NumChannels)
				if done := ch.asm.Add(beat, buf.TempoBPM, pcm, nFrames); done != nil {
					e.emitCaptured(ch, done)
				}
			}
		}
	}
}

// emitCaptured labels a completed local interval with the room index and ships
// it as WAIF frames to the relay.
func (e *linkAudioEngine) emitCaptured(ch *captureChannel, done *capture.CompletedInterval) {
	e.mu.Lock()
	roomIdx, ok := e.labeler.RoomIndex(done.Index)
	bpm, cfg := e.tempoBPM, e.cfg
	e.mu.Unlock()
	if !ok {
		return // no room anchor yet; can't label
	}
	frames, next, err := ch.enc.EncodeInterval(done.Samples, roomIdx, ch.streamID, ch.seq, bpm, cfg.Quantum, cfg.Bars)
	if err != nil {
		log.Printf("[audio] encode failed on %q: %v", ch.name, err)
		return
	}
	ch.seq = next
	for _, f := range frames {
		e.send(f)
	}
}

// --- emit ---

// HandleRemoteAudio is called on the session goroutine. streamName is the
// sender's human-readable name for this stream (from StreamNames sync); the
// published channel is named "{peer} · {stream}" (ADR-0002). It may be empty
// early (before names arrive) — we fall back to "stream N" and rename the
// channel in place once the real name is known (affinity preserves the channel).
func (e *linkAudioEngine) HandleRemoteAudio(fromIdentity, displayName, streamName string, waif []byte) {
	f, err := DecodeAudioFrameWire(waif)
	if err != nil {
		n := e.wireDecodeFailures.Add(1)
		if n == 1 || n%100 == 0 {
			log.Printf("[audio] warn: WAIF decode failed from %s (%d total): %v", fromIdentity, n, err)
		}
		return
	}
	key := affinity.Key{Identity: fromIdentity, Stream: f.StreamID}

	e.mu.Lock()
	st, ok := e.emit[key]
	if !ok {
		channels := int(f.Channels)
		if channels < 1 {
			channels = 1
		}
		dec, derr := NewIntervalDecoder(channels, engineInternalRate)
		if derr != nil {
			e.mu.Unlock()
			log.Printf("[audio] decoder init failed for %v: %v", key, derr)
			return
		}
		label := streamLabel(streamName, f.StreamID)
		st = &emitStream{
			key:             key,
			channels:        channels,
			dec:             dec,
			reasm:           emit.New(channels, samplesPerWaifFrame(engineInternalRate)),
			sched:           playout.New(e.offsetD),
			lastDisplayName: displayName,
			lastStreamName:  streamName,
		}
		e.emit[key] = st
		e.sinks.Resolve(key, displayName, label, func() *abllink.Sink {
			return e.link.NewSink(affinity.FormatName(displayName, label), engineInternalRate*2)
		})
	} else if st.lastDisplayName != displayName || st.lastStreamName != streamName {
		// Name became known / changed: rename the existing channel in place, don't
		// re-mint it (affinity preserves the channel).
		desiredName := affinity.FormatName(displayName, streamLabel(streamName, f.StreamID))
		if ent, ok := e.sinks.Get(key); ok && ent.Handle != nil {
			ent.Handle.SetName(desiredName)
			ent.Name = desiredName
		}
		st.lastDisplayName = displayName
		st.lastStreamName = streamName
	}
	dec := st.dec // st.dec is only touched on this (session) goroutine
	e.mu.Unlock()

	// Decode off-lock (heavy); place under the lock (reasm/sched are shared with
	// the emit-loop goroutine).
	pcm, derr := dec.DecodeFrame(f.OpusData)
	if derr != nil {
		n := st.decodeFailures.Add(1)
		if n == 1 || n%100 == 0 {
			log.Printf("[audio] warn: Opus decode failed on %v (%d total): %v", key, n, derr)
		}
		return
	}
	e.mu.Lock()
	switch st.sched.OnFrame(f.IntervalIndex) {
	case playout.TooLate:
		// interval already finished playing; drop
	default:
		// Buffer or LiveAppend: place the frame; the paced reader picks it up.
		st.reasm.Add(f.IntervalIndex, int(f.FrameNumber), pcm, f.IsFinal, int(f.TotalFrames))
	}
	e.mu.Unlock()
}

// emitLoop detects local interval boundaries and paces released intervals into
// their sinks a few ms at a time (deep-queue top-up, ADR-0002).
func (e *linkAudioEngine) emitLoop() {
	defer e.wg.Done()
	ss := abllink.NewSessionState()
	defer ss.Close()
	t := time.NewTicker(engineTickInterval)
	defer t.Stop()

	var lastLocalIdx int64
	haveLast := false

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			e.link.CaptureAppSessionState(ss)
			q := e.quantumSnapshot()
			localBeat := ss.BeatAtTime(e.link.ClockMicros(), q)

			e.mu.Lock()
			cfg := e.cfg
			tempo := e.tempoBPM
			localIdx := cfg.IndexAtBeat(localBeat)
			roomLabel, labeled := e.labeler.RoomIndex(localIdx)
			e.mu.Unlock()

			if labeled && (!haveLast || localIdx > lastLocalIdx) {
				e.onBoundary(cfg, tempo, localIdx, roomLabel)
				lastLocalIdx = localIdx
				haveLast = true
			}
			e.topUpSinks(ss, q, cfg, tempo)
		}
	}
}

// onBoundary releases each stream's due interval at this local boundary.
func (e *linkAudioEngine) onBoundary(cfg interval.Config, tempo float64, localIdx, roomLabel int64) {
	startBeat, _ := cfg.BeatWindow(localIdx)
	totalFrames := roundUp(cfg.IntervalSamples(engineInternalRate, tempo), samplesPerWaifFrame(engineInternalRate))

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.emit {
		release, advanced := st.sched.OnBoundary(roomLabel)
		if !advanced {
			continue
		}
		idx := release
		// Late/incomplete at the N+D boundary → play-partial (below) + warn +
		// per-stream "interval incomplete" metric, distinct from LAN loss and
		// decode failures (ADR-0003).
		if !st.reasm.Complete(idx) {
			n := st.intervalsIncomplete.Add(1)
			if n == 1 || n%50 == 0 {
				_, recv, total, _ := st.reasm.Interval(idx)
				log.Printf("[audio] warn: interval %d incomplete at boundary for %v (%d/%d frames, %d total) — playing partial",
					idx, st.key, recv, total, n)
			}
		}
		st.reasm.Drop(idx - 1) // intervals before the one we're releasing can never play
		reasm := st.reasm
		st.reader = emit.NewPacedReader(
			func() []int16 { s, _, _, _ := reasm.Interval(idx); return s },
			st.channels, engineInternalRate, tempo, startBeat, totalFrames,
		)
	}
}

// topUpSinks writes the next paced chunk of each playing interval to its sink.
func (e *linkAudioEngine) topUpSinks(ss *abllink.SessionState, quantum float64, cfg interval.Config, tempo float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.emit {
		if st.reader == nil {
			continue
		}
		ent, ok := e.sinks.Get(st.key)
		if !ok || ent.Handle == nil {
			continue
		}
		samples, beat, done := st.reader.Next(emitChunkFrames)
		if len(samples) > 0 {
			frames := len(samples) / st.channels
			ent.Handle.WriteInterleaved(samples, ss, beat, quantum, frames, st.channels, engineInternalRate)
		}
		if done {
			st.reader = nil
			playing, _ := st.sched.Playing()
			st.reasm.Drop(playing)
		}
	}
}

func (e *linkAudioEngine) quantumSnapshot() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.quantum
}

// --- small helpers ---

// streamLabel is the stream half of a channel name, falling back to "stream N"
// before the sender's StreamNames sync arrives.
func streamLabel(name string, id uint16) string {
	if name == "" {
		return fmt.Sprintf("stream %d", id)
	}
	return name
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func roundUp(n, multiple int) int {
	if multiple <= 0 {
		return n
	}
	return ((n + multiple - 1) / multiple) * multiple
}
