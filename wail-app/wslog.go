package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WsLogEntry is a structured log entry for WebSocket broadcast.
type WsLogEntry struct {
	Level       string
	Target      string
	Message     string
	TimestampUs uint64
}

// WsLogWriter captures log output and broadcasts to subscribers.
// Implements io.Writer so it can be added to a MultiWriter chain.
type WsLogWriter struct {
	enabled    atomic.Bool
	mu         sync.Mutex
	subscribers []chan WsLogEntry
	epoch      time.Time
}

// NewWsLogWriter creates a new WebSocket log writer.
func NewWsLogWriter() *WsLogWriter {
	return &WsLogWriter{epoch: time.Now()}
}

// Write implements io.Writer. Parses log lines and broadcasts to subscribers.
func (w *WsLogWriter) Write(p []byte) (n int, err error) {
	if !w.enabled.Load() {
		return len(p), nil
	}
	line := strings.TrimSpace(string(p))
	if line == "" {
		return len(p), nil
	}

	entry := WsLogEntry{
		Level:       "info",
		Target:      "wail",
		Message:     line,
		TimestampUs: uint64(time.Since(w.epoch).Microseconds()),
	}

	// Simple level detection from log format
	if strings.Contains(line, "WARN") || strings.Contains(line, "warn") {
		entry.Level = "warn"
	} else if strings.Contains(line, "ERROR") || strings.Contains(line, "error") {
		entry.Level = "error"
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.subscribers {
		select {
		case ch <- entry:
		default: // don't block on slow subscribers
		}
	}
	return len(p), nil
}

// Subscribe returns a channel that receives log entries.
func (w *WsLogWriter) Subscribe() <-chan WsLogEntry {
	ch := make(chan WsLogEntry, 256)
	w.mu.Lock()
	w.subscribers = append(w.subscribers, ch)
	w.mu.Unlock()
	return ch
}

// SetEnabled toggles log broadcasting.
func (w *WsLogWriter) SetEnabled(enabled bool) {
	w.enabled.Store(enabled)
}

// IsEnabled returns whether broadcasting is active.
func (w *WsLogWriter) IsEnabled() bool {
	return w.enabled.Load()
}

// SetupLogOutputs configures the Go log package to write to stderr + file + wslog.
// Returns the file writer and ws log writer for runtime control.
func SetupLogOutputs(logDir string) (*RotatingFileWriter, *WsLogWriter, error) {
	fw, err := NewRotatingFileWriter(logDir, "wail.log")
	if err != nil {
		return nil, nil, fmt.Errorf("rotating log: %w", err)
	}
	wsw := NewWsLogWriter()
	return fw, wsw, nil
}

// CombinedWriter returns a MultiWriter combining stderr, file log, and wslog.
func CombinedWriter(fw *RotatingFileWriter, wsw *WsLogWriter) io.Writer {
	return io.MultiWriter(fw, wsw)
}
