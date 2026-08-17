// Package ui reports progress. It has two implementations behind one
// interface, so no engine code branches on whether a terminal is attached:
// progress bars interactively, structured log lines under cron.
package ui

import (
	"fmt"
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// Mode selects between the two implementations.
type Mode string

const (
	// ModeAuto picks bars when the output is a terminal.
	ModeAuto Mode = "auto"
	// ModeAlways forces bars, for a terminal this build cannot detect.
	ModeAlways Mode = "always"
	// ModeNever forces log lines, which is what cron and CI want.
	ModeNever Mode = "never"
)

// ParseMode validates a --progress value, listing the alternatives rather
// than failing with the bare word the user typed.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeAuto, ModeAlways, ModeNever:
		return Mode(s), nil
	}
	return "", fmt.Errorf("ui: unknown progress mode %q, want auto, always or never", s)
}

// Units is how a tracker's numbers are rendered.
type Units int

const (
	// UnitsCount is a plain tally, for records and requests.
	UnitsCount Units = iota
	// UnitsBytes renders as KB, MB and so on, for downloads.
	UnitsBytes
)

// Reporter is the whole surface the engine uses. Implementations are safe for
// concurrent use: workers call Track and Logf from their own goroutines.
type Reporter interface {
	// Track registers a unit of work. A total of zero means unknown, which
	// stays indeterminate until SetTotal is called.
	Track(name string, total int64, units Units) Tracker
	// Logf emits a note that must not be swallowed by the display.
	Logf(format string, args ...any)
	// Close flushes and stops. Idempotent.
	Close()
}

// Tracker is one unit of work. Every method is safe to call concurrently, and
// safe to call after Done.
type Tracker interface {
	Add(n int64)
	SetTotal(n int64)
	Message(s string)
	Done()
	Fail(err error)
}

// New builds the reporter for a mode. out is where bars are drawn; log is
// where structured lines go. Both are consulted so an interactive run can
// still surface a warning through the logger.
func New(mode Mode, out io.Writer, log Logger) (Reporter, error) {
	switch mode {
	case ModeAlways:
		return newBarReporter(out, log), nil
	case ModeNever:
		return newLogReporter(log), nil
	case ModeAuto:
		if isTerminal(out) {
			return newBarReporter(out, log), nil
		}
		return newLogReporter(log), nil
	}
	return nil, fmt.Errorf("ui: unknown progress mode %q", mode)
}

// isTerminal reports whether bars would be readable. Anything that is not an
// os.File cannot be one, which also covers the buffers used in tests.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}
