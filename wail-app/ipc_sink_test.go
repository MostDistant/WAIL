//go:build !linkstub

package main

import (
	"net"
	"slices"
	"testing"
	"time"
)

// pipeFrameReader drains an IPC connection into a channel of decoded frames.
func pipeFrameReader(conn net.Conn) <-chan []byte {
	frames := make(chan []byte, 16)
	go func() {
		defer close(frames)
		rb := NewIPCRecvBuffer()
		buf := make([]byte, 4096)
		for {
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := conn.Read(buf)
			if n > 0 {
				rb.Push(buf[:n])
				for {
					f, ferr := rb.NextFrame()
					if ferr != nil || f == nil {
						break
					}
					frames <- f
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return frames
}

func TestIPCEmitSinkFrames(t *testing.T) {
	pool := NewIPCWriterPool()
	// net.Pipe is synchronous, so Broadcast's Write completes only once the reader
	// consumes it — the goroutine below keeps it moving.
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()
	pool.Add(1, serverEnd, 1)
	defer pool.Remove(1) // stop the writer goroutine at test end
	frames := pipeFrameReader(clientEnd)

	sink := newIPCEmitSink(pool, "peerZ", 5)

	// A FIFO sink must NOT receive the chunk at write time (the feeder runs
	// cushion-ahead of the playhead); it is released by Flush once its stamped
	// beat is due.
	samples := []int16{1, 2, 3, 4}
	sink.WriteInterleaved(samples, nil, 100.0, 0, 2, 2, 48000)
	select {
	case f := <-frames:
		t.Fatalf("chunk delivered before its stamped beat: %x", f)
	case <-time.After(150 * time.Millisecond):
	}

	// The mapper converts the chunk's stamped beat to the plugin-facing
	// monotonic play-at µs (the engine's clock bridge); the wire frame must
	// carry exactly that stamp.
	playAt := func(beat float64) int64 { return int64(beat * 1e6) }
	sink.Flush(100.0, 0, playAt)
	select {
	case f := <-frames:
		rp, ok := DecodeRemotePCM(f)
		if !ok || rp.PeerID != "peerZ" || rp.StreamID != 5 || !slices.Equal(rp.Samples, samples) {
			t.Fatalf("RemotePCM mismatch: ok=%v %+v", ok, rp)
		}
		if rp.PlayAtMicros != 100_000_000 {
			t.Fatalf("PlayAtMicros = %d, want 100000000 (mapper applied to beat 100)", rp.PlayAtMicros)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RemotePCM")
	}

	// A future-stamped chunk stays held through flushes that don't reach it.
	sink.WriteInterleaved([]int16{9}, nil, 200.0, 0, 1, 2, 48000)
	sink.Flush(150.0, 0, playAt)
	select {
	case f := <-frames:
		t.Fatalf("future chunk released early: %x", f)
	case <-time.After(150 * time.Millisecond):
	}
	sink.Flush(200.0, 0, playAt)
	select {
	case f := <-frames:
		if rp, ok := DecodeRemotePCM(f); !ok || !slices.Equal(rp.Samples, []int16{9}) {
			t.Fatalf("RemotePCM mismatch: ok=%v %+v", ok, rp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for held RemotePCM")
	}

	sink.SetName("Zoe · vox")
	select {
	case f := <-frames:
		pid, sid, name, ok := DecodeStreamName(f)
		if !ok || pid != "peerZ" || sid != 5 || name != "Zoe · vox" {
			t.Fatalf("StreamName mismatch: ok=%v pid=%q sid=%d name=%q", ok, pid, sid, name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for StreamName")
	}
}

func TestIPCEmitSinkFlushFanOutByVersion(t *testing.T) {
	pool := NewIPCWriterPool()
	srv1, cli1 := net.Pipe()
	defer srv1.Close()
	defer cli1.Close()
	srv2, cli2 := net.Pipe()
	defer srv2.Close()
	defer cli2.Close()
	pool.Add(1, srv1, 1) // legacy recv plugin
	defer pool.Remove(1)
	pool.Add(2, srv2, 2) // transport-aware recv plugin
	defer pool.Remove(2)
	frames1 := pipeFrameReader(cli1)
	frames2 := pipeFrameReader(cli2)

	sink := newIPCEmitSink(pool, "peerZ", 5)
	sink.WriteInterleaved([]int16{1, 2, 3, 4}, nil, 100.0, 0, 2, 2, 48000)
	playAt := func(beat float64) int64 { return int64(beat * 1e6) }
	sink.Flush(100.0, 0, playAt)

	select {
	case f := <-frames1:
		rp, ok := DecodeRemotePCM(f)
		if !ok || rp.PlayAtMicros != 100_000_000 || rp.Beat != 0 {
			t.Fatalf("v1 conn: bad v1 frame: ok=%v %+v", ok, rp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("v1 conn: no frame")
	}
	select {
	case f := <-frames2:
		rp, ok := DecodeRemotePCM2(f)
		if !ok || rp.PlayAtMicros != 100_000_000 || rp.Beat != 100.0 || !slices.Equal(rp.Samples, []int16{1, 2, 3, 4}) {
			t.Fatalf("v2 conn: bad v2 frame: ok=%v %+v", ok, rp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("v2 conn: no frame")
	}
}

func TestIPCEmitSinkEmptyPoolNoop(t *testing.T) {
	pool := NewIPCWriterPool()
	sink := newIPCEmitSink(pool, "p", 0)
	if !sink.WriteInterleaved([]int16{1, 2}, nil, 0, 0, 1, 2, 48000) {
		t.Fatal("WriteInterleaved should return true when no recv plugin is connected")
	}
	sink.SetName("x") // must not block or panic on an empty pool
}
