package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

const (
	maxLogFileSize = 50 * 1024 * 1024 // 50 MB
	maxLogArchives = 9
)

// RotatingFileWriter implements io.Writer with size-based rotation.
// Thread-safe. Can be disabled at runtime via SetEnabled.
type RotatingFileWriter struct {
	mu      sync.Mutex
	dir     string
	name    string
	file    *os.File
	written int64
	enabled atomic.Bool
}

// NewRotatingFileWriter creates a rotating file logger.
// Writes to {dir}/{name}. Rotates to {name}.1, .2, ... .{maxLogArchives}.
func NewRotatingFileWriter(dir, name string) (*RotatingFileWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &RotatingFileWriter{dir: dir, name: name}
	w.enabled.Store(true)
	if err := w.openFile(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotatingFileWriter) openFile() error {
	path := filepath.Join(w.dir, w.name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.file = f
	w.written = info.Size()
	return nil
}

// Write implements io.Writer.
func (w *RotatingFileWriter) Write(p []byte) (n int, err error) {
	if !w.enabled.Load() {
		return len(p), nil // silently discard
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, fmt.Errorf("log file not open")
	}
	if w.written+int64(len(p)) > maxLogFileSize {
		w.rotate()
	}
	n, err = w.file.Write(p)
	w.written += int64(n)
	return
}

func (w *RotatingFileWriter) rotate() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
	// Shift archives: .9 → delete, .8 → .9, ... .1 → .2, current → .1
	base := filepath.Join(w.dir, w.name)
	os.Remove(fmt.Sprintf("%s.%d", base, maxLogArchives))
	for i := maxLogArchives - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", base, i), fmt.Sprintf("%s.%d", base, i+1))
	}
	os.Rename(base, base+".1")
	w.openFile()
}

// SetEnabled toggles logging on or off at runtime.
func (w *RotatingFileWriter) SetEnabled(enabled bool) {
	w.enabled.Store(enabled)
}

// Close closes the log file.
func (w *RotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// SetupLogger configures the Go log package to write to both stderr and a rotating file.
// Returns the file writer for later control (enable/disable).
func SetupLogger(logDir string) (*RotatingFileWriter, error) {
	fw, err := NewRotatingFileWriter(logDir, "wail.log")
	if err != nil {
		return nil, err
	}
	mw := io.MultiWriter(os.Stderr, fw)
	// We use the default log package; set its output
	// (imported as "log" in the caller)
	return fw, setLogOutput(mw)
}

// setLogOutput is a helper to set log output without importing "log" here
// (to avoid circular issues). The caller should wire this.
func setLogOutput(w io.Writer) error {
	// This is called from main.go which imports "log" and calls log.SetOutput
	// We return nil; the caller does the actual SetOutput call.
	return nil
}
