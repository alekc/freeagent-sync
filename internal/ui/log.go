package ui

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Logger is the slice of slog this package needs. Narrow so tests can capture
// without a handler, and so callers are not forced onto a concrete logger.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// SlogLogger adapts *slog.Logger to Logger.
func SlogLogger(l *slog.Logger) Logger { return slogAdapter{l} }

type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Info(msg string, args ...any)  { a.l.Info(msg, args...) }
func (a slogAdapter) Warn(msg string, args ...any)  { a.l.Warn(msg, args...) }
func (a slogAdapter) Error(msg string, args ...any) { a.l.Error(msg, args...) }

// logReporter is the non-interactive implementation. It stays quiet during a
// unit of work and emits one line when it finishes, because a cron mailbox
// wants the outcome rather than the animation.
type logReporter struct {
	log Logger
}

func newLogReporter(log Logger) *logReporter { return &logReporter{log: log} }

func (r *logReporter) Track(name string, total int64, units Units) Tracker {
	return &logTracker{
		log:     r.log,
		name:    name,
		total:   total,
		units:   units,
		started: time.Now(),
	}
}

func (r *logReporter) Logf(format string, args ...any) {
	r.log.Info(fmt.Sprintf(format, args...))
}

func (r *logReporter) Close() {}

type logTracker struct {
	log     Logger
	name    string
	units   Units
	started time.Time

	mu       sync.Mutex
	value    int64
	total    int64
	message  string
	finished bool
}

func (t *logTracker) Add(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.value += n
}

func (t *logTracker) SetTotal(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total = n
}

func (t *logTracker) Message(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.message = s
}

func (t *logTracker) Done() {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	args := t.summary()
	t.mu.Unlock()

	t.log.Info("finished", args...)
}

func (t *logTracker) Fail(err error) {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	args := append(t.summary(), "error", err)
	t.mu.Unlock()

	t.log.Error("failed", args...)
}

// summary assumes the caller holds the lock.
func (t *logTracker) summary() []any {
	args := []any{
		"task", t.name,
		"took", time.Since(t.started).Round(time.Millisecond),
	}
	if t.units == UnitsBytes {
		args = append(args, "bytes", t.value)
	} else {
		args = append(args, "count", t.value)
	}
	if t.total > 0 {
		args = append(args, "expected", t.total)
	}
	if t.message != "" {
		args = append(args, "detail", t.message)
	}
	return args
}
