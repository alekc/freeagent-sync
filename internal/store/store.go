// Package store owns the SQLite archive: schema, migrations, and the
// conventions every other package relies on for timestamps and money.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

// TimeLayout is how every timestamp is stored: RFC 3339 in UTC at fixed
// width, so the text sorts chronologically and any tool can read it.
// RFC3339Nano is unusable here because it trims trailing zeros.
const TimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// DateLayout is how business dates are stored, distinct from an instant.
const DateLayout = time.DateOnly

// dirPerm and filePerm keep the archive readable only by its owner. It holds
// bank transactions and invoices.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// DB is an open archive.
type DB struct {
	*sql.DB
	path string
}

// Path returns the file backing the archive.
func (d *DB) Path() string { return d.path }

// Open prepares the archive at path, creating and migrating it if needed.
// Parent directories are created 0700.
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("store: Open requires a path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolving %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), dirPerm); err != nil {
		return nil, fmt.Errorf("store: creating the data directory: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("store: opening %q: %w", abs, err)
	}

	// One connection. SQLite takes a single writer anyway, and this tool's
	// concurrency is in HTTP rather than in SQL, so serialising here removes
	// a whole class of SQLITE_BUSY for no measurable cost.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	db := &DB{DB: sqlDB, path: abs}
	if err := db.prepare(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// dsn builds the connection string. The file: prefix is load-bearing: without
// it the driver strips the query before opening, so every pragma below would
// silently do nothing and the archive would run without foreign keys.
func dsn(abs string) string {
	q := url.Values{}
	q.Set("_journal", "WAL")
	q.Set("_synchronous", "NORMAL")
	q.Set("_foreign_keys", "1")
	q.Set("_timeout", "5000")
	// Deferred transactions upgrade to a write lock mid-flight and cannot
	// retry cleanly when that fails. Taking the lock up front can wait
	// instead, which is what _timeout is for.
	q.Set("_txlock", "immediate")
	return "file:" + abs + "?" + q.Encode()
}

func (d *DB) prepare(ctx context.Context) error {
	if err := d.PingContext(ctx); err != nil {
		return fmt.Errorf("store: connecting to %q: %w", d.path, err)
	}
	if err := os.Chmod(d.path, filePerm); err != nil {
		return fmt.Errorf("store: tightening permissions on the archive: %w", err)
	}
	if err := d.verifyPragmas(ctx); err != nil {
		return err
	}
	return d.migrate(ctx)
}

// verifyPragmas reads back what the DSN asked for. A pragma that failed to
// apply is invisible until something corrupts, so it is checked at open
// rather than trusted.
func (d *DB) verifyPragmas(ctx context.Context) error {
	checks := []struct {
		pragma string
		want   string
	}{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
	}
	for _, c := range checks {
		var got string
		if err := d.QueryRowContext(ctx, "PRAGMA "+c.pragma).Scan(&got); err != nil {
			return fmt.Errorf("store: reading PRAGMA %s: %w", c.pragma, err)
		}
		if got != c.want {
			return fmt.Errorf(
				"store: PRAGMA %s is %q, want %q; the DSN did not take effect",
				c.pragma, got, c.want)
		}
	}
	return nil
}

// FormatTime renders an instant in the archive's storage format. A zero time
// yields the empty string, which callers store as NULL.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(TimeLayout)
}

// ParseTime reads a stored instant. The empty string yields the zero time.
func ParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(TimeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: %q is not a stored timestamp: %w", s, err)
	}
	return t, nil
}

// NullTime renders an instant for a nullable column.
func NullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return FormatTime(t)
}

// FormatDate renders a business date in the archive's storage format.
func FormatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(DateLayout)
}
