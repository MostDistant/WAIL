//go:build !linkstub

package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestIPCServerRecvReplay verifies the server dispatches a recv-role connection and
// replays the current stream names to it, so a Recv plugin can label its ports for
// streams whose audio is already flowing.
func TestIPCServerRecvReplay(t *testing.T) {
	lb := NewLinkBridge(120, 4)
	eng := newAudioEngine(lb, "TestPeer", func([]byte) {}, 1).(*linkAudioEngine)
	pool := NewIPCWriterPool()
	eng.SetRecvPool(pool)

	// Publish a stream so there's a name to replay (no engine goroutines needed).
	enc, err := NewIntervalEncoder(2, 48000, 128)
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	frames, _, err := enc.EncodeInterval(make([]int16, 960*2*2), 5, 0, 0, 120, 4, 4)
	if err != nil || len(frames) == 0 {
		t.Fatalf("encode: %v (%d frames)", err, len(frames))
	}
	eng.HandleRemoteAudio("idA", "Alice", "guitar", frames[0])

	srv := &ipcServer{engine: eng, pool: pool}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.start(ctx, 0); err != nil {
		t.Fatalf("server start: %v", err)
	}

	conn, err := net.Dial("tcp", srv.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{IPCRoleRecv}); err != nil {
		t.Fatalf("write role: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	rb := NewIPCRecvBuffer()
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			rb.Push(buf[:n])
			if f, _ := rb.NextFrame(); f != nil {
				pid, _, name, ok := DecodeStreamName(f)
				if !ok {
					t.Fatalf("expected a StreamName replay, got tag %d", IPCTag(f))
				}
				if pid != "idA" || !strings.Contains(name, "Alice") {
					t.Fatalf("unexpected replay: pid=%q name=%q", pid, name)
				}
				return // success
			}
		}
		if err != nil {
			t.Fatalf("read (no StreamName replay): %v", err)
		}
	}
}
