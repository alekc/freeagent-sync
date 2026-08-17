package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Run modes. An incremental run advances the stored cursor; the other two
// never do, because an operator asking a narrow question must not leave the
// next scheduled run believing it is caught up.
const (
	ModeIncremental = "incremental"
	ModeFull        = "full"
	ModeAdHoc       = "ad-hoc"
	ModeReconcile   = "reconcile"
	ModeProbe       = "probe"
)

// Run outcomes, recorded so "when did this last actually work" is a query.
const (
	OutcomeOK        = "ok"
	OutcomePartial   = "partial"
	OutcomeFailed    = "failed"
	OutcomeBudget    = "budget-exhausted"
	OutcomeCancelled = "cancelled"
)

// AdvancesCursor reports whether a mode is allowed to move the high-water
// mark. Centralised because getting it wrong loses records silently.
func AdvancesCursor(mode string) bool {
	return mode == ModeIncremental || mode == ModeFull
}

// FamilyState is the per-family, per-scope sync position. Scope is empty for
// plain collections and carries the bank account URL for the families that
// require one.
type FamilyState struct {
	Family               string
	Scope                string
	Cursor               time.Time
	LastFullReconcile    time.Time
	SupportsUpdatedSince *bool
	LastRunID            int64
}

// FamilyState reads the sync position, returning a zero value when the family
// has never been synced rather than an error.
func (d *DB) FamilyState(
	ctx context.Context, accountID int64, family, scope string,
) (FamilyState, error) {
	state := FamilyState{Family: family, Scope: scope}

	var cursor, reconcile sql.NullString
	var supports sql.NullBool
	var runID sql.NullInt64
	err := d.QueryRowContext(ctx,
		`SELECT cursor_updated_at, last_full_reconcile_at, supports_updated_since, last_run_id
		 FROM sync_state WHERE account_id = ? AND family = ? AND scope = ?`,
		accountID, family, scope).Scan(&cursor, &reconcile, &supports, &runID)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("store: reading sync state for %s: %w", family, err)
	}

	if state.Cursor, err = parseNullTime(cursor); err != nil {
		return state, err
	}
	if state.LastFullReconcile, err = parseNullTime(reconcile); err != nil {
		return state, err
	}
	if supports.Valid {
		state.SupportsUpdatedSince = &supports.Bool
	}
	state.LastRunID = runID.Int64
	return state, nil
}

// SaveCursor advances the high-water mark. Callers pass the maximum
// updated_at actually observed in the responses, never time.Now, so a record
// written while the run was in flight is picked up next time instead of
// being skipped.
func (d *DB) SaveCursor(
	ctx context.Context, accountID int64, family, scope string, cursor time.Time, runID int64,
) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO sync_state (account_id, family, scope, cursor_updated_at, last_run_id)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (account_id, family, scope) DO UPDATE SET
		   cursor_updated_at = excluded.cursor_updated_at,
		   last_run_id = excluded.last_run_id`,
		accountID, family, scope, NullTime(cursor), runID)
	if err != nil {
		return fmt.Errorf("store: saving the cursor for %s: %w", family, err)
	}
	return nil
}

// SaveReconcile records that a full sweep completed, which is what decides
// when the next one is due.
func (d *DB) SaveReconcile(
	ctx context.Context, accountID int64, family, scope string, at time.Time, runID int64,
) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO sync_state (account_id, family, scope, last_full_reconcile_at, last_run_id)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (account_id, family, scope) DO UPDATE SET
		   last_full_reconcile_at = excluded.last_full_reconcile_at,
		   last_run_id = excluded.last_run_id`,
		accountID, family, scope, NullTime(at), runID)
	if err != nil {
		return fmt.Errorf("store: recording the reconcile of %s: %w", family, err)
	}
	return nil
}

// SaveUpdatedSinceSupport records what the probe established. A family that
// ignores the filter returns everything, which looks like success, so this is
// stored rather than re-guessed on every run.
func (d *DB) SaveUpdatedSinceSupport(
	ctx context.Context, accountID int64, family, scope string, supported bool,
) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO sync_state (account_id, family, scope, supports_updated_since)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (account_id, family, scope) DO UPDATE SET
		   supports_updated_since = excluded.supports_updated_since`,
		accountID, family, scope, supported)
	if err != nil {
		return fmt.Errorf("store: recording updated_since support for %s: %w", family, err)
	}
	return nil
}

// FamilyStates lists every known sync position for an account, in family
// order, for status reporting.
func (d *DB) FamilyStates(ctx context.Context, accountID int64) ([]FamilyState, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT family, scope, cursor_updated_at, last_full_reconcile_at,
		        supports_updated_since, last_run_id
		 FROM sync_state WHERE account_id = ? ORDER BY family, scope`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: listing sync state: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []FamilyState
	for rows.Next() {
		var (
			s                 FamilyState
			cursor, reconcile sql.NullString
			supports          sql.NullBool
			runID             sql.NullInt64
		)
		if err := rows.Scan(&s.Family, &s.Scope, &cursor, &reconcile,
			&supports, &runID); err != nil {
			return nil, fmt.Errorf("store: listing sync state: %w", err)
		}
		if s.Cursor, err = parseNullTime(cursor); err != nil {
			return nil, err
		}
		if s.LastFullReconcile, err = parseNullTime(reconcile); err != nil {
			return nil, err
		}
		if supports.Valid {
			s.SupportsUpdatedSince = &supports.Bool
		}
		s.LastRunID = runID.Int64
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing sync state: %w", err)
	}
	return out, nil
}

// RunWindow is the pair of windows a run was asked for, recorded so a later
// reader can tell a narrow ad-hoc run from a full one.
type RunWindow struct {
	From         time.Time
	To           time.Time
	ChangedSince time.Time
	ChangedUntil time.Time
}

// RunSummary is what a finished run reports.
type RunSummary struct {
	Families        []string
	Requests        int64
	RecordsUpserted int64
	RecordsDeleted  int64
	BytesDownloaded int64
	Outcome         string
	Err             error
}

// StartRun opens a run record and returns its id.
func (d *DB) StartRun(
	ctx context.Context, accountID int64, mode string, window RunWindow,
) (int64, error) {
	res, err := d.ExecContext(ctx,
		`INSERT INTO sync_runs
		 (account_id, mode, started_at, window_from, window_to, changed_since, changed_until)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		accountID, mode, FormatTime(time.Now()),
		NullTime(window.From), NullTime(window.To),
		NullTime(window.ChangedSince), NullTime(window.ChangedUntil))
	if err != nil {
		return 0, fmt.Errorf("store: opening a run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: opening a run: %w", err)
	}
	return id, nil
}

// FinishRun closes a run record. It is called even when the run failed, so an
// unfinished row means the process died rather than that nothing happened.
func (d *DB) FinishRun(ctx context.Context, runID int64, summary RunSummary) error {
	var message any
	if summary.Err != nil {
		message = summary.Err.Error()
	}
	_, err := d.ExecContext(ctx,
		`UPDATE sync_runs SET
		   finished_at = ?, families = ?, requests = ?, records_upserted = ?,
		   records_deleted = ?, bytes_downloaded = ?, outcome = ?, error = ?
		 WHERE id = ?`,
		FormatTime(time.Now()), strings.Join(summary.Families, ","),
		summary.Requests, summary.RecordsUpserted, summary.RecordsDeleted,
		summary.BytesDownloaded, summary.Outcome, message, runID)
	if err != nil {
		return fmt.Errorf("store: closing run %d: %w", runID, err)
	}
	return nil
}

// Run is a completed or in-flight run, for status output.
type Run struct {
	ID              int64
	Mode            string
	StartedAt       time.Time
	FinishedAt      time.Time
	Families        string
	Requests        int64
	RecordsUpserted int64
	RecordsDeleted  int64
	BytesDownloaded int64
	Outcome         string
	Error           string
}

// RecentRuns lists an account's most recent runs, newest first.
func (d *DB) RecentRuns(ctx context.Context, accountID int64, limit int) ([]Run, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, mode, started_at, finished_at, families, requests,
		        records_upserted, records_deleted, bytes_downloaded, outcome, error
		 FROM sync_runs WHERE account_id = ?
		 ORDER BY started_at DESC, id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Run
	for rows.Next() {
		var (
			r                 Run
			started           string
			finished, outcome sql.NullString
			failure           sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.Mode, &started, &finished, &r.Families,
			&r.Requests, &r.RecordsUpserted, &r.RecordsDeleted,
			&r.BytesDownloaded, &outcome, &failure); err != nil {
			return nil, fmt.Errorf("store: listing runs: %w", err)
		}
		if r.StartedAt, err = ParseTime(started); err != nil {
			return nil, err
		}
		if r.FinishedAt, err = parseNullTime(finished); err != nil {
			return nil, err
		}
		r.Outcome, r.Error = outcome.String, failure.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing runs: %w", err)
	}
	return out, nil
}

// SaveCapability records a probe result.
func (d *DB) SaveCapability(
	ctx context.Context, accountID int64, family, probe, result, detail string,
) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO capabilities (account_id, family, probe, result, detail, probed_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (account_id, family, probe) DO UPDATE SET
		   result = excluded.result, detail = excluded.detail, probed_at = excluded.probed_at`,
		accountID, family, probe, result, detail, FormatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("store: recording the %s probe for %s: %w", probe, family, err)
	}
	return nil
}

func parseNullTime(s sql.NullString) (time.Time, error) {
	if !s.Valid {
		return time.Time{}, nil
	}
	return ParseTime(s.String)
}
