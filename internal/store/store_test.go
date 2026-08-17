package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "freeagent.sqlite")
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenCreatesAndMigrates(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	var version int
	if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}

	// Every table the design promises should exist, so a missing CREATE in
	// the migration fails here rather than at the first insert.
	want := []string{
		"accounts", "account_locks", "attachments", "blobs", "capabilities",
		"documents", "identity_map", "meta", "records", "record_versions",
		"report_snapshots", "sync_runs", "sync_state",
	}
	for _, table := range want {
		var name string
		err := db.QueryRowContext(t.Context(),
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?",
			table).Scan(&name)
		if err != nil {
			t.Errorf("table %s is missing: %v", table, err)
		}
	}
}

// The DSN pragmas are silently dropped if the connection string loses its
// file: prefix, so this asserts they actually took effect.
func TestOpenAppliesPragmas(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	var journal string
	if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}

	var fk int
	if err := db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

func TestOpenTightensPermissions(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	info, err := os.Stat(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != filePerm {
		t.Errorf("archive mode = %o, want %o", perm, filePerm)
	}

	dir, err := os.Stat(filepath.Dir(db.Path()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm != dirPerm {
		t.Errorf("data directory mode = %o, want %o", perm, dirPerm)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "freeagent.sqlite")

	first, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ExecContext(t.Context(),
		`INSERT INTO accounts (slug, environment, created_at) VALUES ('acme', 'sandbox', ?)`,
		FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("reopening a migrated archive: %v", err)
	}
	defer func() { _ = second.Close() }()

	var count int
	if err := second.QueryRowContext(t.Context(),
		"SELECT count(*) FROM accounts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("accounts after reopen = %d, want 1", count)
	}
}

// An older binary against a newer archive must stop, not query columns that
// have moved underneath it.
func TestOpenRefusesANewerSchema(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "freeagent.sqlite")

	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(t.Context(), path)
	if err == nil {
		t.Fatal("opening a newer archive succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "upgrade fasync") {
		t.Errorf("error = %q, want it to tell the user to upgrade", err)
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := Open(t.Context(), ""); err == nil {
		t.Fatal("Open(\"\") succeeded, want an error")
	}
}

// Foreign keys are per-connection, so this is really checking that the pragma
// reached the pooled connection and not just the one that ran the migration.
func TestForeignKeysAreEnforced(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	_, err := db.ExecContext(t.Context(),
		`INSERT INTO records
		 (account_id, family, url, body, body_sha256, first_seen_at, last_seen_at)
		 VALUES (404, 'bills', 'https://example.test/bills/1', '{}', 'abc', ?, ?)`,
		FormatTime(time.Now()), FormatTime(time.Now()))
	if err == nil {
		t.Fatal("a record against a missing account was accepted")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("error = %q, want a foreign key violation", err)
	}
}

// STRICT tables are the schema-level half of failing early: a wrong type is
// rejected at write instead of surfacing as a nonsense read months later.
func TestStrictTablesRejectAWrongType(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	_, err := db.ExecContext(t.Context(),
		`INSERT INTO accounts (slug, environment, created_at, writable)
		 VALUES ('acme', 'sandbox', ?, 'not-a-number')`,
		FormatTime(time.Now()))
	if err == nil {
		t.Fatal("a text value in an INTEGER column was accepted")
	}
}

func TestSoftDeleteKeepsTheRow(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	now := FormatTime(time.Now())
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO records
		 (account_id, family, url, body, body_sha256, first_seen_at, last_seen_at)
		 VALUES (1, 'bills', 'https://example.test/bills/1', '{"x":1}', 'abc', ?, ?)`,
		now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(),
		`UPDATE records SET deleted_at = ? WHERE url = 'https://example.test/bills/1'`,
		now); err != nil {
		t.Fatal(err)
	}

	var body string
	var deleted sql.NullString
	err := db.QueryRowContext(t.Context(),
		`SELECT body, deleted_at FROM records WHERE url = 'https://example.test/bills/1'`).
		Scan(&body, &deleted)
	if err != nil {
		t.Fatalf("a soft-deleted record should still be readable: %v", err)
	}
	if body != `{"x":1}` {
		t.Errorf("body = %q, want it preserved through the delete", body)
	}
	if !deleted.Valid {
		t.Error("deleted_at was not recorded")
	}
}

func seedAccount(t *testing.T, db *DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO accounts (id, slug, environment, created_at)
		 VALUES (1, 'acme', 'sandbox', ?)`, FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 3, 14, 9, 26, 53, 589793238, time.FixedZone("BST", 3600))

	got, err := ParseTime(FormatTime(when))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(when) {
		t.Errorf("round trip = %s, want %s", got, when)
	}
	if got.Location() != time.UTC {
		t.Errorf("stored time came back in %s, want UTC", got.Location())
	}
}

func TestTimeZeroIsEmpty(t *testing.T) {
	t.Parallel()
	if s := FormatTime(time.Time{}); s != "" {
		t.Errorf("FormatTime(zero) = %q, want empty", s)
	}
	got, err := ParseTime("")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("ParseTime(\"\") = %s, want the zero time", got)
	}
	if NullTime(time.Time{}) != nil {
		t.Error("NullTime(zero) should be nil so the column stores NULL")
	}
}

// Fixed width matters: RFC3339Nano trims trailing zeros, which would make
// stored timestamps sort wrongly as text.
func TestStoredTimesSortChronologically(t *testing.T) {
	t.Parallel()
	earlier := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	later := time.Date(2026, 3, 14, 9, 0, 0, 500, time.UTC)

	a, b := FormatTime(earlier), FormatTime(later)
	if len(a) != len(b) {
		t.Fatalf("widths differ: %q and %q", a, b)
	}
	if a >= b {
		t.Errorf("%q should sort before %q", a, b)
	}
}

func TestParseTimeRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, err := ParseTime("14/03/2026"); err == nil {
		t.Fatal("a non-stored format was accepted")
	}
}

func TestFormatDate(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 3, 14, 23, 59, 0, 0, time.UTC)
	if got := FormatDate(when); got != "2026-03-14" {
		t.Errorf("FormatDate = %q, want 2026-03-14", got)
	}
	if got := FormatDate(time.Time{}); got != "" {
		t.Errorf("FormatDate(zero) = %q, want empty", got)
	}
}

func TestMigrationsAreWellFormed(t *testing.T) {
	t.Parallel()
	got, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no migrations were embedded")
	}
	for i, m := range got {
		if i > 0 && m.version <= got[i-1].version {
			t.Errorf("migrations are not in ascending order at %s", m.name)
		}
		if strings.TrimSpace(m.body) == "" {
			t.Errorf("migration %s is empty", m.name)
		}
	}
	if last := got[len(got)-1].version; last != SchemaVersion {
		t.Errorf("highest migration is %d, want SchemaVersion %d", last, SchemaVersion)
	}
}

func TestVersionOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		want    int
		wantErr bool
	}{
		{name: "0001_init.sql", want: 1},
		{name: "0042_add_thing.sql", want: 42},
		{name: "12_x.sql", want: 12},
		{name: "init.sql", wantErr: true},
		{name: "abc_init.sql", wantErr: true},
		{name: "0000_init.sql", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := versionOf(tc.name)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("versionOf(%q) succeeded, want an error", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("versionOf(%q) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}
