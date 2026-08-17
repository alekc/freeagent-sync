package store

import (
	"errors"
	"testing"
	"time"
)

func TestFamilyStateIsZeroWhenUnsynced(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	state, err := db.FamilyState(t.Context(), 1, "bills", "")
	if err != nil {
		t.Fatalf("an unsynced family should not be an error: %v", err)
	}
	if !state.Cursor.IsZero() || state.SupportsUpdatedSince != nil {
		t.Errorf("state = %+v, want a zero value", state)
	}
}

// Cursor, reconcile time and capability are written by different code paths
// at different times, so each upsert must leave the others alone.
func TestSaveCursorAndReconcileDoNotClobberEachOther(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)
	runID := startTestRun(t, db)

	cursor := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	reconcile := time.Date(2026, 3, 10, 2, 0, 0, 0, time.UTC)

	if err := db.SaveCursor(t.Context(), 1, "bills", "", cursor, runID); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveReconcile(t.Context(), 1, "bills", "", reconcile, runID); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveUpdatedSinceSupport(t.Context(), 1, "bills", "", true); err != nil {
		t.Fatal(err)
	}

	state, err := db.FamilyState(t.Context(), 1, "bills", "")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Cursor.Equal(cursor) {
		t.Errorf("Cursor = %s, want %s", state.Cursor, cursor)
	}
	if !state.LastFullReconcile.Equal(reconcile) {
		t.Errorf("LastFullReconcile = %s, want %s", state.LastFullReconcile, reconcile)
	}
	if state.SupportsUpdatedSince == nil || !*state.SupportsUpdatedSince {
		t.Errorf("SupportsUpdatedSince = %v, want true", state.SupportsUpdatedSince)
	}
	if state.LastRunID != runID {
		t.Errorf("LastRunID = %d, want %d", state.LastRunID, runID)
	}
}

// Unknown is a third state, distinct from "does not support it". A family
// that has never been probed must not be assumed either way.
func TestUpdatedSinceSupportHasThreeStates(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	state, err := db.FamilyState(t.Context(), 1, "bills", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.SupportsUpdatedSince != nil {
		t.Fatal("an unprobed family reports a capability it has not been asked about")
	}

	if err := db.SaveUpdatedSinceSupport(t.Context(), 1, "bills", "", false); err != nil {
		t.Fatal(err)
	}
	state, err = db.FamilyState(t.Context(), 1, "bills", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.SupportsUpdatedSince == nil || *state.SupportsUpdatedSince {
		t.Errorf("SupportsUpdatedSince = %v, want a stored false", state.SupportsUpdatedSince)
	}
}

// Scope separates the same family read through different bank accounts.
func TestFamilyStateIsScoped(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)
	runID := startTestRun(t, db)

	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if err := db.SaveCursor(t.Context(), 1, "bank_transactions", "acct/1", first, runID); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCursor(t.Context(), 1, "bank_transactions", "acct/2", second, runID); err != nil {
		t.Fatal(err)
	}

	one, err := db.FamilyState(t.Context(), 1, "bank_transactions", "acct/1")
	if err != nil {
		t.Fatal(err)
	}
	if !one.Cursor.Equal(first) {
		t.Errorf("scope acct/1 cursor = %s, want %s", one.Cursor, first)
	}

	states, err := db.FamilyStates(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Errorf("listed %d states, want 2", len(states))
	}
}

func TestAdvancesCursor(t *testing.T) {
	t.Parallel()
	advancing := []string{ModeIncremental, ModeFull}
	notAdvancing := []string{ModeAdHoc, ModeReconcile, ModeProbe}

	for _, mode := range advancing {
		if !AdvancesCursor(mode) {
			t.Errorf("%s should advance the cursor", mode)
		}
	}
	// An ad-hoc run answering a narrow question must never leave the next
	// scheduled run believing it is caught up.
	for _, mode := range notAdvancing {
		if AdvancesCursor(mode) {
			t.Errorf("%s must not advance the cursor", mode)
		}
	}
}

func TestRunLifecycle(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	window := RunWindow{
		From:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ChangedSince: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	runID, err := db.StartRun(t.Context(), 1, ModeAdHoc, window)
	if err != nil {
		t.Fatal(err)
	}

	summary := RunSummary{
		Families:        []string{"bills", "invoices"},
		Requests:        12,
		RecordsUpserted: 340,
		RecordsDeleted:  2,
		BytesDownloaded: 1024,
		Outcome:         OutcomeOK,
	}
	if err := db.FinishRun(t.Context(), runID, summary); err != nil {
		t.Fatal(err)
	}

	runs, err := db.RecentRuns(t.Context(), 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("listed %d runs, want 1", len(runs))
	}
	got := runs[0]
	if got.Mode != ModeAdHoc || got.Outcome != OutcomeOK {
		t.Errorf("run = %+v, want an ad-hoc run that succeeded", got)
	}
	if got.Families != "bills,invoices" || got.Requests != 12 || got.RecordsUpserted != 340 {
		t.Errorf("run summary did not round trip: %+v", got)
	}
	if got.FinishedAt.IsZero() {
		t.Error("FinishedAt was not recorded")
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty on a clean run", got.Error)
	}
}

// A failed run is still closed, so an unfinished row means the process died
// rather than that the run did nothing.
func TestFinishRunRecordsFailure(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	runID, err := db.StartRun(t.Context(), 1, ModeIncremental, RunWindow{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishRun(t.Context(), runID, RunSummary{
		Outcome: OutcomeFailed,
		Err:     errors.New("token expired"),
	}); err != nil {
		t.Fatal(err)
	}

	runs, err := db.RecentRuns(t.Context(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Outcome != OutcomeFailed || runs[0].Error != "token expired" {
		t.Errorf("run = %+v, want the failure recorded", runs[0])
	}
}

func TestRecentRunsAreNewestFirst(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	var ids []int64
	for range 3 {
		id, err := db.StartRun(t.Context(), 1, ModeIncremental, RunWindow{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	runs, err := db.RecentRuns(t.Context(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("listed %d runs, want the limit of 2", len(runs))
	}
	if runs[0].ID != ids[2] {
		t.Errorf("newest run = %d, want %d", runs[0].ID, ids[2])
	}
}

// An in-flight run has no outcome yet, which status renders differently from
// one that finished.
func TestUnfinishedRunHasNoOutcome(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	if _, err := db.StartRun(t.Context(), 1, ModeIncremental, RunWindow{}); err != nil {
		t.Fatal(err)
	}
	runs, err := db.RecentRuns(t.Context(), 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Outcome != "" || !runs[0].FinishedAt.IsZero() {
		t.Errorf("run = %+v, want it open", runs[0])
	}
}

func TestSaveCapabilityUpserts(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	if err := db.SaveCapability(
		t.Context(), 1, "bills", "updated_since", "honoured", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCapability(
		t.Context(), 1, "bills", "updated_since", "ignored", "returned 40 records"); err != nil {
		t.Fatal(err)
	}

	var result, detail string
	err := db.QueryRowContext(t.Context(),
		`SELECT result, detail FROM capabilities
		 WHERE account_id = 1 AND family = 'bills' AND probe = 'updated_since'`).
		Scan(&result, &detail)
	if err != nil {
		t.Fatal(err)
	}
	if result != "ignored" || detail != "returned 40 records" {
		t.Errorf("capability = %q/%q, want the second probe to have replaced the first",
			result, detail)
	}
}
