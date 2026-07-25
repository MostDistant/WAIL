package main

// Loopback-TCP IPC between the WAIL app and the CLAP Send/Recv plugins (ADR-0005).
// The plugin is a thin raw-PCM bridge: the app owns all Opus/WAIF/interval/relay
// logic, so — unlike the retired plugin era — nothing here carries encoded audio.
// The app is the server; plugins are clients that auto-reconnect.
//
// This file is deliberately cgo-free (stdlib only) so the wire codec builds and
// unit-tests under -tags linkstub. The engine-facing adapters that turn these
// messages into captureSource/emitSink live in cgo-tagged files.

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// maxIPCFrameSize caps a single framed payload (16 MB): a garbage length prefix
// must not trigger a huge allocation. One PCM block is far smaller, so any frame
// past this is a protocol violation and the connection should be dropped.
const maxIPCFrameSize = 16 << 20

// IPC role bytes sent by a plugin as the first byte on connect. RecvV2 is the
// same role with a versioned wire appetite: the app sends RemotePCM2 (beat
// stamps) only to v2 connections. A new role byte (not a trailing version
// byte) keeps negotiation deterministic — no deadline racing the metrics tick.
const (
	IPCRoleSend   byte = 0x00
	IPCRoleRecv   byte = 0x01
	IPCRoleRecvV2 byte = 0x02
)

// IPC message tags. 0x10+ are the raw-PCM tags of the revived protocol; 0x06
// (Metrics) keeps its old value. The retired WAIF/Opus tags (0x01–0x05) are gone.
const (
	IPCTagRawPCM     byte = 0x10 // Send → App: one block of captured PCM
	IPCTagRemotePCM  byte = 0x11 // App → Recv: decoded PCM for one remote stream
	IPCTagStreamName byte = 0x12 // App → Recv: display name for a stream's output port
	IPCTagStreamGone byte = 0x13 // App → Recv: a stream ended; free its port
	IPCTagTrackName  byte = 0x14 // Send → App: DAW track name for a plugin stream
	IPCTagRemotePCM2 byte = 0x15 // App → Recv: RemotePCM + Link beat stamp (v2 role)
	IPCTagMetrics    byte = 0x06 // Plugin → App: cumulative drop counter
)

// RawPCM flag bits.
const (
	IPCRawFlagInt16   byte = 0x01 // pcm payload is int16 LE (else float32 LE)
	IPCRawFlagPlaying byte = 0x02 // host transport was rolling for this block
)

const ipcRawPCMHeaderLen = 17 // tag(1)+streamIndex(2)+flags(1)+channels(1)+rate(4)+frameCounter(8)

// EncodeFrame wraps a payload in a length-prefixed IPC frame: [u32 LE length][payload].
func EncodeFrame(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	return frame
}

// DecodeFrame extracts one complete frame from buf. Returns (payload, consumed,
// ok); ok is false when more bytes are still needed. It does not enforce the size
// cap — IPCRecvBuffer.NextFrame does, so it can surface an unrecoverable error.
func DecodeFrame(buf []byte) ([]byte, int, bool) {
	if len(buf) < 4 {
		return nil, 0, false
	}
	payloadLen := int(binary.LittleEndian.Uint32(buf[:4]))
	total := 4 + payloadLen
	if len(buf) < total {
		return nil, 0, false
	}
	payload := make([]byte, payloadLen)
	copy(payload, buf[4:total])
	return payload, total, true
}

// IPCRecvBuffer accumulates bytes from socket reads and yields complete frames.
type IPCRecvBuffer struct {
	buf []byte
}

// NewIPCRecvBuffer creates a new receive buffer.
func NewIPCRecvBuffer() *IPCRecvBuffer {
	return &IPCRecvBuffer{buf: make([]byte, 0, 64*1024)}
}

// Push appends received bytes.
func (r *IPCRecvBuffer) Push(data []byte) {
	r.buf = append(r.buf, data...)
}

// NextFrame extracts the next complete frame. It returns (nil, nil) when more
// bytes are needed, and a non-nil error when the pending frame's length prefix
// exceeds maxIPCFrameSize — an unrecoverable framing error; the caller should
// close the connection.
func (r *IPCRecvBuffer) NextFrame() ([]byte, error) {
	if len(r.buf) >= 4 {
		if n := binary.LittleEndian.Uint32(r.buf[:4]); int64(n) > maxIPCFrameSize {
			return nil, fmt.Errorf("ipc: frame length %d exceeds max %d", n, maxIPCFrameSize)
		}
	}
	payload, consumed, ok := DecodeFrame(r.buf)
	if !ok {
		return nil, nil
	}
	r.buf = r.buf[consumed:]
	return payload, nil
}

// Buffered returns the number of unconsumed bytes.
func (r *IPCRecvBuffer) Buffered() int {
	return len(r.buf)
}

// IPCTag returns the tag byte from a payload, or -1 if empty.
func IPCTag(payload []byte) int {
	if len(payload) == 0 {
		return -1
	}
	return int(payload[0])
}

// --- raw-PCM message codecs ---

// EncodeRawPCM encodes a Send → App block: fixed header + opaque pcm bytes
// (float32 LE, or int16 LE when IPCRawFlagInt16 is set — the adapter interprets).
func EncodeRawPCM(streamIndex uint16, flags, channels byte, sampleRate uint32, frameCounter uint64, pcm []byte) []byte {
	msg := make([]byte, ipcRawPCMHeaderLen+len(pcm))
	msg[0] = IPCTagRawPCM
	binary.LittleEndian.PutUint16(msg[1:3], streamIndex)
	msg[3] = flags
	msg[4] = channels
	binary.LittleEndian.PutUint32(msg[5:9], sampleRate)
	binary.LittleEndian.PutUint64(msg[9:17], frameCounter)
	copy(msg[ipcRawPCMHeaderLen:], pcm)
	return msg
}

// RawPCM is a decoded Send → App block; Samples is the still-opaque pcm payload.
type RawPCM struct {
	StreamIndex  uint16
	Flags        byte
	Channels     byte
	SampleRate   uint32
	FrameCounter uint64
	Samples      []byte
}

// DecodeRawPCM decodes a RawPCM message. ok is false on a short or mistagged frame.
func DecodeRawPCM(payload []byte) (RawPCM, bool) {
	if len(payload) < ipcRawPCMHeaderLen || payload[0] != IPCTagRawPCM {
		return RawPCM{}, false
	}
	pcm := make([]byte, len(payload)-ipcRawPCMHeaderLen)
	copy(pcm, payload[ipcRawPCMHeaderLen:])
	return RawPCM{
		StreamIndex:  binary.LittleEndian.Uint16(payload[1:3]),
		Flags:        payload[3],
		Channels:     payload[4],
		SampleRate:   binary.LittleEndian.Uint32(payload[5:9]),
		FrameCounter: binary.LittleEndian.Uint64(payload[9:17]),
		Samples:      pcm,
	}, true
}

// EncodeRemotePCM encodes an App → Recv block for one remote stream.
// playAtMicros is the machine-monotonic time (wail_mono_micros domain) at
// which the first frame should play: the app converts each chunk's Link-beat
// stamp into this domain (it can read both clocks; the plugin can't), and a
// transport-aware plugin renders the chunk at that instant against its host
// sample clock. Zero means "no stamp" — the plugin plays FIFO (old apps,
// transport-stopped fallback). Samples are int16 LE.
func EncodeRemotePCM(peerID string, streamID uint16, channels byte, sampleRate uint32, playAtMicros int64, samples []int16) []byte {
	msg := make([]byte, 0, 1+1+len(peerID)+2+1+4+8+2*len(samples))
	msg = append(msg, IPCTagRemotePCM)
	msg = appendStr8(msg, peerID)
	msg = binary.LittleEndian.AppendUint16(msg, streamID)
	msg = append(msg, channels)
	msg = binary.LittleEndian.AppendUint32(msg, sampleRate)
	msg = binary.LittleEndian.AppendUint64(msg, uint64(playAtMicros))
	for _, s := range samples {
		msg = binary.LittleEndian.AppendUint16(msg, uint16(s))
	}
	return msg
}

// EncodeRemotePCM2 encodes the v2 App → Recv block: everything RemotePCM
// carries plus the chunk's Link beat (f64) for transport-phase alignment.
// Header is 23 bytes: tag(1)+peerID(1+n)+streamID(2)+channels(1)+rate(4)
// +playAtMicros(8)+beat(8).
func EncodeRemotePCM2(peerID string, streamID uint16, channels byte, sampleRate uint32, playAtMicros int64, beat float64, samples []int16) []byte {
	msg := make([]byte, 0, 1+1+len(peerID)+2+1+4+8+8+2*len(samples))
	msg = append(msg, IPCTagRemotePCM2)
	msg = appendStr8(msg, peerID)
	msg = binary.LittleEndian.AppendUint16(msg, streamID)
	msg = append(msg, channels)
	msg = binary.LittleEndian.AppendUint32(msg, sampleRate)
	msg = binary.LittleEndian.AppendUint64(msg, uint64(playAtMicros))
	msg = binary.LittleEndian.AppendUint64(msg, math.Float64bits(beat))
	for _, s := range samples {
		msg = binary.LittleEndian.AppendUint16(msg, uint16(s))
	}
	return msg
}

// RemotePCM is a decoded App → Recv block. Beat is 0 for v1 frames (absent).
type RemotePCM struct {
	PeerID       string
	StreamID     uint16
	Channels     byte
	SampleRate   uint32
	PlayAtMicros int64
	Beat         float64
	Samples      []int16
}

// DecodeRemotePCM decodes a RemotePCM message. ok is false on a malformed frame.
func DecodeRemotePCM(payload []byte) (RemotePCM, bool) {
	if len(payload) < 1 || payload[0] != IPCTagRemotePCM {
		return RemotePCM{}, false
	}
	peerID, rest, ok := readStr8(payload[1:])
	if !ok || len(rest) < 15 {
		return RemotePCM{}, false
	}
	streamID := binary.LittleEndian.Uint16(rest[0:2])
	channels := rest[2]
	sampleRate := binary.LittleEndian.Uint32(rest[3:7])
	playAtMicros := int64(binary.LittleEndian.Uint64(rest[7:15]))
	pcm := rest[15:]
	if len(pcm)%2 != 0 {
		return RemotePCM{}, false
	}
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	return RemotePCM{
		PeerID:       peerID,
		StreamID:     streamID,
		Channels:     channels,
		SampleRate:   sampleRate,
		PlayAtMicros: playAtMicros,
		Samples:      samples,
	}, true
}

// DecodeRemotePCM2 decodes a v2 RemotePCM block. ok is false on a malformed frame.
func DecodeRemotePCM2(payload []byte) (RemotePCM, bool) {
	if len(payload) < 1 || payload[0] != IPCTagRemotePCM2 {
		return RemotePCM{}, false
	}
	peerID, rest, ok := readStr8(payload[1:])
	if !ok || len(rest) < 23 {
		return RemotePCM{}, false
	}
	streamID := binary.LittleEndian.Uint16(rest[0:2])
	channels := rest[2]
	sampleRate := binary.LittleEndian.Uint32(rest[3:7])
	playAtMicros := int64(binary.LittleEndian.Uint64(rest[7:15]))
	beat := math.Float64frombits(binary.LittleEndian.Uint64(rest[15:23]))
	pcm := rest[23:]
	if len(pcm)%2 != 0 {
		return RemotePCM{}, false
	}
	samples := make([]int16, len(pcm)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	return RemotePCM{
		PeerID:       peerID,
		StreamID:     streamID,
		Channels:     channels,
		SampleRate:   sampleRate,
		PlayAtMicros: playAtMicros,
		Beat:         beat,
		Samples:      samples,
	}, true
}

// EncodeStreamName encodes an App → Recv display-name update for a stream's port.
func EncodeStreamName(peerID string, streamID uint16, name string) []byte {
	msg := make([]byte, 0, 1+1+len(peerID)+2+2+len(name))
	msg = append(msg, IPCTagStreamName)
	msg = appendStr8(msg, peerID)
	msg = binary.LittleEndian.AppendUint16(msg, streamID)
	msg = appendStr16(msg, name)
	return msg
}

// DecodeStreamName decodes a StreamName message. Returns (peerID, streamID, name, ok).
func DecodeStreamName(payload []byte) (string, uint16, string, bool) {
	if len(payload) < 1 || payload[0] != IPCTagStreamName {
		return "", 0, "", false
	}
	peerID, rest, ok := readStr8(payload[1:])
	if !ok || len(rest) < 2 {
		return "", 0, "", false
	}
	streamID := binary.LittleEndian.Uint16(rest[0:2])
	name, _, ok := readStr16(rest[2:])
	if !ok {
		return "", 0, "", false
	}
	return peerID, streamID, name, true
}

// EncodeStreamGone encodes an App → Recv "stream ended, free its port" message.
func EncodeStreamGone(peerID string, streamID uint16) []byte {
	msg := make([]byte, 0, 1+1+len(peerID)+2)
	msg = append(msg, IPCTagStreamGone)
	msg = appendStr8(msg, peerID)
	msg = binary.LittleEndian.AppendUint16(msg, streamID)
	return msg
}

// DecodeStreamGone decodes a StreamGone message. Returns (peerID, streamID, ok).
func DecodeStreamGone(payload []byte) (string, uint16, bool) {
	if len(payload) < 1 || payload[0] != IPCTagStreamGone {
		return "", 0, false
	}
	peerID, rest, ok := readStr8(payload[1:])
	if !ok || len(rest) < 2 {
		return "", 0, false
	}
	return peerID, binary.LittleEndian.Uint16(rest[0:2]), true
}

// EncodeTrackName encodes a Send → App track-name update: the name of the DAW
// track the Send plugin is inserted on (from the host's clap.track-info), used
// as the stream's default label. Sent once after connect and again on rename.
func EncodeTrackName(streamIndex uint16, name string) []byte {
	msg := make([]byte, 0, 1+2+2+len(name))
	msg = append(msg, IPCTagTrackName)
	msg = binary.LittleEndian.AppendUint16(msg, streamIndex)
	msg = appendStr16(msg, name)
	return msg
}

// DecodeTrackName decodes a TrackName message. Returns (streamIndex, name, ok).
func DecodeTrackName(payload []byte) (uint16, string, bool) {
	if len(payload) < 3 || payload[0] != IPCTagTrackName {
		return 0, "", false
	}
	streamIndex := binary.LittleEndian.Uint16(payload[1:3])
	name, _, ok := readStr16(payload[3:])
	if !ok {
		return 0, "", false
	}
	return streamIndex, name, true
}

// EncodeMetrics encodes a plugin-side cumulative drop counter (dropped PCM blocks).
func EncodeMetrics(dropped uint64) []byte {
	msg := make([]byte, 9)
	msg[0] = IPCTagMetrics
	binary.LittleEndian.PutUint64(msg[1:], dropped)
	return msg
}

// DecodeMetrics decodes a plugin metrics report. Returns (dropped, ok).
func DecodeMetrics(payload []byte) (uint64, bool) {
	if len(payload) < 9 || payload[0] != IPCTagMetrics {
		return 0, false
	}
	return binary.LittleEndian.Uint64(payload[1:9]), true
}

// appendStr8 appends a string with a 1-byte length prefix (clamped to 255).
func appendStr8(b []byte, s string) []byte {
	n := min(len(s), 255)
	b = append(b, byte(n))
	return append(b, s[:n]...)
}

// readStr8 reads a 1-byte-length-prefixed string, returning it and the remainder.
func readStr8(p []byte) (string, []byte, bool) {
	if len(p) < 1 {
		return "", nil, false
	}
	n := int(p[0])
	if len(p) < 1+n {
		return "", nil, false
	}
	return string(p[1 : 1+n]), p[1+n:], true
}

// appendStr16 appends a string with a 2-byte LE length prefix (clamped to 65535).
func appendStr16(b []byte, s string) []byte {
	n := min(len(s), 65535)
	b = binary.LittleEndian.AppendUint16(b, uint16(n))
	return append(b, s[:n]...)
}

// readStr16 reads a 2-byte-LE-length-prefixed string and the remainder.
func readStr16(p []byte) (string, []byte, bool) {
	if len(p) < 2 {
		return "", nil, false
	}
	n := int(binary.LittleEndian.Uint16(p[0:2]))
	if len(p) < 2+n {
		return "", nil, false
	}
	return string(p[2 : 2+n]), p[2+n:], true
}

// ipcWriterQueue bounds each recv connection's pending-frame queue; Broadcast drops
// (never blocks) when a connection's writer falls behind, so a slow plugin socket
// can't stall the caller (the engine's emit loop / session goroutine).
const ipcWriterQueue = 256

// ipcWriteDeadline caps one blocking write on a per-connection writer goroutine.
const ipcWriteDeadline = 200 * time.Millisecond

// ipcWriter serializes writes to one recv connection on its own goroutine, fed by a
// bounded channel so producers never block on socket backpressure. version is the
// negotiated wire appetite (1 = RemotePCM, 2 = RemotePCM2 with beat stamps).
type ipcWriter struct {
	conn    net.Conn
	ch      chan []byte
	version int
	dropped atomic.Uint64
}

// IPCWriterPool fans frames out to connected recv plugins. Broadcast is non-blocking:
// each connection has a dedicated writer goroutine + bounded queue, decoupling the
// realtime/engine path from any one plugin socket's backpressure.
type IPCWriterPool struct {
	mu      sync.Mutex
	writers map[int]*ipcWriter
}

// NewIPCWriterPool creates a new writer pool.
func NewIPCWriterPool() *IPCWriterPool {
	return &IPCWriterPool{writers: make(map[int]*ipcWriter)}
}

// Add registers a recv connection and starts its writer goroutine.
func (p *IPCWriterPool) Add(connID int, conn net.Conn, version int) {
	w := &ipcWriter{conn: conn, ch: make(chan []byte, ipcWriterQueue), version: version}
	p.mu.Lock()
	p.writers[connID] = w
	p.mu.Unlock()
	go serveIPCWriter(w)
}

// serveIPCWriter drains one connection's queue with blocking writes. On error it
// closes the conn (unblocking the reader, whose defer calls Remove) and exits; the
// range ends when Remove closes the channel.
func serveIPCWriter(w *ipcWriter) {
	for frame := range w.ch {
		w.conn.SetWriteDeadline(time.Now().Add(ipcWriteDeadline))
		if _, err := w.conn.Write(frame); err != nil {
			w.conn.Close()
			return
		}
	}
}

// Remove drops a connection and stops its writer goroutine (idempotent).
func (p *IPCWriterPool) Remove(connID int) {
	p.mu.Lock()
	if w, ok := p.writers[connID]; ok {
		delete(p.writers, connID)
		close(w.ch)
	}
	p.mu.Unlock()
}

// IsEmpty returns true if no recv plugins are connected.
func (p *IPCWriterPool) IsEmpty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writers) == 0
}

// Len returns the number of active connections.
func (p *IPCWriterPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writers)
}

// Broadcast enqueues a frame for every connection without blocking. If a
// connection's queue is full (its writer is behind), the frame is dropped for that
// connection and counted. The frame is read-only and may be shared across writers.
func (p *IPCWriterPool) Broadcast(frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.writers {
		select {
		case w.ch <- frame:
		default:
			w.dropped.Add(1)
		}
	}
}

// BroadcastToVersion enqueues a frame only for connections at the given wire
// version (RemotePCM2 frames must never reach a v1 plugin). Same drop semantics
// as Broadcast.
func (p *IPCWriterPool) BroadcastToVersion(version int, frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.writers {
		if w.version != version {
			continue
		}
		select {
		case w.ch <- frame:
		default:
			w.dropped.Add(1)
		}
	}
}

// HasVersion reports whether any connected recv plugin speaks the given wire version.
func (p *IPCWriterPool) HasVersion(version int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.writers {
		if w.version == version {
			return true
		}
	}
	return false
}

// SendTo enqueues a frame to one connection's writer without blocking (drops on a
// full queue). Used for on-connect replay so it shares that connection's single
// serialized writer with Broadcast — never a second goroutine writing the same conn.
func (p *IPCWriterPool) SendTo(connID int, frame []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w, ok := p.writers[connID]; ok {
		select {
		case w.ch <- frame:
		default:
			w.dropped.Add(1)
		}
	}
}

// WriteFrame encodes a payload as a framed message and writes it to a connection.
func WriteFrame(w io.Writer, payload []byte) error {
	_, err := w.Write(EncodeFrame(payload))
	return err
}
