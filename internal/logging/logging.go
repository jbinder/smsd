// Package logging provides a small file logger with size-based rotation that
// writes to ~/.local/state/smsd/log.txt. It intentionally avoids third-party
// logging dependencies to keep the binary and memory footprint small.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// Logger is a leveled logger that writes to a rotating file (and optionally a
// mirror writer such as stderr). It is safe for concurrent use.
type Logger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	std      *log.Logger
	mirror   io.Writer
}

// New opens (or creates) the log file at path, rotating it once it grows past
// maxBytes. If mirror is non-nil, records are also written there.
func New(path string, maxBytes int64, mirror io.Writer) (*Logger, error) {
	l := &Logger{path: path, maxBytes: maxBytes, mirror: mirror}
	if err := l.open(); err != nil {
		return nil, err
	}
	l.std = log.New(l, "", log.LstdFlags|log.LUTC)
	return l, nil
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening log %s: %w", l.path, err)
	}
	l.f = f
	return nil
}

// Write implements io.Writer for the underlying log.Logger, rotating first.
func (l *Logger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotateIfNeeded()
	if l.mirror != nil {
		l.mirror.Write(p)
	}
	return l.f.Write(p)
}

func (l *Logger) rotateIfNeeded() {
	if l.maxBytes <= 0 {
		return
	}
	info, err := l.f.Stat()
	if err != nil || info.Size() < l.maxBytes {
		return
	}
	l.f.Close()
	// Keep a single previous copy: log.txt -> log.txt.1
	_ = os.Rename(l.path, l.path+".1")
	if err := l.open(); err != nil {
		// Fall back to stderr so we never lose logging entirely.
		l.f = os.Stderr
	}
}

// Infof logs an informational message.
func (l *Logger) Infof(format string, args ...any) {
	l.std.Output(2, "INFO  "+fmt.Sprintf(format, args...))
}

// Warnf logs a warning.
func (l *Logger) Warnf(format string, args ...any) {
	l.std.Output(2, "WARN  "+fmt.Sprintf(format, args...))
}

// Errorf logs an error.
func (l *Logger) Errorf(format string, args ...any) {
	l.std.Output(2, "ERROR "+fmt.Sprintf(format, args...))
}

// Close flushes and closes the log file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.f != os.Stderr {
		return l.f.Close()
	}
	return nil
}
