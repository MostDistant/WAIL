//go:build !linkstub

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"
)

// maybeStartIPCServer starts the loopback-TCP listener that CLAP Send/Recv plugins
// connect to (ADR-0005). It returns a stop function; the listener also closes when
// ctx ends. Under -tags linkstub the no-op variant (ipc_server_stub.go) is used.
func maybeStartIPCServer(ctx context.Context, port uint16, engine AudioEngine) func() {
	eng, ok := engine.(*linkAudioEngine)
	if !ok {
		return func() {}
	}
	s := &ipcServer{engine: eng, pool: NewIPCWriterPool()}
	eng.SetRecvPool(s.pool)
	if err := s.start(ctx, port); err != nil {
		// A busy port (e.g. two app instances sharing one) is non-fatal: the app
		// still works over Link Audio, just without the plugin bridge.
		log.Printf("[ipc] listener disabled: %v", err)
		return func() {}
	}
	return s.stop
}

type ipcServer struct {
	engine *linkAudioEngine
	pool   *IPCWriterPool
	ln     net.Listener
	nextID atomic.Int64
}

func (s *ipcServer) start(ctx context.Context, port uint16) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	s.ln = ln
	log.Printf("[ipc] listening on 127.0.0.1:%d for CLAP plugins", port)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go s.acceptLoop(ctx)
	return nil
}

func (s *ipcServer) stop() {
	if s.ln != nil {
		s.ln.Close()
	}
}

func (s *ipcServer) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed (ctx ended) or fatal accept error
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *ipcServer) handleConn(ctx context.Context, conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	// Unblock the blocking reads below when the session ends. The watcher exits when
	// this handler returns (done), so it doesn't leak across connect/disconnect churn.
	done := make(chan struct{})
	defer close(done)
	defer conn.Close()
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	role := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, role); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	switch role[0] {
	case IPCRoleSend:
		s.handleSend(ctx, conn)
	case IPCRoleRecv:
		s.handleRecv(ctx, conn)
	default:
		log.Printf("[ipc] unknown role byte 0x%02x", role[0])
	}
}

func (s *ipcServer) handleSend(ctx context.Context, conn net.Conn) {
	// The Send plugin writes its stream index (u16 LE) right after the role byte.
	// Require it: defaulting-without-consuming on timeout would let those two bytes
	// be misread as frame data, so treat a failure to read them as fatal.
	idx := make([]byte, 2)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, idx); err != nil {
		log.Printf("[ipc] send handshake: no stream index: %v", err)
		return
	}
	conn.SetReadDeadline(time.Time{})
	streamIndex := binary.LittleEndian.Uint16(idx)

	src, key, epoch, ok := s.engine.AddPluginSource(streamIndex, fmt.Sprintf("Plugin Send %d", streamIndex))
	if !ok {
		log.Printf("[ipc] failed to register plugin send stream %d", streamIndex)
		return
	}
	defer s.engine.RemovePluginSource(key, epoch)

	rb := NewIPCRecvBuffer()
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			rb.Push(buf[:n])
			for {
				payload, ferr := rb.NextFrame()
				if ferr != nil {
					log.Printf("[ipc] send stream %d framing error: %v", streamIndex, ferr)
					return
				}
				if payload == nil {
					break
				}
				if raw, ok := DecodeRawPCM(payload); ok {
					src.Push(rawPCMToInt16(raw.Flags, raw.Samples), raw.FrameCounter, int(raw.Channels), raw.SampleRate)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *ipcServer) handleRecv(ctx context.Context, conn net.Conn) {
	id := int(s.nextID.Add(1))
	// Add before replaying names: a stream created in this window still reaches the
	// new connection via Broadcast (a duplicate name is harmless); replaying first
	// could miss it.
	s.pool.Add(id, conn)
	defer s.pool.Remove(id)
	// Enqueue the name replay through the pool so it shares this conn's single writer
	// goroutine with Broadcast (no concurrent writes to the same socket).
	for _, f := range s.engine.StreamNameFrames() {
		s.pool.SendTo(id, f)
	}
	log.Printf("[ipc] recv plugin connected (id %d)", id)

	rb := NewIPCRecvBuffer()
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			rb.Push(buf[:n])
			for {
				payload, ferr := rb.NextFrame()
				if ferr != nil {
					return
				}
				if payload == nil {
					break
				}
				// Only Metrics is expected back from a recv plugin (drop counter).
				_, _ = DecodeMetrics(payload)
			}
		}
		if err != nil {
			return
		}
	}
}
