//go:build !linkstub

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	"github.com/nicholasgasior/wail/wail-app/internal/pace"
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
	engineBitrateKbps  = 256 // quality-first: music at 48k stereo; ~34KB/s per stream
	discoveryInterval  = 1 * time.Second
	emitChunkFrames    = engineInternalRate * 5 / 1000 // ~5ms per paced write
	// emitCushionMs is how far ahead of the playhead each sink is kept fed —
	// stall tolerance for the emit loop. Receivers tolerate near-future stamps
	// (the reference renderer stalls on far-future buffers and plays ~4 beats
	// behind; Live's policy is unverified, so stay well under 100ms ≈ "now").
	emitCushionMs     = 80
	emitCushionFrames = engineInternalRate * emitCushionMs / 1000
	// plcMaxFramesPerGap caps Opus packet-loss concealment per seq gap (120ms):
	// libopus fades PLC to silence past ~100ms; a deeper gap's tail stays silent.
	plcMaxFramesPerGap = 6

	// Outgoing WAIF frames are paced at 2× real time (frames are 20ms of audio)
	// so a whole interval never bursts into the send queue or the relay's rate
	// limiter, while the send position stays ahead of the receiver's playhead
	// by a margin that grows through the interval (see internal/pace).
	sendFrameGap     = 10 * time.Millisecond
	sendQueueBatches = 4
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
	unlabeledWindows   atomic.Uint64 // capture windows dropped before a room anchor

	// Capture-dump debug toggle. dumpEnabled/dumpGen are read lock-free on the
	// hot path; dumpDir (resolved fresh per enable) is read under mu only when a
	// drain goroutine actually opens a dump. The dump files themselves are owned
	// entirely by each channel's drain goroutine (open/write/close), so nothing
	// races with the SetCaptureDump toggle.
	dumpEnabled atomic.Bool
	dumpGen     atomic.Uint64

	mu       sync.Mutex
	labeler  interval.RoomLabeler
	cfg      interval.Config
	tempoBPM float64
	dumpDir  string // guarded by mu; current dump session directory

	capture      map[string]*captureChannel // by channel-id hex
	emit         map[affinity.Key]*emitStream
	own          *affinity.OwnChannels // our published sinks, for discovery exclusion
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
	pacer  *pace.Sender

	// Cumulative diagnostics, mirrored from drain-goroutine-owned state each
	// tick so Health() can read them race-free from other goroutines.
	statRingDropped atomic.Uint64
	statLANLost     atomic.Uint64
	statLANGaps     atomic.Uint64
	statResnaps     atomic.Uint64
	statSlews       atomic.Uint64
	statLate        atomic.Uint64
	statBackfill    atomic.Uint64

	// Capture dump (debug). Owned by the drain goroutine; dumpGen tracks which
	// dump session this writer belongs to so a toggle-off/on reopens fresh files.
	dump    *captureDump
	dumpGen uint64
}

type emitStream struct {
	key      affinity.Key
	channels int
	dec      *IntervalDecoder
	reasm    *emit.Reassembler
	sched    *playout.Scheduler
	sink     *abllink.Sink // published Link Audio channel; the emit map keyed by
	// affinity.Key IS the affinity (a reconnecting identity reuses this stream +
	// channel), so no separate registry is needed.

	// Last raw name inputs; recompute + rename the channel only when these change
	// (avoids per-frame name formatting on the hot path).
	lastDisplayName string
	lastStreamName  string

	feeder *emit.Feeder // cushion-ahead sink feeder (owned by the emit loop)

	// Previous WriteInterleaved result (emit-loop-owned). A refusal can mean
	// "no listener" (constant while idle — not an event) or "queue full"; only
	// the success→failure edge marks a hole in audio a listener was receiving.
	lastWriteOK bool

	// Stream-order tracking for PLC (session-goroutine-owned, like dec): a seq
	// gap in arrival order is a permanent loss on our TCP transport.
	haveSeq   bool
	expectSeq uint32
	lastPos   emit.FramePos

	// Observability (ADR-0003 / pillar 8). Atomic so Health() on another
	// goroutine reads them race-free: decodeFailures/framesConcealed are bumped
	// on the session goroutine, the rest on the emit-loop goroutine.
	decodeFailures      atomic.Uint64 // Opus decode errors
	intervalsIncomplete atomic.Uint64 // released before the streaming tail arrived (benign)
	sinkUnderrunEvents  atomic.Uint64 // paced feed fell behind the playhead past the cushion
	sinkUnderrunFrames  atomic.Uint64 // frames skipped (played as silence) due to underrun
	framesMissedAtPlay  atomic.Uint64 // frames still absent when their interval retired
	framesConcealed     atomic.Uint64 // missing frames masked by Opus PLC
	sinkWriteRejected   atomic.Uint64 // sink refused a chunk mid-stream (queue full / listener left)
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
		own:      affinity.NewOwnChannels(),
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
	for _, st := range e.emit {
		if st.sink != nil {
			st.sink.Close()
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

func (e *linkAudioEngine) RoomIndex(localIndex int64) (int64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.labeler.RoomIndex(localIndex)
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

// SetCaptureDump toggles the debug capture-to-WAV dump (pre-Opus + post-Opus)
// for every enabled capture channel. Enabling resolves a fresh session
// directory and bumps the generation; each channel's drain goroutine notices on
// its next tick and opens/closes its own files (so file I/O never leaves that
// goroutine — no race with this toggle).
func (e *linkAudioEngine) SetCaptureDump(enabled bool) {
	if !enabled {
		if e.dumpEnabled.CompareAndSwap(true, false) {
			log.Printf("[audio] capture dump: disabled")
		}
		return
	}
	if e.dumpEnabled.Load() {
		return // already on
	}
	dir := filepath.Join(defaultDataDir(), "dumps", time.Now().Format("2006-01-02_15-04-05"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[audio] capture dump: cannot create %s: %v", dir, err)
		return
	}
	e.mu.Lock()
	e.dumpDir = dir
	e.mu.Unlock()
	// Bump generation before flipping the flag: any goroutine that observes the
	// flag as on (Go atomics are sequentially consistent) also sees the new gen.
	e.dumpGen.Add(1)
	e.dumpEnabled.Store(true)
	log.Printf("[audio] capture dump: enabled → %s", dir)
}

// reconcileDump opens or closes ch.dump to match the engine's dump toggle. Runs
// only on ch's drain goroutine, which therefore owns the dump files exclusively.
func (e *linkAudioEngine) reconcileDump(ch *captureChannel) {
	if e.dumpEnabled.Load() {
		gen := e.dumpGen.Load()
		if ch.dump != nil && ch.dumpGen == gen {
			return // already dumping this session
		}
		if ch.dump != nil {
			ch.dump.Close()
			ch.dump = nil
		}
		e.mu.Lock()
		dir := e.dumpDir
		e.mu.Unlock()
		name := sanitizeFilename(ch.name)
		if name == "" {
			name = "channel"
		}
		name = fmt.Sprintf("%s_stream%d", name, ch.streamID)
		d, err := newCaptureDump(dir, name, 2, engineInternalRate)
		if err != nil {
			log.Printf("[audio] capture dump: open failed for %q: %v", ch.name, err)
			return
		}
		ch.dump = d
		ch.dumpGen = gen
		log.Printf("[audio] capture dump: writing %q → %s", ch.name, dir)
	} else if ch.dump != nil {
		ch.dump.Close()
		ch.dump = nil
		log.Printf("[audio] capture dump: stopped for %q", ch.name)
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
		id := hex.EncodeToString(c.ID[:])
		// Never capture our own republished channels (feedback loop). Matching
		// by peer name alone is too blunt — a third-party publisher may share
		// our peer name — so classify by minted sink names and learned IDs.
		if e.own.Own(id, c.PeerName == e.peerName, c.Name) {
			continue
		}
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
	ch.asm = capture.NewWindowed(e.cfg, 2, engineInternalRate, samplesPerWaifFrame(engineInternalRate))
	name := ch.name // stable copy: the onDrop callback runs off the pacer goroutine
	ch.pacer = pace.New(sendFrameGap, sendQueueBatches, e.send, func(n int) {
		log.Printf("[audio] WARN: send backlog on %q — dropped a batch of %d frames", name, n)
	})

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
	if ch.pacer != nil {
		// Close only — Enqueue on a closed pacer is a no-op, so the drain
		// goroutine can race with teardown without a nil check.
		ch.pacer.Close()
	}
	if ch.source != nil {
		ch.source.Close()
		ch.source = nil
	}
}

func (e *linkAudioEngine) drainCapture(ctx context.Context, ch *captureChannel) {
	defer e.wg.Done()
	// Close any active dump on the same goroutine that writes it (teardown races
	// only against a context cancel, never a concurrent write).
	defer func() {
		if ch.dump != nil {
			ch.dump.Close()
			ch.dump = nil
		}
	}()
	ss := abllink.NewSessionState()
	defer ss.Close()
	t := time.NewTicker(engineTickInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.reconcileDump(ch)
			ch.statRingDropped.Store(ch.source.Dropped())
			ch.statLANLost.Store(ch.loss.LostBuffers())
			ch.statLANGaps.Store(ch.loss.GapEvents())
			if n := ch.asm.Resnaps(); n > ch.statResnaps.Load() {
				log.Printf("[audio] capture re-anchored on %q: stamp diverged past threshold — audible splice (total %d)", ch.name, n)
				ch.statResnaps.Store(n)
			}
			ch.statSlews.Store(ch.asm.SlewedFrames())
			ch.statLate.Store(ch.asm.DroppedLate())
			ch.statBackfill.Store(ch.asm.DroppedBackfill())
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
					// Real discontinuity: place the next buffer by its beat stamp
					// so the lost span reads as silence (placement is otherwise
					// sample-contiguous; see capture.Assembler).
					ch.asm.Reanchor()
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
				for _, w := range ch.asm.AddWindows(beat, buf.TempoBPM, pcm, nFrames) {
					e.emitWindow(ch, w)
				}
			}
		}
	}
}

// emitWindow labels one capture window with the room index, encodes it, and
// ships it. Windows arrive as their 20ms of audio fills, so WAIF frames leave
// in real time during the interval — the receiver has nearly the whole
// interval before its N+D playout boundary instead of racing the playhead.
func (e *linkAudioEngine) emitWindow(ch *captureChannel, w capture.Window) {
	e.mu.Lock()
	roomIdx, ok := e.labeler.RoomIndex(w.IntervalIndex)
	bpm, cfg := e.tempoBPM, e.cfg
	e.mu.Unlock()
	if !ok {
		// No room anchor yet; can't label. Same drop as the old whole-interval
		// path, now per window.
		if n := e.unlabeledWindows.Add(1); n == 1 || n%1000 == 0 {
			log.Printf("[audio] warn: dropping unlabeled capture windows on %q (%d total)", ch.name, n)
		}
		return
	}
	wire, err := ch.enc.EncodeWindow(w.Samples, WindowMeta{
		RoomIndex:   roomIdx,
		StreamID:    ch.streamID,
		FrameNumber: uint32(w.Number),
		Seq:         ch.seq,
		IsFinal:     w.IsFinal,
		TotalFrames: uint32(w.Total),
		BPM:         bpm,
		Quantum:     cfg.Quantum,
		Bars:        cfg.Bars,
	})
	if err != nil {
		log.Printf("[audio] encode failed on %q: %v", ch.name, err)
		return
	}
	ch.seq++
	if ch.dump != nil {
		ch.dump.writePair(w.Samples, wire)
	}
	ch.pacer.Enqueue([][]byte{wire})
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
		sinkName := affinity.FormatName(displayName, label)
		st = &emitStream{
			key:             key,
			channels:        channels,
			dec:             dec,
			reasm:           emit.New(channels, samplesPerWaifFrame(engineInternalRate)),
			sched:           playout.New(e.offsetD),
			sink:            e.link.NewSink(sinkName, engineInternalRate*2),
			feeder:          emit.NewFeeder(emitCushionFrames, emitChunkFrames),
			lastDisplayName: displayName,
			lastStreamName:  streamName,
		}
		e.own.Published(sinkName)
		e.emit[key] = st
		log.Printf("[audio] publishing Link Audio channel %q for %s stream %d", sinkName, fromIdentity, f.StreamID)
	} else if st.lastDisplayName != displayName || st.lastStreamName != streamName {
		// Name became known / changed: rename the existing channel in place, don't
		// re-mint it (affinity preserves the channel).
		if st.sink != nil {
			newName := affinity.FormatName(displayName, streamLabel(streamName, f.StreamID))
			st.sink.SetName(newName)
			e.own.Published(newName)
		}
		st.lastDisplayName = displayName
		st.lastStreamName = streamName
	}
	dec := st.dec // st.dec is only touched on this (session) goroutine
	cur := emit.FramePos{Interval: f.IntervalIndex, Frame: int(f.FrameNumber)}

	// Seq-gap detection: frames ride one TCP stream in send order, so a gap in
	// arrival order is a permanent loss (reconnect gap / sender queue drop) —
	// never the benign still-in-flight tail. Map the gap to frame slots while
	// the reassembler totals are at hand (under the lock).
	var plcSlots []emit.FramePos
	if st.haveSeq && f.FrameSeq != st.expectSeq && !seqLess(f.FrameSeq, st.expectSeq) {
		gap := int(f.FrameSeq - st.expectSeq)
		cfgFrames := roundUp(e.cfg.IntervalSamples(engineInternalRate, e.tempoBPM),
			samplesPerWaifFrame(engineInternalRate)) / samplesPerWaifFrame(engineInternalRate)
		reasm := st.reasm
		plcSlots = emit.MissingSlots(st.lastPos, cur, gap, func(iv int64) int {
			if _, _, total, ok := reasm.Interval(iv); ok && total > 0 {
				return total
			}
			return cfgFrames
		}, plcMaxFramesPerGap)
	}
	e.mu.Unlock()

	// Decode off-lock (heavy); place under the lock (reasm/sched are shared with
	// the emit-loop goroutine). PLC windows synthesize first, in stream order,
	// so the decoder state stays continuous through the gap and the next real
	// frame splices smoothly.
	var concealed [][]int16
	for range plcSlots {
		pcm, perr := dec.DecodePLC()
		if perr != nil {
			concealed = append(concealed, nil)
			continue
		}
		cp := make([]int16, len(pcm))
		copy(cp, pcm)
		concealed = append(concealed, cp)
	}
	pcm, derr := dec.DecodeFrame(f.OpusData)
	if derr != nil {
		n := st.decodeFailures.Add(1)
		if n == 1 || n%100 == 0 {
			log.Printf("[audio] warn: Opus decode failed on %v (%d total): %v", key, n, derr)
		}
		return
	}
	if !st.haveSeq || !seqLess(f.FrameSeq, st.expectSeq) {
		st.haveSeq = true
		st.expectSeq = f.FrameSeq + 1
		st.lastPos = cur
	}

	e.mu.Lock()
	for i, slot := range plcSlots {
		if concealed[i] == nil {
			continue
		}
		if st.sched.OnFrame(slot.Interval) != playout.TooLate {
			st.reasm.AddPLC(slot.Interval, slot.Frame, concealed[i])
			st.framesConcealed.Add(1)
		}
	}
	switch st.sched.OnFrame(f.IntervalIndex) {
	case playout.TooLate:
		// interval already finished playing; drop
	default:
		// Buffer or LiveAppend: place the frame; the paced reader picks it up.
		st.reasm.Add(f.IntervalIndex, int(f.FrameNumber), pcm, f.IsFinal, int(f.TotalFrames))
	}
	e.mu.Unlock()
}

// Health sums the per-channel and per-stream diagnostic counters. Capture-side
// values are drain-goroutine mirrors (atomics); emit-side counters are atomics
// on their streams; the maps themselves are guarded by e.mu.
func (e *linkAudioEngine) Health() EngineHealth {
	var h EngineHealth
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.capture {
		h.CaptureRingDropped += ch.statRingDropped.Load()
		h.CaptureLANLostBuffers += ch.statLANLost.Load()
		h.CaptureLANGapEvents += ch.statLANGaps.Load()
		h.CaptureResnaps += ch.statResnaps.Load()
		h.CaptureSlews += ch.statSlews.Load()
		h.CaptureDroppedLate += ch.statLate.Load()
		h.CaptureDroppedBackfill += ch.statBackfill.Load()
	}
	for _, st := range e.emit {
		h.EmitIntervalsIncomplete += st.intervalsIncomplete.Load()
		h.EmitSinkUnderrunEvents += st.sinkUnderrunEvents.Load()
		h.EmitSinkUnderrunFrames += st.sinkUnderrunFrames.Load()
		h.EmitFramesMissingAtPlay += st.framesMissedAtPlay.Load()
		h.EmitFramesConcealed += st.framesConcealed.Load()
		h.EmitSinkWriteRejected += st.sinkWriteRejected.Load()
		h.OpusDecodeFailures += st.decodeFailures.Load()
	}
	h.WireDecodeFailures = e.wireDecodeFailures.Load()
	return h
}

// emitLoop detects local interval boundaries and keeps each sink fed a bounded
// cushion ahead of the playhead (emit.Feeder; ADR-0002 deep-queue emit).
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
			e.topUpSinks(ss, q, localBeat)
		}
	}
}

// onBoundary releases each stream's due interval at this local boundary:
// retire the finished interval (measuring what never arrived), then promote
// the feeder's pre-rolled reader — or install a fresh one — for the release.
func (e *linkAudioEngine) onBoundary(cfg interval.Config, tempo float64, localIdx, roomLabel int64) {
	startBeat, endBeat := cfg.BeatWindow(localIdx)
	totalFrames := intervalPlayoutFrames(cfg, engineInternalRate, tempo)
	paddedFrames := intervalPaddedFrames(cfg, engineInternalRate, tempo)

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.emit {
		release, advanced := st.sched.OnBoundary(roomLabel)
		if !advanced {
			continue
		}
		idx := release

		// Retire the interval that just finished playing. Live-append had its
		// whole playback window; slots still empty were rendered as silence —
		// the honest audible-loss measure (PLC-concealed slots are not empty).
		if missing, _ := st.reasm.Missing(idx - 1); missing > 0 {
			st.framesMissedAtPlay.Add(uint64(missing))
		}
		st.reasm.Drop(idx - 1)

		// Released before its streaming tail arrived: expected with real-time
		// senders (the last frames are in flight at the boundary) — informational.
		if !st.reasm.Complete(idx) {
			n := st.intervalsIncomplete.Add(1)
			if n == 1 || n%100 == 0 {
				_, recv, total, _ := st.reasm.Interval(idx)
				log.Printf("[audio] interval %d released with %d/%d frames for %v (tail in flight — expected with streaming send; %d total)",
					idx, recv, total, st.key, n)
			}
		}

		reasm := st.reasm
		feeder := st.feeder
		channels := st.channels
		nextIdx := idx + 1
		// Runs when the cushion first crosses the playing interval's end
		// (~cushion before the boundary, under e.mu via topUpSinks). When the
		// final window's padding carries the next interval's real head (a
		// continuation-padding sender), play the current reader through the
		// pad and start the next reader past its twice-encoded head — the
		// decoded stream then has no boundary discontinuity at all. Silent
		// padding (old senders) keeps the truncate-at-interval-end handoff.
		makeNext := func() (*emit.PacedReader, int64, int) {
			start := 0
			if cur := feeder.Current(); cur != nil && paddedFrames > totalFrames {
				if s, _, _, ok := reasm.Interval(idx); ok && padCarriesAudio(s, totalFrames, paddedFrames, channels) {
					cur.SetTotalFrames(paddedFrames)
					start = paddedFrames - totalFrames
				}
			}
			return emit.NewPacedReader(
				func() []int16 { s, _, _, _ := reasm.Interval(nextIdx); return s },
				channels, engineInternalRate, tempo, endBeat, totalFrames,
			), nextIdx, start
		}
		if st.feeder.Promote(idx, makeNext) {
			// Adopted the pre-rolled reader; re-anchor if tempo moved since pre-roll.
			if cur := st.feeder.Current(); cur != nil && cur.TempoBPM() != tempo {
				cur.Rebase(tempo, totalFrames)
			}
		} else {
			st.feeder.SetCurrent(idx, emit.NewPacedReader(
				func() []int16 { s, _, _, _ := reasm.Interval(idx); return s },
				st.channels, engineInternalRate, tempo, startBeat, totalFrames,
			), makeNext)
		}
	}
}

// topUpSinks advances each stream's feeder to playhead+cushion, writing paced
// chunks into its sink (multiple chunks per tick when catching up after a stall).
func (e *linkAudioEngine) topUpSinks(ss *abllink.SessionState, quantum float64, localBeat float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.emit {
		if st.sink == nil {
			continue
		}
		st.feeder.Advance(localBeat, func(samples []int16, beat float64) {
			frames := len(samples) / st.channels
			ok := st.sink.WriteInterleaved(samples, ss, beat, quantum, frames, st.channels, engineInternalRate)
			// The feeder's cursor has moved past this chunk either way, so a
			// refusal while a listener was streaming is a permanent hole.
			if !ok && st.lastWriteOK {
				st.sinkWriteRejected.Add(1)
			}
			st.lastWriteOK = ok
		})
		ev, fr := st.feeder.Underruns()
		st.sinkUnderrunEvents.Store(ev)
		st.sinkUnderrunFrames.Store(fr)
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
