package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// SchemaVersion is the highest migration this binary carries. SQLite's own
// user_version holds what the file is at, so the two are compared at open and
// there is no second copy of the number to drift.
const SchemaVersion = 2

type migration struct {
	version int
	name    string
	body    string
}

// migrate brings the archive up to SchemaVersion, and refuses to run against
// a file written by a newer binary rather than querying columns that moved.
func (d *DB) migrate(ctx context.Context) error {
	current, err := d.userVersion(ctx)
	if err != nil {
		return err
	}
	if current > SchemaVersion {
		return fmt.Errorf(
			"store: %s is at schema version %d but this build only knows %d; upgrade fasync",
			d.path, current, SchemaVersion)
	}

	pending, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range pending {
		if m.version <= current {
			continue
		}
		if err := d.applyMigration(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) applyMigration(ctx context.Context, m migration) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: starting migration %s: %w", m.name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("store: applying migration %s: %w", m.name, err)
	}
	// user_version takes no bind parameter. The value came from a filename
	// already parsed as an integer, so the interpolation cannot inject.
	if _, err := tx.ExecContext(ctx,
		"PRAGMA user_version = "+strconv.Itoa(m.version)); err != nil {
		return fmt.Errorf("store: recording migration %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: committing migration %s: %w", m.name, err)
	}
	return nil
}

func (d *DB) userVersion(ctx context.Context) (int, error) {
	var v int
	if err := d.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: reading the schema version: %w", err)
	}
	return v, nil
}

// loadMigrations reads the embedded files in version order and fails on
// anything misnamed, so a typo cannot be skipped silently at startup.
func loadMigrations() ([]migration, error) {
	entries, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("store: listing migrations: %w", err)
	}

	out := make([]migration, 0, len(entries))
	seen := make(map[int]string, len(entries))
	for _, entry := range entries {
		name := path.Base(entry)
		version, err := versionOf(name)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf(
				"store: migrations %s and %s share version %d", prev, name, version)
		}
		seen[version] = name

		body, err := migrationFS.ReadFile(entry)
		if err != nil {
			return nil, fmt.Errorf("store: reading migration %s: %w", name, err)
		}
		out = append(out, migration{version: version, name: name, body: string(body)})
	}

	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	if len(out) > 0 && out[len(out)-1].version != SchemaVersion {
		return nil, fmt.Errorf(
			"store: highest migration is %d but SchemaVersion is %d",
			out[len(out)-1].version, SchemaVersion)
	}
	return out, nil
}

// versionOf reads the leading number from a name like 0001_init.sql.
func versionOf(name string) (int, error) {
	prefix, _, found := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
	if !found {
		return 0, fmt.Errorf("store: migration %q must be named <version>_<description>.sql", name)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("store: migration %q has no positive version prefix", name)
	}
	return version, nil
}
