package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// ReportSnapshot is one dated capture of a derived report.
//
// Reports are not upserted like records. A profit and loss for last quarter is
// not a stale version of this quarter's: it is a different answer to a
// different question, and both are worth keeping.
type ReportSnapshot struct {
	ID       int64
	Report   string
	FromDate string
	ToDate   string
	TakenAt  time.Time
	Body     []byte
	SHA256   string
}

// SaveReportSnapshot stores a capture, skipping one whose body is identical to
// the newest capture of the same report and window. Re-running a pull hourly
// should not fill the table with copies of an unchanged report.
func (d *DB) SaveReportSnapshot(
	ctx context.Context, accountID int64, report, fromDate, toDate string, body []byte,
) (bool, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return false, fmt.Errorf("store: %s is not valid JSON: %w", report, err)
	}
	sum := sha256.Sum256(compact.Bytes())
	digest := hex.EncodeToString(sum[:])

	var previous string
	err := d.QueryRowContext(ctx,
		`SELECT body_sha256 FROM report_snapshots
		 WHERE account_id = ? AND report = ?
		   AND coalesce(from_date, '') = ? AND coalesce(to_date, '') = ?
		 ORDER BY taken_at DESC, id DESC LIMIT 1`,
		accountID, report, fromDate, toDate).Scan(&previous)
	if err == nil && previous == digest {
		return false, nil
	}

	if _, err := d.ExecContext(ctx,
		`INSERT INTO report_snapshots
		 (account_id, report, from_date, to_date, taken_at, body, body_sha256)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		accountID, report, nullString(fromDate), nullString(toDate),
		FormatTime(time.Now()), compact.String(), digest); err != nil {
		return false, fmt.Errorf("store: saving the %s snapshot: %w", report, err)
	}
	return true, nil
}

// LatestReportSnapshot returns the most recent capture of a report, ignoring
// the window it was taken for.
func (d *DB) LatestReportSnapshot(
	ctx context.Context, accountID int64, report string,
) (*ReportSnapshot, error) {
	var (
		snap           ReportSnapshot
		from, to, body string
		taken          string
	)
	err := d.QueryRowContext(ctx,
		`SELECT id, report, coalesce(from_date, ''), coalesce(to_date, ''),
		        taken_at, body, body_sha256
		 FROM report_snapshots WHERE account_id = ? AND report = ?
		 ORDER BY taken_at DESC, id DESC LIMIT 1`, accountID, report).
		Scan(&snap.ID, &snap.Report, &from, &to, &taken, &body, &snap.SHA256)
	if err != nil {
		return nil, fmt.Errorf("store: reading the %s snapshot: %w", report, err)
	}

	snap.FromDate, snap.ToDate, snap.Body = from, to, []byte(body)
	if snap.TakenAt, err = ParseTime(taken); err != nil {
		return nil, err
	}
	return &snap, nil
}

// ReportSnapshotCounts reports how many captures exist per report.
func (d *DB) ReportSnapshotCounts(
	ctx context.Context, accountID int64,
) (map[string]int64, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT report, count(*) FROM report_snapshots
		 WHERE account_id = ? GROUP BY report ORDER BY report`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: counting report snapshots: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var report string
		var count int64
		if err := rows.Scan(&report, &count); err != nil {
			return nil, fmt.Errorf("store: counting report snapshots: %w", err)
		}
		out[report] = count
	}
	return out, rows.Err()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
