// Package lock serialises runs against one data directory.
//
// SQLite's WAL handles concurrent database access on its own. What needs a
// lock is everything around it: advancing a cursor, sweeping, and the partial
// downloads in tmp/. Two cron ticks overlapping there would corrupt state
// that no transaction covers.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// ErrHeld reports that another run holds the lock. It is a distinct error
// because a cron job needs to tell "already running" from "broken", and the
// two get different exit codes.
var ErrHeld = errors.New("lock: another run holds the data directory")

// dirPerm and filePerm match the rest of the data directory.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// Lock is a held advisory lock on a data directory.
type Lock struct {
	file *flock.Flock
	path string
}

// Acquire takes the lock without waiting. It fails immediately rather than
// blocking, because a cron tick that queues behind the previous one just
// stacks up work until something falls over. Better to exit and try at the
// next tick.
func Acquire(path string) (*Lock, error) {
	if path == "" {
		return nil, errors.New("lock: Acquire requires a path")
	}
	// The path is the operator's own data directory, chosen on their own
	// command line. There is no untrusted input to traverse with.
	//nolint:gosec // G703: the path is operator-supplied by design
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("lock: creating the data directory: %w", err)
	}

	f := flock.New(path)
	held, err := f.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock: taking %s: %w", path, err)
	}
	if !held {
		return nil, fmt.Errorf("%w: %s", ErrHeld, path)
	}

	// Best effort: the lock works regardless of mode, but the data directory
	// is 0700 and a stray world-readable file in it is untidy.
	//nolint:gosec // G703: same operator-supplied path as above
	_ = os.Chmod(path, filePerm)
	return &Lock{file: f, path: path}, nil
}

// Release drops the lock. Safe to call twice.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Unlock()
	l.file = nil
	if err != nil {
		return fmt.Errorf("lock: releasing %s: %w", l.path, err)
	}
	return nil
}

// Path reports the file backing the lock.
func (l *Lock) Path() string { return l.path }
