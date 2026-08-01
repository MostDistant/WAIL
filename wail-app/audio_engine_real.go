//go:build !linkstub

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	"github.com/nicholasgasior/wail/wail-app/internal/metronome"
	"github.com/nicholasgasior/wail/wail-app/internal/offset"
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
	// stall tolerance for the emit loop. It stamps audio into the future, which
	// some Link Audio receivers stall on (a third-party DAW bridge was measured
	// with ~0 tolerance), so the default was briefly 0 — floored to one ~5ms
	// write chunk, the minimum that still emits. Field testing (2026-07-23) showed
	// that floor gives the emit loop zero jitter tolerance: the playhead
	// overtakes the cursor on ordinary scheduling noise and every tick logged
	// sink underruns with audible dropouts (~150 events/s). 100ms absorbs that
	// comfortably; receivers that choke on feed-ahead lose out for now (see
	// tradeoffs.md). WAIL_EMIT_CUSHION_MS overrides.
	emitCushionMs = 100
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

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	wireDecodeFailures atomic.Uint64 // WAIF wire-decode errors (pre-Opus)
	unlabeledWindows   atomic.Uint64 // capture windows dropped before a room anchor

	// Latest room label of the current local interval, refreshed each emit-loop
	// tick. HandleRemoteAudio diffs incoming frame labels against this to
	// measure anchor offset disagreement with each sender in real time.
	lastRoomLabel   atomic.Int64
	lastRoomLabeled atomic.Bool

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

	// loggedRoomSkip remembers which room-prefixed channels we have already
	// explained skipping (by channel id), so the reason is stated once per
	// channel instead of on every discovery tick.
	loggedRoomSkip map[string]bool

	// peerGoneGrace is how long a departed peer's channels stay published
	// (WAIL_STREAM_RETIRE_SEC), resolved once at construction.
	peerGoneGrace time.Duration

	// intent is what each sending identity says it is still sending (from their
	// StreamNames sync, or "gone" when they leave). A published stream absent
	// from its owner's intent is retired at a boundary once it has drained —
	// without this, e.emit only ever grows within a session and dead channels
	// keep publishing silence, holding a port on every WAIL Receive on the LAN.
	intent map[string]peerIntent

	// retired accumulates the diagnostic counters of retired streams, so the
	// Health totals the session diffs never go backwards when a stream goes
	// away (a decrease silently swallows later real events: the delta helper
	// only logs when now > prev).
	retired EngineHealth

	// stopping is set under mu before Stop's wg.Wait, and checked before any wg.Add
	// (startDrainLocked). It stops a late AddPluginSource / SetCaptureEnabled from
	// racing a wg.Add against the Wait (which would panic) or resurrecting state on a
	// torn-down engine.
	stopping bool

	// restore is the remembered set of user-enabled capture channels, keyed by
	// human-stable (peer, channel) name. A discovered channel matching the set
	// auto-enables; SetCaptureEnabled keeps it in sync with the user's toggles.
	restore map[CaptureChannelKey]bool

	cushionFrames int // per-sink feed-ahead (emitCushionMs or WAIL_EMIT_CUSHION_MS)

	// Locally-generated room metronome click channel (nil = off). metronomeOn
	// mirrors (metronome != nil) so the emit loop skips it lock-free when off.
	metronome   *metronomeStream
	metronomeOn atomic.Bool

	// Local-grid jump handoff: the emit loop detects the beat discontinuity
	// and attributes it; the session goroutine consumes it (TakeGridJump),
	// re-arms grid alignment, and reports it to the room.
	gridJump atomic.Pointer[GridJump]

	// Our own entry snap moves the grid deliberately, which trips the same
	// detector. Recording it is what separates "we did this" from "something
	// out there did this" — the difference between a wasted re-alignment
	// cycle and a real one.
	lastSnapAtNs    atomic.Int64
	lastSnapDeltaUs atomic.Int64

	// curRoomIdx is the room index of the local interval currently in progress
	// (emit loop publishes, HandleRemoteAudio reads) for label-offset
	// confirmation. math.MinInt64 until the labeler is aligned.
	curRoomIdx atomic.Int64
}

// peerIntent is one identity's declared set of streams. keep holds the stream
// ids they say they are still sending; gone means they left the room, so
// nothing of theirs is wanted.
type peerIntent struct {
	keep map[uint16]bool
	gone bool
}

// Retirement graces, measured from a stream's last frame. A peer that drops a
// stream said so explicitly, so it only has to outlast frames still in flight.
// A peer that left may just be blipping, and affinity exists so their channel
// survives a reconnect and the far side's routing holds — hence far longer.
const (
	retireGraceDropped  = 5 * time.Second
	retireGracePeerGone = 30 * time.Second
)

// retireHorizonIntervals is how far past the release cursor (plus D) buffered
// audio still counts as imminent, and so still blocks retirement. Matches the
// margin the emit loop already treats as normal buffering before it calls a
// sender's labels an anchor mismatch.
const retireHorizonIntervals = 2

// enginePeerGoneGrace resolves how long a departed peer's channels stay
// published, WAIL_STREAM_RETIRE_SEC overriding the default. Always logs the
// effective value: a session must be diagnosable from its log alone, without
// inferring the default from an absent override line.
func enginePeerGoneGrace() time.Duration {
	d, src := retireGracePeerGone, "default"
	if v := os.Getenv("WAIL_STREAM_RETIRE_SEC"); v != "" {
		// Floored, not just non-negative: a near-zero grace lets the sweep
		// retire a stream inside the window HandleRemoteAudio drops the lock
		// for its decode, which costs that frame and flaps the channel.
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			d, src = time.Duration(n)*time.Second, "WAIL_STREAM_RETIRE_SEC"
		} else {
			log.Printf("[audio] warn: bad WAIL_STREAM_RETIRE_SEC %q (want whole seconds >= 1): using %s", v, d)
		}
	}
	log.Printf("[audio] departed-peer channel retirement: %s (%s)", d, src)
	return d
}

// Cushion clamp bounds (see emitCushionMs): 100 is the floor — below it the
// emit loop has no jitter tolerance and underruns on scheduling noise (field
// evidence 2026-07-23); far above, receivers may drop far-future stamps.
const (
	cushionMinMs = 100
	cushionMaxMs = 500
)

func clampCushionMs(ms int) int   { return min(max(ms, cushionMinMs), cushionMaxMs) }
func cushionFramesFor(ms int) int { return engineInternalRate * ms / 1000 }
func cushionMsFromFrames(f int) int {
	return f * 1000 / engineInternalRate
}

// engineCushionFrames resolves the initial emit cushion: emitCushionMs unless
// WAIL_EMIT_CUSHION_MS overrides it (clamped). SetCushionMs changes it live.
func engineCushionFrames() int {
	ms := emitCushionMs
	src := "default"
	if v := os.Getenv("WAIL_EMIT_CUSHION_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			ms = clampCushionMs(n)
			src = "WAIL_EMIT_CUSHION_MS"
		} else {
			log.Printf("[audio] warn: bad WAIL_EMIT_CUSHION_MS %q: %v", v, err)
		}
	}
	// Always log the effective value — sessions must be diagnosable from the
	// log alone, without inferring the default from an absent override line.
	log.Printf("[audio] emit cushion: %dms (%s)", ms, src)
	return cushionFramesFor(ms)
}

// SetIntervalOffset live-adjusts the receive playout offset D for every
// current and future stream (clamped 0..4; returns the effective D). D is
// the NINJAM latency/reliability knob each receiver sets for itself — how
// many intervals remote audio is held before playout.
func (e *linkAudioEngine) SetIntervalOffset(d int) int {
	if d < 0 {
		d = 0
	}
	if d > 4 {
		d = 4
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.offsetD = d
	for _, st := range e.emit {
		st.sched.SetOffset(int64(d))
	}
	log.Printf("[audio] interval offset D set to %d (live)", d)
	return d
}

// SetCushionMs live-adjusts the emit feed-ahead depth for every current and
// future stream (and the metronome). Clamped; returns the effective ms.
func (e *linkAudioEngine) SetCushionMs(ms int) int {
	ms = clampCushionMs(ms)
	frames := cushionFramesFor(ms)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cushionFrames = frames
	for _, st := range e.emit {
		st.feeder.SetCushion(frames)
	}
	if e.metronome != nil {
		e.metronome.feeder.SetCushion(frames)
	}
	log.Printf("[audio] emit cushion set to %dms (live)", ms)
	return ms
}

type captureChannel struct {
	id       abllink.ChannelID
	name     string
	peerName string
	streamID uint16
	enabled  bool

	source captureSource
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
	// sinks are this stream's published outputs. sinks[0] is the Link Audio
	// sink — the write-rejected metric anchors on it. The emit map keyed
	// by affinity.Key IS the affinity (a reconnecting identity reuses this stream +
	// channel), so no separate registry is needed.
	sinks []emitSink

	// Last raw name inputs; recompute + rename the channel only when these change
	// (avoids per-frame name formatting on the hot path).
	lastDisplayName string
	lastStreamName  string

	// lastFrameAt is when a frame for this stream last arrived, the idle clock
	// for retirement. Set on the session goroutine, read by the emit loop's
	// boundary sweep — both under e.mu.
	lastFrameAt time.Time

	feeder *emit.Feeder // cushion-ahead sink feeder (owned by the emit loop)

	// shiftFrames is the codec lookahead (OPUS_GET_LOOKAHEAD) this stream's
	// decoded audio is realigned by at read time (see shiftedInterval);
	// shiftScratch backs the shifted view. Emit-loop-owned.
	shiftFrames  int
	shiftScratch []int16

	// Previous WriteInterleaved result (emit-loop-owned). A refusal can mean
	// "no listener" (constant while idle — not an event) or "queue full"; only
	// the success→failure edge marks a hole in audio a listener was receiving.
	lastWriteOK bool

	// Stream-order tracking for PLC (session-goroutine-owned, like dec): a seq
	// gap in arrival order is a permanent loss on our TCP transport.
	haveSeq   bool
	expectSeq uint32
	lastPos   emit.FramePos

	// labelTrack confirms this stream's interval labels agree with our room
	// index (ADR-0006 follow-up): a persistent nonzero verdict means the
	// peer's audio plays that many intervals late/early, silently.
	labelTrack emit.LabelOffsetTracker

	// offsetTrk accumulates per-frame RMS at absolute room-frame positions
	// (debug-room analysis: this stream's rhythmic phase offset vs the room
	// grid, internal/offset). Created on first decoded frame.
	offsetTrk *offset.Tracker

	// Observability (ADR-0003 / pillar 8). Atomic so Health() on another
	// goroutine reads them race-free: decodeFailures/framesConcealed are bumped
	// on the session goroutine, the rest on the emit-loop goroutine.
	decodeFailures      atomic.Uint64 // Opus decode errors
	intervalsIncomplete atomic.Uint64 // released before the streaming tail arrived (benign)
	sinkUnderrunEvents  atomic.Uint64 // paced feed fell behind the playhead past the cushion
	sinkUnderrunFrames  atomic.Uint64 // frames skipped (played as silence) due to underrun
	framesMissedAtPlay  atomic.Uint64 // frames still absent when their interval retired
	framesTooLate       atomic.Uint64 // frames dropped: sender labels already behind our playout (anchor mismatch)
	labelGapBand        atomic.Int64  // last logged sender-label gap (edge-trigger)
	labelGapLogNs       atomic.Int64  // rate limit for the label-gap warn
	gapLogs             uint64        // buffered-ahead warn count (emit-loop-owned, rate limit)
	framesConcealed     atomic.Uint64 // missing frames masked by Opus PLC
	sinkWriteRejected   atomic.Uint64 // sink refused a chunk mid-stream (queue full / listener left)
}

// metronomeStream is a locally-generated click track published as a "WAIL
// Metronome" Link Audio channel — no remote peer, no decode/reassembly. It
// rides the same feeder/cushion/sink emit machinery as remote streams (so the
// click audibly reflects emit-path health) and clicks on the local Link beat
// grid, so it lines up with the DAW's own metronome. Guarded by e.mu.
type metronomeStream struct {
	sink         *abllink.Sink
	feeder       *emit.Feeder
	channels     int
	installedIdx int64 // interval whose reader is currently installed
	haveReader   bool
}

func newAudioEngine(lb *LinkBridge, peerName string, send func(waif []byte), offsetD int) AudioEngine {
	e := &linkAudioEngine{
		lb:             lb,
		link:           lb.Link(),
		peerName:       peerName,
		send:           send,
		offsetD:        offsetD,
		cfg:            interval.Config{Bars: 4, Quantum: lb.Quantum()},
		tempoBPM:       120,
		capture:        make(map[string]*captureChannel),
		emit:           make(map[affinity.Key]*emitStream),
		intent:         make(map[string]peerIntent),
		loggedRoomSkip: make(map[string]bool),
		own:            affinity.NewOwnChannels(),
		restore:        make(map[CaptureChannelKey]bool),
		peerGoneGrace:  enginePeerGoneGrace(),

		cushionFrames: engineCushionFrames(),
	}
	e.curRoomIdx.Store(math.MinInt64) // unlabeled until the first room anchor
	return e
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
	// Mark stopping under mu before Wait so any concurrent startDrainLocked (which
	// does wg.Add under mu) either already happened-before this, or sees stopping and
	// bails — never a wg.Add racing the Wait below.
	e.mu.Lock()
	e.stopping = true
	e.mu.Unlock()
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
		for _, sk := range st.sinks {
			sk.Close()
		}
	}
	e.emit = make(map[affinity.Key]*emitStream)
	if e.metronome != nil {
		if e.metronome.sink != nil {
			e.metronome.sink.Close()
		}
		e.metronome = nil
		e.metronomeOn.Store(false)
	}
	e.mu.Unlock()

	e.link.EnableLinkAudio(false)
	log.Printf("[audio] Link Audio engine stopped")
}

func (e *linkAudioEngine) SetRoomAnchor(currentIndex int64, bpm float64, bars uint32, quantum float64) {
	// Sample our local interval index now and align the labeler to the room index.
	ss := abllink.NewSessionState()
	defer ss.Close()
	e.link.CaptureAppSessionState(ss)
	clockMicros := e.link.ClockMicros()

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
	}
	localBeat := ss.BeatAtTime(clockMicros, e.cfg.BeatsPerInterval())
	localIdx := e.cfg.IndexAtBeat(localBeat)
	prevOff, wasAligned := e.labeler.Offset(), e.labeler.Aligned()
	e.labeler.Align(currentIndex, localIdx)
	newOff := e.labeler.Offset()
	switch {
	case !wasAligned:
		log.Printf("[audio] room anchor applied: index=%d (bpm %.1f, %d bars x %.0f beats) — label offset %+d at local idx %d",
			currentIndex, e.tempoBPM, e.cfg.Bars, e.cfg.Quantum, newOff, localIdx)
	case newOff != prevOff:
		log.Printf("[audio] room anchor re-aligned: index=%d — label offset %+d → %+d at local idx %d (room labels shift %+d intervals)",
			currentIndex, prevOff, newOff, localIdx, newOff-prevOff)
	}
}

func (e *linkAudioEngine) RoomIndex(localIndex int64) (int64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.labeler.RoomIndex(localIndex)
}

// AlignRoomLabel aligns the labeler to an explicitly derived local index for
// the given room index (ADR-0006 "known by construction": the session
// computed localIndex from the anchor's boundary time on an aligned grid, so
// this never suffers the sample-align/snap off-by-one). Logs only on change —
// re-running entry conformance is idempotent.
// OnGridSnap re-anchors every emit feeder after an entry-conformance grid
// snap (ADR-0006): the snap moved the playhead, not the audio — the jumped
// frames skip silently, never counting as underruns (the join-time ~500k
// "underrun frames" were exactly this setup event misattributed as loss).
func (e *linkAudioEngine) OnGridSnap(deltaUs int64) {
	// Recorded before the early return: a backward snap moves the grid too,
	// so it must still be attributable when the detector sees it.
	e.lastSnapAtNs.Store(time.Now().UnixNano())
	e.lastSnapDeltaUs.Store(deltaUs)
	if deltaUs <= 0 {
		return // backward jump: the playhead pauses; nothing to skip
	}
	frames := int(deltaUs) * engineInternalRate / 1e6
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.emit {
		st.feeder.SkipFrames(frames)
	}
}

func (e *linkAudioEngine) AlignRoomLabel(roomIndex, localIndex int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prevOff, wasAligned := e.labeler.Offset(), e.labeler.Aligned()
	e.labeler.Align(roomIndex, localIndex)
	if newOff := e.labeler.Offset(); !wasAligned || newOff != prevOff {
		log.Printf("[audio] room label aligned by construction: room %d ↔ local %d — label offset %+d (was %+d)",
			roomIndex, localIndex, newOff, prevOff)
	}
}

// abs64 returns |v| for the label-offset comparison below (int64; math.Abs
// is float64). Formerly shared with the grid-alignment glue (now
// internal/align, which has its own copy).
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// LabelOffsetFor returns the worst (largest-|delta|) interval-label verdict
// across one identity's streams: 0 = labels agree with our room index, k = the
// peer's audio silently plays k intervals off (positive = late: frames labeled
// ahead are held extra boundaries by playout). ok is false when no stream has
// finalized a verdict yet (too little data, or unaligned).
func (e *linkAudioEngine) LabelOffsetFor(identity string) (int64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var worst int64
	found := false
	for key, st := range e.emit {
		if key.Identity != identity {
			continue
		}
		if v, ok := st.labelTrack.Verdict(); ok && (!found || abs64(v) > abs64(worst)) {
			worst, found = v, true
		}
	}
	return worst, found
}

func (e *linkAudioEngine) CaptureChannels() []CaptureChannelInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]CaptureChannelInfo, 0, len(e.capture))
	for id, ch := range e.capture {
		// Defense in depth: own republished channels are excluded at discovery
		// (reconcileChannels), but never let one reach the GUI even if that
		// filter leaks — the send-mixer must only show non-WAIL channels.
		if e.own.Own(id, ch.peerName == e.peerName, ch.name) {
			continue
		}
		// Same guard by name prefix — excludes any WAIL peer's room channels,
		// not just our own (reconcileChannels applies the identical rule).
		if strings.HasPrefix(ch.name, affinity.RoomChannelPrefix) {
			continue
		}
		out = append(out, CaptureChannelInfo{
			ChannelID: id, Name: ch.name, PeerName: ch.peerName, Enabled: ch.enabled,
			StreamID: ch.streamID,
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
	key := CaptureChannelKey{PeerName: ch.peerName, ChannelName: ch.name}
	if on {
		e.restore[key] = true
	} else {
		delete(e.restore, key)
	}
	if on && !ch.enabled {
		e.startCaptureLocked(ch)
	} else if !on && ch.enabled {
		e.stopCaptureLocked(ch)
	}
}

// SetCaptureRestore replaces the remembered set of enabled capture channels
// (loaded from disk at session start). Already-discovered channels matching
// the set enable immediately; later discoveries are matched in reconcile.
func (e *linkAudioEngine) SetCaptureRestore(keys []CaptureChannelKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.restore = make(map[CaptureChannelKey]bool, len(keys))
	for _, k := range keys {
		e.restore[k] = true
	}
	for _, ch := range e.capture {
		if !ch.enabled && e.restore[CaptureChannelKey{PeerName: ch.peerName, ChannelName: ch.name}] {
			e.startCaptureLocked(ch)
		}
	}
}

// restoreSet returns a copy of the current restore set (for persistence).
func (e *linkAudioEngine) restoreSet() map[CaptureChannelKey]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[CaptureChannelKey]bool, len(e.restore))
	for k := range e.restore {
		out[k] = true
	}
	return out
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
		// Never surface WAIL room-published channels (our own republished
		// streams, the metronome, or *another* WAIL peer's) as capture inputs —
		// they carry already-relayed room audio, so capturing one re-relays it.
		if strings.HasPrefix(c.Name, affinity.RoomChannelPrefix) {
			// Say so once: a user channel that happens to carry the prefix is
			// excluded by exactly this rule, and silence made that unfindable.
			if !e.loggedRoomSkip[id] {
				e.loggedRoomSkip[id] = true
				log.Printf("[audio] channel %q (peer %q) not offered for capture: the %q prefix marks room-published audio, which WAIL never re-captures",
					c.Name, c.PeerName, affinity.RoomChannelPrefix)
			}
			continue
		}
		seen[id] = true
		ch, ok := e.capture[id]
		if !ok {
			// Register discovered channel as available but disabled; the user
			// enables it via SetCaptureEnabled (explicit opt-in send-mixer) —
			// unless it matches the remembered set, in which case it starts now.
			ch = &captureChannel{id: c.ID, name: c.Name, peerName: c.PeerName, streamID: e.nextStreamID}
			e.nextStreamID++
			e.capture[id] = ch
			if e.restore[CaptureChannelKey{PeerName: c.PeerName, ChannelName: c.Name}] {
				log.Printf("[audio] auto-enabling remembered capture channel %q · %q", c.PeerName, c.Name)
				e.startCaptureLocked(ch)
			}
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
	if !e.startDrainLocked(ch, linkCaptureSource{s: src}, 2) {
		src.Close()
	}
}

// startDrainLocked wires an already-built captureSource into the
// encode→pace→send path and launches its drain goroutine. It returns false —
// leaving ch disabled and the source unadopted, so the caller can release it —
// if the encoder fails to init. mu held.
func (e *linkAudioEngine) startDrainLocked(ch *captureChannel, src captureSource, channels int) bool {
	if ch.enabled || e.stopping {
		return false
	}
	// Channel count is fixed at setup; capture is stereo today.
	enc, err := NewIntervalEncoder(channels, engineInternalRate, engineBitrateKbps)
	if err != nil {
		log.Printf("[audio] encoder init failed for %q: %v", ch.name, err)
		return false
	}
	ch.source = src
	ch.enabled = true
	ch.enc = enc
	ch.asm = capture.NewWindowed(e.cfg, channels, engineInternalRate, samplesPerWaifFrame(engineInternalRate))
	name := ch.name // stable copy: the onDrop callback runs off the pacer goroutine
	ch.pacer = pace.New(sendFrameGap, sendQueueBatches, e.send, func(n int) {
		log.Printf("[audio] WARN: send backlog on %q — dropped a batch of %d frames", name, n)
	})

	dctx, dcancel := context.WithCancel(e.ctx)
	ch.drain = dcancel
	e.wg.Add(1)
	go e.drainCapture(dctx, ch)
	return true
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

	// Snapshot the source once. Teardown may nil/close ch.source under e.mu while
	// this goroutine is mid-tick (we read it lock-free), so hold our own reference:
	// a closed source's Pop/Dropped are safe no-ops, but reading a niled ch.source
	// would panic. Each (re)connection gets its own captureChannel, so this local is
	// never shared with another drain goroutine.
	src := ch.source

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.reconcileDump(ch)
			ch.statRingDropped.Store(src.Dropped())
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
			e.syncCaptureConfig(ch)
			bpi := e.cfgSnapshot().BeatsPerInterval() // stable within a tick; snapshot once, not per buffer
			for {
				buf, beat, mapped, popped := src.PopMapped(ss, bpi)
				if !popped {
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
		sinkName := affinity.FormatRoomChannelName(displayName, label)
		sinks := []emitSink{e.link.NewSink(sinkName, engineInternalRate*2)}
		st = &emitStream{
			key:             key,
			channels:        channels,
			dec:             dec,
			shiftFrames:     dec.Lookahead(),
			reasm:           emit.New(channels, samplesPerWaifFrame(engineInternalRate)),
			sched:           playout.New(e.offsetD),
			sinks:           sinks,
			feeder:          emit.NewFeeder(e.cushionFrames, emitChunkFrames),
			lastDisplayName: displayName,
			lastStreamName:  streamName,
		}
		e.own.Published(sinkName)
		e.emit[key] = st
		log.Printf("[audio] publishing Link Audio channel %q for %s stream %d", sinkName, fromIdentity, f.StreamID)
	} else if st.lastDisplayName != displayName || st.lastStreamName != streamName {
		// Name became known / changed: rename the existing channel in place, don't
		// re-mint it (affinity preserves the channel). All sinks share the name.
		newName := affinity.FormatRoomChannelName(displayName, streamLabel(streamName, f.StreamID))
		for _, sk := range st.sinks {
			sk.SetName(newName)
		}
		e.own.Published(newName)
		st.lastDisplayName = displayName
		st.lastStreamName = streamName
	}
	// Idle clock for retirement. Frames still in flight after a peer drops a
	// stream keep pushing this out, which is exactly the grace's job.
	st.lastFrameAt = time.Now()
	// Label-offset confirmation (ADR-0006 follow-up): bucket this frame's
	// label against our current room index; a verdict change means the peer's
	// labeling shifted — warn when it's nonzero (their audio silently plays
	// that many intervals late/early).
	if roomIdx := e.curRoomIdx.Load(); roomIdx != math.MinInt64 {
		if verdict, changed := st.labelTrack.Add(roomIdx, f.IntervalIndex); changed && verdict != 0 {
			// Frames labeled ahead of our room index (positive) are held extra
			// boundaries by playout (release = label − D) → positive = late.
			dir, n := "late", verdict
			if n < 0 {
				dir, n = "early", -n
			}
			if isLoopbackIdentity(fromIdentity) {
				// Self-echo: "the peer" is this machine, so there is nobody to
				// chase — it is our own capture labels against our own room
				// clock, which is a labeler fault, not a remote one.
				log.Printf("[audio] warn: server-echo loopback stream %d labels intervals off by %+d — our own capture labels disagree with our room clock by %d interval(s)",
					f.StreamID, verdict, n)
			} else {
				log.Printf("[audio] warn: %s stream %d labels intervals off by %+d — their audio plays %d interval(s) %s",
					fromIdentity, f.StreamID, verdict, n, dir)
			}
		}
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
	// Debug-room offset analysis: frame energy at its absolute room-frame
	// position. Cheap (one channel's sum of squares per 20ms frame).
	if st.offsetTrk == nil {
		st.offsetTrk = offset.NewTracker(4000)
	}
	{
		ch := int(f.Channels)
		if ch < 1 {
			ch = 1
		}
		var ss float64
		nf := len(pcm) / ch
		for i := 0; i < nf; i++ {
			v := float64(pcm[i*ch])
			ss += v * v
		}
		rms := 0.0
		if nf > 0 {
			rms = math.Sqrt(ss / float64(nf))
		}
		fpi := int64(FramesPerInterval(e.tempoBPM, e.cfg))
		if fpi <= 0 {
			fpi = 1
		}
		st.offsetTrk.Add(f.IntervalIndex*fpi+int64(f.FrameNumber), rms)
	}
	if !st.haveSeq || !seqLess(f.FrameSeq, st.expectSeq) {
		st.haveSeq = true
		st.expectSeq = f.FrameSeq + 1
		st.lastPos = cur
	}

	e.mu.Lock()
	// The lock was dropped for the decode, so the boundary sweep may have
	// retired this stream meanwhile. Placing the frame in the orphan would
	// lose it and leave the next frame re-minting the channel — don't rely on
	// the grace being long enough to make that unreachable.
	if live, ok := e.emit[key]; !ok || live != st {
		e.mu.Unlock()
		return
	}
	for i, slot := range plcSlots {
		if concealed[i] == nil {
			continue
		}
		if st.sched.OnFrame(slot.Interval) != playout.TooLate {
			st.reasm.AddPLC(slot.Interval, slot.Frame, concealed[i])
			st.framesConcealed.Add(1)
		}
	}
	// Room-label gap: the frame's interval index is the sender's room label,
	// ours is the emit loop's latest. Both tick in lockstep in a healthy room
	// (gap ∈ {-1,0,+1}); a persistent larger gap is an anchor offset mismatch
	// — this stream will sound gap intervals late (positive) or be dropped as
	// too-late (very negative). Edge-triggered + rate-limited per stream.
	if e.lastRoomLabeled.Load() {
		if gap := f.IntervalIndex - e.lastRoomLabel.Load(); gap < -1 || gap > 1 {
			nowNs := time.Now().UnixNano()
			if st.labelGapBand.Load() != gap || nowNs-st.labelGapLogNs.Load() > int64(5*time.Second) {
				st.labelGapBand.Store(gap)
				st.labelGapLogNs.Store(nowNs)
				log.Printf("[audio] WARN: %s stream %d labels run %+d intervals vs our room clock (frame interval %d, our label %d) — anchor offset mismatch; audio ≈%+d intervals off (negative beyond -%d: dropped as too-late)",
					fromIdentity, f.StreamID, gap, f.IntervalIndex, e.lastRoomLabel.Load(), gap, st.sched.Offset()+1)
			}
		}
	}
	switch st.sched.OnFrame(f.IntervalIndex) {
	case playout.TooLate:
		// Interval already finished playing; drop. A nonzero rate here means
		// the sender's labels run behind our playout (anchor offset mismatch).
		st.framesTooLate.Add(1)
	default:
		// Buffer or LiveAppend: place the frame; the paced reader picks it up.
		st.reasm.Add(f.IntervalIndex, int(f.FrameNumber), pcm, f.IsFinal, int(f.TotalFrames))
	}
	e.mu.Unlock()
}

// SetPeerStreams records what one identity says it is still sending (their
// StreamNames sync). Streams of theirs outside keep become retirable; streams
// back inside it are wanted again, which is how a reconnecting peer keeps its
// channel. An empty keep means they send nothing — not "unknown".
func (e *linkAudioEngine) SetPeerStreams(identity string, keep map[uint16]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	own := make(map[uint16]bool, len(keep))
	for id := range keep {
		own[id] = true
	}
	e.intent[identity] = peerIntent{keep: own}
}

// ClearPeerIntent forgets what we were told about an identity, putting it back
// to "no news" — nothing of theirs is retirable until they say so again. The
// counterpart to DropPeer for a source that will never send StreamNames (the
// loopback echo), where the drop would otherwise stay in force forever.
func (e *linkAudioEngine) ClearPeerIntent(identity string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.intent, identity)
}

// DropPeer marks everything an identity publishes as retirable on the longer
// peer-gone grace: they left the room, or (for the ":loopback" identity) the
// server echo was switched off. A frame arriving later, or a fresh
// SetPeerStreams, revives them well inside the grace.
func (e *linkAudioEngine) DropPeer(identity string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.intent[identity] = peerIntent{gone: true}
}

// retirableLocked reports whether this stream's owner has stopped claiming it,
// and the grace that applies. No recorded intent means we have not heard
// otherwise — the stream stays.
func (e *linkAudioEngine) retirableLocked(key affinity.Key) (time.Duration, bool) {
	in, ok := e.intent[key.Identity]
	switch {
	case !ok:
		return 0, false
	case in.gone:
		return e.peerGoneGrace, true
	case !in.keep[key.Stream]:
		return retireGraceDropped, true
	}
	return 0, false
}

// sweepRetiredLocked closes and forgets streams whose owner has stopped
// claiming them, once they are idle past their grace AND have nothing left
// buffered. The drained check is what keeps this from truncating the tail of
// the last interval: playout runs D boundaries behind the final frame, so a
// grace shorter than one interval would otherwise cut audio mid-flight.
// Boundary-aligned (called from onBoundary under mu) so a sink never closes
// while topUpSinks is feeding it. Caller holds e.mu.
// roomLabel is the boundary being processed, which is also the horizon's
// reference: this runs before the release loop advances each scheduler, so
// reading the cursor from the schedulers here would measure the *previous*
// boundary and retire one interval sooner than intended.
func (e *linkAudioEngine) sweepRetiredLocked(now time.Time, roomLabel int64) {
	for key, st := range e.emit {
		grace, ok := e.retirableLocked(key)
		if !ok {
			continue
		}
		if now.Sub(st.lastFrameAt) < grace {
			continue
		}
		// "Drained" has to mean nothing *imminent*, not nothing at all. A
		// sender whose labels run ahead of our playout leaves stragglers
		// buffered that far ahead, and Drop only clears at or below the release
		// cursor — so asking for an empty reassembler keeps a dead channel
		// published for as many boundaries as that peer is mislabeled. Anything
		// at or below the horizon still blocks: an interval is released at
		// boundary index+D and dropped one boundary later, so the interval that
		// just played is still held here and must not be cut.
		backlog := 0
		if min, buffered := st.reasm.MinIndex(); buffered {
			if min <= roomLabel+retireHorizonIntervals {
				continue
			}
			// Past the horizon: this audio is not imminent, and retiring
			// discards it. Count it so the log can say so — silently dropping
			// a decoded backlog is exactly the kind of thing that should not
			// have to be inferred later.
			if max, ok := st.reasm.MaxIndex(); ok {
				backlog = int(max-min) + 1
			}
		}
		e.retireStreamLocked(key, st, backlog)
	}
	// Forget intent for identities with nothing published left, so the map
	// tracks the room rather than growing for the session's lifetime.
	for identity := range e.intent {
		used := false
		for key := range e.emit {
			if key.Identity == identity {
				used = true
				break
			}
		}
		if !used {
			delete(e.intent, identity)
		}
	}
}

// retireStreamLocked unpublishes one stream: its Link Audio channel leaves the
// LAN (so every WAIL Receive frees the port it held), its counters fold into
// the engine's retired totals, and it drops out of e.emit. Caller holds e.mu.
// backlog is how many buffered intervals go with it — nonzero only on the
// horizon path, where the stream still held audio too far ahead to be
// imminent. The log has to name that, or a discarded decode looks from the
// outside like the sender simply stopping.
func (e *linkAudioEngine) retireStreamLocked(key affinity.Key, st *emitStream, backlog int) {
	for _, sk := range st.sinks {
		sk.Close()
	}
	e.retired.EmitIntervalsIncomplete += st.intervalsIncomplete.Load()
	e.retired.EmitSinkUnderrunEvents += st.sinkUnderrunEvents.Load()
	e.retired.EmitSinkUnderrunFrames += st.sinkUnderrunFrames.Load()
	e.retired.EmitFramesMissingAtPlay += st.framesMissedAtPlay.Load()
	e.retired.EmitFramesConcealed += st.framesConcealed.Load()
	e.retired.EmitFramesTooLate += st.framesTooLate.Load()
	e.retired.EmitSinkWriteRejected += st.sinkWriteRejected.Load()
	e.retired.OpusDecodeFailures += st.decodeFailures.Load()
	delete(e.emit, key)
	name := affinity.FormatRoomChannelName(st.lastDisplayName, streamLabel(st.lastStreamName, key.Stream))
	if backlog > 0 {
		log.Printf("[audio] retired Link Audio channel %q (%s stream %d — sender stopped publishing it; discarded %d buffered interval(s) labeled too far ahead to play)",
			name, key.Identity, key.Stream, backlog)
		return
	}
	log.Printf("[audio] retired Link Audio channel %q (%s stream %d — sender stopped publishing it)",
		name, key.Identity, key.Stream)
}

// TakeGridJump reports a local Link grid jump detected since the last call,
// clearing it. The session polls this rather than the engine calling into the
// steerer directly: the detector runs on the emit loop, and alignment state
// belongs to the session goroutine.
func (e *linkAudioEngine) TakeGridJump() (GridJump, bool) {
	j := e.gridJump.Swap(nil)
	if j == nil {
		return GridJump{}, false
	}
	return *j, true
}

// isGridJump reports whether the beat clock moved by more than elapsed time
// can explain. bpm must be the tempo the beat is actually advancing at — the
// session's, not the room's, which differ whenever a peer holds the session
// elsewhere. A tick spaced further apart than jumpDetectMaxGapSec is not
// judged at all: the loop stalled, so the expectation is guesswork, and a
// false positive costs an audible re-entry snap.
func isGridJump(deltaBeats, elapsedSec, bpm float64) bool {
	if elapsedSec <= 0 || elapsedSec >= jumpDetectMaxGapSec || bpm <= 0 {
		return false
	}
	return math.Abs(deltaBeats-elapsedSec*bpm/60.0) > 0.5
}

// noteGridJump hands a jump to the session — unless we caused it ourselves.
// Our own snap moves the grid on purpose: re-entering conformance over it
// would re-measure a grid we just aligned, and reporting it would send the
// room hunting a merge that never happened.
func (e *linkAudioEngine) noteGridJump(j GridJump) {
	if j.SelfCaused {
		return
	}
	e.gridJump.Store(&j)
}

// classifyGridJump attributes a beat discontinuity to whatever evidence the
// tick carries. Order matters: our own snap first (we know we did it), then a
// changed peer set (a session merge re-phases the timeline), then a tempo
// move. Anything left is reported as unattributed rather than guessed at.
func (e *linkAudioEngine) classifyGridJump(beats, roomBPM, sessionBPM float64, peers uint64, ev jumpEvidence, bpi float64, now time.Time) GridJump {
	ms := 0.0
	if roomBPM > 0 {
		ms = beats * 60_000 / roomBPM
	}
	j := GridJump{Beats: beats, Ms: ms, Intervals: int64(math.Round(beats / bpi)), Peers: peers}

	switch {
	case e.matchesOwnSnap(ms, now):
		j.SelfCaused = true
		j.Cause = "our own grid snap"
		j.Detail = fmt.Sprintf("entry conformance snapped %+.0f ms, which is this jump; peers=%d tempo=%.2f BPM",
			float64(e.lastSnapDeltaUs.Load())/1000, peers, sessionBPM)
	case !ev.peersChangedAt.IsZero() && now.Sub(ev.peersChangedAt) < gridJumpEvidenceWindow:
		j.Cause = "Link session merge"
		j.Detail = fmt.Sprintf("LAN peer count %d→%d, %s before the jump — a joining peer re-phased the shared timeline; tempo=%.2f BPM",
			ev.peersFrom, peers, now.Sub(ev.peersChangedAt).Round(time.Millisecond), sessionBPM)
	case !ev.tempoChangedAt.IsZero() && now.Sub(ev.tempoChangedAt) < gridJumpEvidenceWindow:
		j.Cause = "session tempo change"
		j.Detail = fmt.Sprintf("session tempo %.2f→%.2f BPM, %s before the jump (room %.2f); peers=%d",
			ev.tempoFrom, sessionBPM, now.Sub(ev.tempoChangedAt).Round(time.Millisecond), roomBPM, peers)
	default:
		j.Cause = "unattributed"
		j.Detail = fmt.Sprintf("no peer or tempo change in the preceding %s (peers=%d, tempo=%.2f BPM) — a transport reset or an external ForceBeatAtTime",
			gridJumpEvidenceWindow, peers, sessionBPM)
	}
	return j
}

// matchesOwnSnap reports whether a jump is the grid snap we just performed —
// recent AND of the same magnitude. Recency alone is not evidence: a snap
// smaller than the detector's own 0.5-beat threshold produces no jump at all,
// yet would blanket-absorb a real merge arriving inside the window, withholding
// it so nothing ever re-aligns. The record is consumed, so one snap can explain
// at most one jump.
func (e *linkAudioEngine) matchesOwnSnap(ms float64, now time.Time) bool {
	at := e.lastSnapAtNs.Load()
	if at == 0 || now.Sub(time.Unix(0, at)) >= gridSnapAttributionWindow {
		return false
	}
	snapMs := math.Abs(float64(e.lastSnapDeltaUs.Load()) / 1000)
	if math.Abs(snapMs-math.Abs(ms)) > math.Max(snapMatchToleranceMs, 0.2*snapMs) {
		return false
	}
	e.lastSnapAtNs.Store(0)
	return true
}

// Health sums the per-channel and per-stream diagnostic counters. Capture-side
// values are drain-goroutine mirrors (atomics); emit-side counters are atomics
// on their streams; the maps themselves are guarded by e.mu.
func (e *linkAudioEngine) Health() EngineHealth {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Seed with retired streams' counters so the totals only ever climb: the
	// session logs a counter only when it exceeds the previous snapshot, so a
	// dip would silently swallow every later event until it recovered.
	h := e.retired
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
		h.EmitFramesTooLate += st.framesTooLate.Load()
		h.EmitSinkWriteRejected += st.sinkWriteRejected.Load()
		h.OpusDecodeFailures += st.decodeFailures.Load()
	}
	h.WireDecodeFailures = e.wireDecodeFailures.Load()
	h.EmitCushionMs = cushionMsFromFrames(e.cushionFrames)
	// Debug-room stream offsets: each stream's measured rhythmic phase offset
	// vs the room grid (internal/offset). Only computed for streams with
	// enough buffered frames to judge; absent otherwise.
	beatMs := 0.0
	if e.tempoBPM > 0 {
		beatMs = 60000 / e.tempoBPM
	}
	if beatMs > 0 {
		for _, st := range e.emit {
			if st.offsetTrk == nil || st.offsetTrk.Len() < 400 {
				continue
			}
			if ms, ok := st.offsetTrk.Offset(20, beatMs); ok {
				h.StreamOffsets = append(h.StreamOffsets, StreamOffset{
					Name: affinity.FormatName(st.lastDisplayName, st.lastStreamName),
					Ms:   ms,
				})
			}
		}
		sort.Slice(h.StreamOffsets, func(i, j int) bool { return h.StreamOffsets[i].Name < h.StreamOffsets[j].Name })
	}
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
	var lastBeat, lastBeatAt float64
	haveLastBeat := false
	var lastPeers uint64
	var lastSessionBPM float64
	var ev jumpEvidence

	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			e.link.CaptureAppSessionState(ss)
			clockMicros := e.link.ClockMicros()
			// Attribution evidence, sampled every tick so a jump can be
			// explained rather than merely reported.
			peers := e.link.NumPeers()
			sessionBPM := ss.Tempo()

			e.mu.Lock()
			cfg := e.cfg
			tempo := e.tempoBPM
			bpi := cfg.BeatsPerInterval()
			localBeat := ss.BeatAtTime(clockMicros, bpi)
			localIdx := cfg.IndexAtBeat(localBeat)
			roomLabel, labeled := e.labeler.RoomIndex(localIdx)
			if labeled {
				e.curRoomIdx.Store(roomLabel)
			}
			e.mu.Unlock()

			if labeled {
				e.lastRoomLabel.Store(roomLabel)
				e.lastRoomLabeled.Store(true)
			}
			// Local-grid jump detection: the beat should advance by exactly
			// elapsed-wall × tempo between ticks. Anything else is a Link
			// session merge/transport reset — which silently invalidates the
			// labeler offset until the next anchor (the stable-multi-interval-
			// delay failure mode seen in the field).
			// Expected against the SESSION tempo, which is what the beat
			// actually advances at — the room tempo can differ while a peer
			// holds the session elsewhere, and then any delayed tick reads as
			// a jump. A stalled tick is skipped outright rather than measured:
			// after a GC pause or a starved scheduler on a loaded DAW machine
			// the expectation is not trustworthy, and a false jump costs an
			// audible re-entry snap.
			elapsed := float64(clockMicros)/1e6 - lastBeatAt
			beatBPM := sessionBPM
			if beatBPM <= 0 {
				beatBPM = tempo
			}
			if d := localBeat - lastBeat; haveLastBeat && isGridJump(d, elapsed, beatBPM) {
				{
					jump := e.classifyGridJump(d, beatBPM, sessionBPM, peers, ev, bpi, time.Now())
					log.Printf("[audio] WARN: local Link beat jumped %+.2f beats (%+.0f ms, expected %+.2f from elapsed time) — cause: %s; %s",
						jump.Beats, jump.Ms, elapsed*beatBPM/60.0, jump.Cause, jump.Detail)
					// Our own snap moves the grid on purpose. Re-entering
					// conformance over it would re-measure a grid we just
					// aligned, and reporting it as a jump would send everyone
					// hunting a merge that never happened.
					e.noteGridJump(jump)
				}
			}
			lastBeat, lastBeatAt, haveLastBeat = localBeat, float64(clockMicros)/1e6, true
			// Remember *when* the evidence last moved, not just the previous
			// tick's values: peer discovery and the timeline merge it causes
			// are tens of milliseconds apart, so comparing across one tick
			// reports the merge it exists to name as "unattributed".
			if haveLastBeat && peers != lastPeers {
				ev.peersChangedAt, ev.peersFrom = time.Now(), lastPeers
			}
			if haveLastBeat && math.Abs(sessionBPM-lastSessionBPM) > 0.01 {
				ev.tempoChangedAt, ev.tempoFrom = time.Now(), lastSessionBPM
			}
			lastPeers, lastSessionBPM = peers, sessionBPM

			if labeled && (!haveLast || localIdx > lastLocalIdx) {
				e.onBoundary(cfg, tempo, localIdx, roomLabel)
				lastLocalIdx = localIdx
				haveLast = true
			}
			e.topUpSinks(ss, bpi, localBeat)
			// The metronome runs on the local Link grid, independent of the
			// labeler that gates onBoundary — it works before a room anchor.
			if e.metronomeOn.Load() {
				e.metronomeTick(ss, cfg, tempo, bpi, localBeat, localIdx)
			}
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
	e.sweepRetiredLocked(time.Now(), roomLabel)
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

		// Buffered-ahead gap: a healthy sender streams in real time, so at
		// release the reassembler holds at most ~D intervals of future audio.
		// More means the sender's labels run ahead of our playout — the same
		// anchor mismatch as the label-gap warn, measured at release time.
		if maxBuf, ok := st.reasm.MaxIndex(); ok {
			if g := maxBuf - idx; g > st.sched.Offset()+2 {
				st.gapLogs++
				if st.gapLogs == 1 || st.gapLogs%50 == 0 {
					log.Printf("[audio] WARN: %v buffered through room interval %d while releasing %d — sender labels %d intervals ahead of playout (anchor offset mismatch)",
						st.key, maxBuf, idx, g-st.sched.Offset())
				}
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
				func() []int16 { return st.shiftedInterval(nextIdx) },
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
				func() []int16 { return st.shiftedInterval(idx) },
				st.channels, engineInternalRate, tempo, startBeat, totalFrames,
			), makeNext)
		}
	}
}

// shiftedInterval returns the stream's interval PCM realigned by the codec
// lookahead: the Opus codec delays every stream by OPUS_GET_LOOKAHEAD
// (~6.5ms — the mini-DAW harness measured every transient landing late), so
// the emit path reads the reassembly back shifted by that amount; the tail
// pulls the next interval's head (silence until it arrives — play-partial
// covers late frames). Scratch-backed; the PacedReader copies out per Next.
func (st *emitStream) shiftedInterval(idx int64) []int16 {
	s, _, _, ok := st.reasm.Interval(idx)
	if !ok {
		return nil
	}
	if len(st.shiftScratch) < len(s) {
		st.shiftScratch = make([]int16, len(s))
	}
	return st.reasm.ShiftedPCM(st.shiftScratch[:len(s)], idx, idx+1, st.shiftFrames)
}

// topUpSinks advances each stream's feeder to playhead+cushion, writing paced
// chunks into its sink (multiple chunks per tick when catching up after a stall).
func (e *linkAudioEngine) topUpSinks(ss *abllink.SessionState, bpi, localBeat float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, st := range e.emit {
		if len(st.sinks) == 0 {
			continue
		}
		st.feeder.Advance(localBeat, func(samples []int16, beat float64) {
			frames := len(samples) / st.channels
			// sinks[0] is the Link Audio channel; anchor the write-rejected
			// metric on it so its meaning is unchanged from the single-sink path.
			linkOK := false
			for i, sk := range st.sinks {
				ok := sk.WriteInterleaved(samples, ss, beat, bpi, frames, st.channels, engineInternalRate)
				if i == 0 {
					linkOK = ok
				}
			}
			// The feeder's cursor has moved past this chunk either way, so a
			// refusal while a listener was streaming is a permanent hole.
			if !linkOK && st.lastWriteOK {
				st.sinkWriteRejected.Add(1)
			}
			st.lastWriteOK = linkOK
		})
		ev, fr := st.feeder.Underruns()
		st.sinkUnderrunEvents.Store(ev)
		st.sinkUnderrunFrames.Store(fr)
	}
}

// SetMetronome publishes or tears down the room metronome Link Audio channel
// (named "WAIL · {peer} · Metronome" so it carries the room-channel prefix).
// The sink is registered as our own so capture discovery won't offer it back as
// an input (a feedback loop). No-op if already in the requested state.
func (e *linkAudioEngine) SetMetronome(enabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if enabled {
		if e.metronome != nil {
			return
		}
		name := affinity.FormatRoomChannelName(e.peerName, "Metronome")
		e.own.Published(name)
		e.metronome = &metronomeStream{
			sink:     e.link.NewSink(name, engineInternalRate*2),
			feeder:   emit.NewFeeder(e.cushionFrames, emitChunkFrames),
			channels: 2,
		}
		e.metronomeOn.Store(true)
		log.Printf("[audio] metronome on (channel %q)", name)
		return
	}
	if e.metronome == nil {
		return
	}
	e.metronomeOn.Store(false)
	if e.metronome.sink != nil {
		e.metronome.sink.Close()
	}
	e.metronome = nil
	log.Printf("[audio] metronome off")
}

// metronomeTick installs the click reader for a new local interval (pre-rolling
// the next so boundaries don't reset the cushion) and paces it into the sink.
// It mirrors onBoundary + topUpSinks minus the reasm/sched bookkeeping (a
// metronome decodes nothing) and tracks its own installed index, so re-enabling
// mid-interval starts the click immediately. Each interval renders fresh, so
// there is no continuation-padding handoff (start offset always 0).
func (e *linkAudioEngine) metronomeTick(ss *abllink.SessionState, cfg interval.Config, tempo, bpi, localBeat float64, localIdx int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.metronome
	if m == nil || m.sink == nil {
		return
	}
	startBeat, endBeat := cfg.BeatWindow(localIdx)
	totalFrames := intervalPlayoutFrames(cfg, engineInternalRate, tempo)
	channels := m.channels
	newReader := func(idx int64, base float64) *emit.PacedReader {
		buf := metronome.RenderInterval(cfg, tempo, engineInternalRate, channels, idx)
		return emit.NewPacedReader(func() []int16 { return buf }, channels, engineInternalRate, tempo, base, totalFrames)
	}
	makeNext := func() (*emit.PacedReader, int64, int) {
		return newReader(localIdx+1, endBeat), localIdx + 1, 0
	}
	switch {
	case !m.haveReader:
		m.feeder.SetCurrent(localIdx, newReader(localIdx, startBeat), makeNext)
		m.installedIdx, m.haveReader = localIdx, true
	case localIdx > m.installedIdx:
		if m.feeder.Promote(localIdx, makeNext) {
			if cur := m.feeder.Current(); cur != nil && cur.TempoBPM() != tempo {
				cur.Rebase(tempo, totalFrames)
			}
		} else {
			m.feeder.SetCurrent(localIdx, newReader(localIdx, startBeat), makeNext)
		}
		m.installedIdx = localIdx
	}
	m.feeder.Advance(localBeat, func(samples []int16, beat float64) {
		frames := len(samples) / channels
		m.sink.WriteInterleaved(samples, ss, beat, bpi, frames, channels, engineInternalRate)
	})
}

// syncCaptureConfig re-grids ch's assembler when the room interval config has
// changed under it (SetRoomAnchor adopts the relay's bars/quantum engine-wide).
// The assembler is created once at channel start with the config of that
// moment; left stale, its labels tick at the old grid's rate against a room
// clock on the new one and the stream drifts out of sync every interval.
// Runs on ch's drain goroutine (owner of ch.asm).
func (e *linkAudioEngine) syncCaptureConfig(ch *captureChannel) {
	if ch.asm == nil {
		return
	}
	if old, new := ch.asm.Config(), e.cfgSnapshot(); old != new {
		log.Printf("[audio] capture %q: room interval changed %d bars x %.0f → %d x %.0f — re-gridding assembler (old partial interval dropped)",
			ch.name, old.Bars, old.Quantum, new.Bars, new.Quantum)
		ch.asm.SetConfig(new)
	}
}

// cfgSnapshot returns the room interval config for the audio path's Link
// timing calls. The whole interval (BPI), not the bar, is the beat lens: Link
// pins beat phase only mod the quantum it is asked for, so asking at the bar
// left which-bar-of-the-interval per-peer arbitrary — audio landed bar-aligned
// but bars apart.
func (e *linkAudioEngine) cfgSnapshot() interval.Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
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
