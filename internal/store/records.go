package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Record is one archived payload, ready to be written. Body is the response
// exactly as received; Canonicalise fills in the derived fields.
type Record struct {
	Family    string
	URL       string
	RemoteID  string
	Body      []byte
	SHA256    string
	UpdatedAt time.Time
}

// UpsertStats reports what a batch did. Unchanged records still have their
// last_seen_at advanced, which is what the reconcile sweep reads.
type UpsertStats struct {
	Inserted  int
	Updated   int
	Unchanged int
	Restored  int
}

// Total is how many records the batch touched.
func (s UpsertStats) Total() int { return s.Inserted + s.Updated + s.Unchanged }

// Add accumulates another batch's counts.
func (s *UpsertStats) Add(other UpsertStats) {
	s.Inserted += other.Inserted
	s.Updated += other.Updated
	s.Unchanged += other.Unchanged
	s.Restored += other.Restored
}

// NewRecord builds a Record from a raw payload, deriving the URL, id, hash
// and modification time from the body itself.
func NewRecord(family string, body []byte) (Record, error) {
	r := Record{Family: family, Body: body}
	if err := r.canonicalise(""); err != nil {
		return Record{}, err
	}
	return r, nil
}

// NewDocumentRecord builds a Record for an endpoint that returns one document
// rather than a collection.
//
// Singletons such as company have no url field, because there is nothing to
// address: the endpoint is the identity. The caller supplies that endpoint as
// the URL, and the whole envelope is archived as one body rather than being
// unwrapped, so a shape surprise cannot lose anything.
func NewDocumentRecord(family, url string, body []byte) (Record, error) {
	if url == "" {
		return Record{}, fmt.Errorf("store: %s needs a document URL", family)
	}
	r := Record{Family: family, Body: body}
	if err := r.canonicalise(url); err != nil {
		return Record{}, err
	}
	return r, nil
}

// canonicalise compacts the JSON and derives the fields the archive indexes
// on. Compaction is deliberately the only normalisation: it makes the hash
// immune to whitespace without reformatting numbers, which would turn exact
// decimal strings into floats and lose money.
func (r *Record) canonicalise(overrideURL string) error {
	var buf bytes.Buffer
	if err := json.Compact(&buf, r.Body); err != nil {
		return fmt.Errorf("store: %s payload is not valid JSON: %w", r.Family, err)
	}
	r.Body = buf.Bytes()

	sum := sha256.Sum256(r.Body)
	r.SHA256 = hex.EncodeToString(sum[:])

	var envelope struct {
		URL       string `json:"url"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(r.Body, &envelope); err != nil {
		return fmt.Errorf("store: %s payload is not an object: %w", r.Family, err)
	}
	r.URL = envelope.URL
	if overrideURL != "" {
		r.URL = overrideURL
	}
	if r.URL == "" {
		return fmt.Errorf("store: %s payload has no url field", r.Family)
	}
	r.RemoteID = IDFromURL(r.URL)

	if envelope.UpdatedAt != "" {
		when, err := time.Parse(time.RFC3339, envelope.UpdatedAt)
		if err != nil {
			return fmt.Errorf("store: %s has an unparseable updated_at %q: %w",
				r.Family, envelope.UpdatedAt, err)
		}
		r.UpdatedAt = when
	}
	return nil
}

// IDFromURL returns the last path segment of a resource URL, which is the
// numeric id for most families and a date for the period-addressed ones.
func IDFromURL(url string) string {
	trimmed := strings.TrimRight(url, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// UpsertRecords archives a batch in one transaction. A record whose body is
// unchanged only has its last_seen_at advanced; a changed one also appends to
// the version history, which is never pruned.
func (d *DB) UpsertRecords(
	ctx context.Context, accountID int64, records []Record,
) (UpsertStats, error) {
	var stats UpsertStats
	if len(records) == 0 {
		return stats, nil
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("store: starting an upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := FormatTime(time.Now())
	for _, rec := range records {
		one, err := upsertOne(ctx, tx, accountID, rec, now)
		if err != nil {
			return stats, err
		}
		stats.Add(one)
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("store: committing an upsert: %w", err)
	}
	return stats, nil
}

func upsertOne(
	ctx context.Context, tx *sql.Tx, accountID int64, rec Record, now string,
) (UpsertStats, error) {
	var stats UpsertStats

	var existingSHA string
	var deletedAt sql.NullString
	err := tx.QueryRowContext(ctx,
		"SELECT body_sha256, deleted_at FROM records WHERE account_id = ? AND url = ?",
		accountID, rec.URL).Scan(&existingSHA, &deletedAt)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := insertRecord(ctx, tx, accountID, rec, now); err != nil {
			return stats, err
		}
		stats.Inserted++
	case err != nil:
		return stats, fmt.Errorf("store: reading %s: %w", rec.URL, err)
	case existingSHA == rec.SHA256:
		// Unchanged. last_seen_at still moves, because the reconcile sweep
		// uses it to decide what has gone from the far end.
		if err := touchRecord(ctx, tx, accountID, rec, now, deletedAt.Valid); err != nil {
			return stats, err
		}
		stats.Unchanged++
		if deletedAt.Valid {
			stats.Restored++
		}
	default:
		if err := updateRecord(ctx, tx, accountID, rec, now); err != nil {
			return stats, err
		}
		stats.Updated++
		if deletedAt.Valid {
			stats.Restored++
		}
	}

	if stats.Inserted+stats.Updated > 0 {
		if err := appendVersion(ctx, tx, accountID, rec, now); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func insertRecord(ctx context.Context, tx *sql.Tx, accountID int64, rec Record, now string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO records
		 (account_id, family, url, remote_id, body, body_sha256,
		  remote_updated_at, first_seen_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		accountID, rec.Family, rec.URL, rec.RemoteID, string(rec.Body), rec.SHA256,
		NullTime(rec.UpdatedAt), now, now)
	if err != nil {
		return fmt.Errorf("store: archiving %s: %w", rec.URL, err)
	}
	return nil
}

// updateRecord replaces the body and clears any soft delete: a record that
// came back is live again, and the history keeps both states.
func updateRecord(ctx context.Context, tx *sql.Tx, accountID int64, rec Record, now string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE records
		 SET body = ?, body_sha256 = ?, remote_updated_at = ?, last_seen_at = ?,
		     deleted_at = NULL, deleted_by_run = NULL
		 WHERE account_id = ? AND url = ?`,
		string(rec.Body), rec.SHA256, NullTime(rec.UpdatedAt), now, accountID, rec.URL)
	if err != nil {
		return fmt.Errorf("store: updating %s: %w", rec.URL, err)
	}
	return nil
}

func touchRecord(
	ctx context.Context, tx *sql.Tx, accountID int64, rec Record, now string, restore bool,
) error {
	query := `UPDATE records SET last_seen_at = ? WHERE account_id = ? AND url = ?`
	if restore {
		query = `UPDATE records SET last_seen_at = ?, deleted_at = NULL, deleted_by_run = NULL
		         WHERE account_id = ? AND url = ?`
	}
	if _, err := tx.ExecContext(ctx, query, now, accountID, rec.URL); err != nil {
		return fmt.Errorf("store: touching %s: %w", rec.URL, err)
	}
	return nil
}

func appendVersion(ctx context.Context, tx *sql.Tx, accountID int64, rec Record, now string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO record_versions
		 (account_id, url, body, body_sha256, remote_updated_at, seen_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		accountID, rec.URL, string(rec.Body), rec.SHA256, NullTime(rec.UpdatedAt), now)
	if err != nil {
		return fmt.Errorf("store: recording a version of %s: %w", rec.URL, err)
	}
	return nil
}

// SoftDeleteUnseen marks every live record in a family that a full sweep did
// not see. FreeAgent has no deletions feed, so this is the only way a removal
// is ever noticed. Rows are kept; only deleted_at is set.
func (d *DB) SoftDeleteUnseen(
	ctx context.Context, accountID int64, family string, sweepStart time.Time, runID int64,
) (int64, error) {
	if sweepStart.IsZero() {
		return 0, errors.New("store: SoftDeleteUnseen needs the time the sweep began")
	}
	res, err := d.ExecContext(ctx,
		`UPDATE records SET deleted_at = ?, deleted_by_run = ?
		 WHERE account_id = ? AND family = ? AND deleted_at IS NULL AND last_seen_at < ?`,
		FormatTime(time.Now()), nullID(runID), accountID, family, FormatTime(sweepStart))
	if err != nil {
		return 0, fmt.Errorf("store: sweeping %s: %w", family, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: sweeping %s: %w", family, err)
	}
	return n, nil
}

// nullID stores a missing run reference as NULL rather than as a zero that
// would fail the foreign key. A sweep normally runs inside a run; this keeps
// a manual one usable without inventing a run id.
func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// LiveRecordCount counts records in a family that have not been soft deleted.
func (d *DB) LiveRecordCount(ctx context.Context, accountID int64, family string) (int64, error) {
	var n int64
	err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM records
		 WHERE account_id = ? AND family = ? AND deleted_at IS NULL`,
		accountID, family).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting %s: %w", family, err)
	}
	return n, nil
}

// RecordBody returns one archived payload, whether or not it is soft deleted.
func (d *DB) RecordBody(ctx context.Context, accountID int64, url string) ([]byte, error) {
	var body string
	err := d.QueryRowContext(ctx,
		"SELECT body FROM records WHERE account_id = ? AND url = ?", accountID, url).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: %s is not archived", url)
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", url, err)
	}
	return []byte(body), nil
}

// VersionCount reports how many distinct bodies have been archived for a
// record, which is how many times it changed plus one.
func (d *DB) VersionCount(ctx context.Context, accountID int64, url string) (int, error) {
	var n int
	err := d.QueryRowContext(ctx,
		"SELECT count(*) FROM record_versions WHERE account_id = ? AND url = ?",
		accountID, url).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting versions of %s: %w", url, err)
	}
	return n, nil
}

// LiveRecordBodies returns every non-deleted body in a family. Used where one
// family's records are the input to reading another, as bank accounts are for
// bank transactions.
func (d *DB) LiveRecordBodies(
	ctx context.Context, accountID int64, family string,
) ([][]byte, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT body FROM records
		 WHERE account_id = ? AND family = ? AND deleted_at IS NULL
		 ORDER BY url`, accountID, family)
	if err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", family, err)
	}
	defer func() { _ = rows.Close() }()

	var out [][]byte
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("store: reading %s: %w", family, err)
		}
		out = append(out, []byte(body))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", family, err)
	}
	return out, nil
}

// RecordRow is one archived record, for the passes that walk all of them.
type RecordRow struct {
	Family    string
	URL       string
	RemoteID  string
	Body      []byte
	SHA256    string
	UpdatedAt time.Time
	Deleted   bool
}

// EachRecord streams every record for an account in family and URL order.
// Streamed rather than returned as a slice: an archive of a real company is
// tens of thousands of bodies, and the tree passes only need one at a time.
func (d *DB) EachRecord(
	ctx context.Context, accountID int64, fn func(RecordRow) error,
) error {
	rows, err := d.QueryContext(ctx,
		`SELECT family, url, remote_id, body, body_sha256, remote_updated_at,
		        deleted_at IS NOT NULL
		 FROM records WHERE account_id = ? ORDER BY family, url`, accountID)
	if err != nil {
		return fmt.Errorf("store: walking records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			r       RecordRow
			body    string
			updated sql.NullString
		)
		if err := rows.Scan(&r.Family, &r.URL, &r.RemoteID, &body,
			&r.SHA256, &updated, &r.Deleted); err != nil {
			return fmt.Errorf("store: walking records: %w", err)
		}
		r.Body = []byte(body)
		if r.UpdatedAt, err = parseNullTime(updated); err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// VersionRow is one entry in a record's history.
type VersionRow struct {
	URL    string
	Body   []byte
	SHA256 string
	SeenAt time.Time
}

// EachVersion streams the whole version history in URL and time order, so a
// caller can group by record without a query per record.
func (d *DB) EachVersion(
	ctx context.Context, accountID int64, fn func(VersionRow) error,
) error {
	rows, err := d.QueryContext(ctx,
		`SELECT url, body, body_sha256, seen_at
		 FROM record_versions WHERE account_id = ? ORDER BY url, seen_at, id`, accountID)
	if err != nil {
		return fmt.Errorf("store: walking versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			v      VersionRow
			body   string
			seenAt string
		)
		if err := rows.Scan(&v.URL, &body, &v.SHA256, &seenAt); err != nil {
			return fmt.Errorf("store: walking versions: %w", err)
		}
		v.Body = []byte(body)
		if v.SeenAt, err = ParseTime(seenAt); err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ArchivedURLs maps every live record's URL to its family. Used by the
// integrity check that looks for cross-references pointing at nothing.
func (d *DB) ArchivedURLs(ctx context.Context, accountID int64) (map[string]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT url, family FROM records WHERE account_id = ? AND deleted_at IS NULL`,
		accountID)
	if err != nil {
		return nil, fmt.Errorf("store: listing archived URLs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var url, family string
		if err := rows.Scan(&url, &family); err != nil {
			return nil, fmt.Errorf("store: listing archived URLs: %w", err)
		}
		out[url] = family
	}
	return out, rows.Err()
}

// LiveRecordBodiesInWindow returns the bodies of a family whose dated_on falls
// inside a window, which is how the reconciliation against a report selects
// the same transactions the report covers.
func (d *DB) LiveRecordBodiesInWindow(
	ctx context.Context, accountID int64, family, fromDate, toDate string,
) ([][]byte, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT body FROM records
		 WHERE account_id = ? AND family = ? AND deleted_at IS NULL
		   AND (? = '' OR json_extract(body, '$.dated_on') >= ?)
		   AND (? = '' OR json_extract(body, '$.dated_on') <= ?)
		 ORDER BY url`,
		accountID, family, fromDate, fromDate, toDate, toDate)
	if err != nil {
		return nil, fmt.Errorf("store: reading %s in window: %w", family, err)
	}
	defer func() { _ = rows.Close() }()

	var out [][]byte
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("store: reading %s in window: %w", family, err)
		}
		out = append(out, []byte(body))
	}
	return out, rows.Err()
}

// RecordBodiesForExport returns bodies in a family including soft-deleted
// ones, for an export that deliberately wants the whole history.
func (d *DB) RecordBodiesForExport(
	ctx context.Context, accountID int64, family, fromDate, toDate string,
) ([][]byte, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT body FROM records
		 WHERE account_id = ? AND family = ?
		   AND (? = '' OR json_extract(body, '$.dated_on') >= ?)
		   AND (? = '' OR json_extract(body, '$.dated_on') <= ?)
		 ORDER BY url`,
		accountID, family, fromDate, fromDate, toDate, toDate)
	if err != nil {
		return nil, fmt.Errorf("store: reading %s for export: %w", family, err)
	}
	defer func() { _ = rows.Close() }()

	var out [][]byte
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, fmt.Errorf("store: reading %s for export: %w", family, err)
		}
		out = append(out, []byte(body))
	}
	return out, rows.Err()
}
