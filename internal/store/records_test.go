package store

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

const billURL = "https://api.test/v2/bills/1"

func billBody(reference string) string {
	return `{"url":"` + billURL + `","reference":"` + reference +
		`","updated_at":"2026-03-14T09:26:53Z","total_value":"1440.00"}`
}

func TestNewRecordDerivesItsFields(t *testing.T) {
	t.Parallel()
	rec, err := NewRecord("bills", []byte(billBody("B1")))
	if err != nil {
		t.Fatal(err)
	}

	if rec.URL != billURL {
		t.Errorf("URL = %q, want %q", rec.URL, billURL)
	}
	if rec.RemoteID != "1" {
		t.Errorf("RemoteID = %q, want 1", rec.RemoteID)
	}
	if len(rec.SHA256) != 64 {
		t.Errorf("SHA256 = %q, want 64 hex characters", rec.SHA256)
	}
	want := time.Date(2026, 3, 14, 9, 26, 53, 0, time.UTC)
	if !rec.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %s, want %s", rec.UpdatedAt, want)
	}
}

// Compaction is the only normalisation, so reformatted whitespace must not
// look like a new version of the record.
func TestNewRecordHashIgnoresWhitespace(t *testing.T) {
	t.Parallel()
	dense, err := NewRecord("bills", []byte(billBody("B1")))
	if err != nil {
		t.Fatal(err)
	}
	spaced, err := NewRecord("bills", []byte(`{
		"url": "`+billURL+`",
		"reference": "B1",
		"updated_at": "2026-03-14T09:26:53Z",
		"total_value": "1440.00"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if dense.SHA256 != spaced.SHA256 {
		t.Error("whitespace changed the hash, which would fabricate a version")
	}
}

// Money arrives as an exact decimal string and must survive archiving
// untouched. Re-encoding through float64 would round it.
func TestNewRecordPreservesExactDecimals(t *testing.T) {
	t.Parallel()
	const body = `{"url":"` + billURL + `","total_value":"1440.005","rate":0.1234567890123}`
	rec, err := NewRecord("bills", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"1440.005"`, `0.1234567890123`} {
		if !strings.Contains(string(rec.Body), want) {
			t.Errorf("archived body lost %s: %s", want, rec.Body)
		}
	}
}

func TestNewRecordRejectsBadPayloads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body, wantErr string
	}{
		{"not json", `{nope}`, "not valid JSON"},
		{"no url", `{"reference":"B1"}`, "no url field"},
		{"bad updated_at", `{"url":"` + billURL + `","updated_at":"yesterday"}`, "updated_at"},
		{"an array", `[{"url":"x"}]`, "not an object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRecord("bills", []byte(tc.body))
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestIDFromURL(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"https://api.test/v2/bills/1":             "1",
		"https://api.test/v2/bills/1/":            "1",
		"https://api.test/v2/vat_returns/2026-03": "2026-03",
		"bills": "bills",
	}
	for in, want := range tests {
		if got := IDFromURL(in); got != want {
			t.Errorf("IDFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpsertInsertsAndVersions(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	rec, err := NewRecord("bills", []byte(billBody("B1")))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := db.UpsertRecords(t.Context(), 1, []Record{rec})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 1 || stats.Updated != 0 || stats.Unchanged != 0 {
		t.Errorf("stats = %+v, want one insert", stats)
	}

	versions, err := db.VersionCount(t.Context(), 1, billURL)
	if err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Errorf("versions = %d, want 1", versions)
	}
}

// An unchanged body must not append a version, or the history fills with
// duplicates on every scheduled run.
func TestUpsertUnchangedDoesNotVersion(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	rec, err := NewRecord("bills", []byte(billBody("B1")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRecords(t.Context(), 1, []Record{rec}); err != nil {
		t.Fatal(err)
	}
	stats, err := db.UpsertRecords(t.Context(), 1, []Record{rec})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Unchanged != 1 || stats.Updated != 0 || stats.Inserted != 0 {
		t.Errorf("stats = %+v, want one unchanged", stats)
	}

	versions, err := db.VersionCount(t.Context(), 1, billURL)
	if err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Errorf("versions = %d after re-seeing the same body, want 1", versions)
	}
}

func TestUpsertChangedAppendsAVersion(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	for _, reference := range []string{"B1", "B2", "B3"} {
		rec, err := NewRecord("bills", []byte(billBody(reference)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.UpsertRecords(t.Context(), 1, []Record{rec}); err != nil {
			t.Fatal(err)
		}
	}

	versions, err := db.VersionCount(t.Context(), 1, billURL)
	if err != nil {
		t.Fatal(err)
	}
	if versions != 3 {
		t.Errorf("versions = %d after three distinct bodies, want 3", versions)
	}

	body, err := db.RecordBody(t.Context(), 1, billURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"B3"`) {
		t.Errorf("current body is not the latest: %s", body)
	}
}

// A record the far end deleted and then restored is live again, and the
// history keeps both states.
func TestUpsertRestoresASoftDeletedRecord(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	rec, err := NewRecord("bills", []byte(billBody("B1")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRecords(t.Context(), 1, []Record{rec}); err != nil {
		t.Fatal(err)
	}

	runID := startTestRun(t, db)
	sweepStart := time.Now().Add(time.Second)
	if _, err := db.SoftDeleteUnseen(t.Context(), 1, "bills", sweepStart, runID); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.LiveRecordCount(t.Context(), 1, "bills"); n != 0 {
		t.Fatalf("live count = %d after the sweep, want 0", n)
	}

	stats, err := db.UpsertRecords(t.Context(), 1, []Record{rec})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Restored != 1 {
		t.Errorf("stats = %+v, want one restore", stats)
	}
	if n, _ := db.LiveRecordCount(t.Context(), 1, "bills"); n != 1 {
		t.Errorf("live count = %d after the record came back, want 1", n)
	}
}

// FreeAgent has no deletions feed, so the sweep is the only mechanism. It
// must catch what was not seen and leave alone what was.
func TestSoftDeleteUnseenOnlyTouchesStaleRows(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	stale, err := NewRecord("bills", []byte(
		`{"url":"https://api.test/v2/bills/1","reference":"gone"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRecords(t.Context(), 1, []Record{stale}); err != nil {
		t.Fatal(err)
	}

	// A sweep beginning now: only what is re-seen after this point survives.
	time.Sleep(2 * time.Millisecond)
	sweepStart := time.Now()
	time.Sleep(2 * time.Millisecond)

	fresh, err := NewRecord("bills", []byte(
		`{"url":"https://api.test/v2/bills/2","reference":"here"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRecords(t.Context(), 1, []Record{fresh}); err != nil {
		t.Fatal(err)
	}

	deleted, err := db.SoftDeleteUnseen(t.Context(), 1, "bills", sweepStart, startTestRun(t, db))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("swept %d records, want 1", deleted)
	}

	live, err := db.LiveRecordCount(t.Context(), 1, "bills")
	if err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Errorf("live count = %d, want 1", live)
	}

	// The swept row is still readable; nothing is ever removed.
	if _, err := db.RecordBody(t.Context(), 1, "https://api.test/v2/bills/1"); err != nil {
		t.Errorf("the swept record is gone: %v", err)
	}
}

func TestSoftDeleteUnseenNeedsASweepStart(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	if _, err := db.SoftDeleteUnseen(t.Context(), 1, "bills", time.Time{}, 1); err == nil {
		t.Fatal("a sweep with no start time was accepted; it would delete everything")
	}
}

func TestSoftDeleteUnseenIsScopedToOneFamily(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	for _, spec := range []struct{ family, url string }{
		{"bills", "https://api.test/v2/bills/1"},
		{"invoices", "https://api.test/v2/invoices/1"},
	} {
		rec, err := NewRecord(spec.family, []byte(`{"url":"`+spec.url+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.UpsertRecords(t.Context(), 1, []Record{rec}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := db.SoftDeleteUnseen(
		t.Context(), 1, "bills", time.Now().Add(time.Second), startTestRun(t, db)); err != nil {
		t.Fatal(err)
	}

	if n, _ := db.LiveRecordCount(t.Context(), 1, "invoices"); n != 1 {
		t.Errorf("sweeping bills also swept invoices: %d live", n)
	}
}

func TestUpsertHandlesABatch(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	var batch []Record
	for i := 1; i <= 50; i++ {
		rec, err := NewRecord("bills", []byte(
			`{"url":"https://api.test/v2/bills/`+strconv.Itoa(i)+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		batch = append(batch, rec)
	}

	stats, err := db.UpsertRecords(t.Context(), 1, batch)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 50 || stats.Total() != 50 {
		t.Errorf("stats = %+v, want 50 inserts", stats)
	}
}

func TestUpsertEmptyBatchIsANoOp(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	stats, err := db.UpsertRecords(t.Context(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Total() != 0 {
		t.Errorf("stats = %+v, want nothing", stats)
	}
}

// A sweep outside a run would leave no record of what deleted a row, so the
// schema requires one. Tests create a real one rather than dodging it.
func startTestRun(t *testing.T, db *DB) int64 {
	t.Helper()
	id, err := db.StartRun(t.Context(), 1, ModeReconcile, RunWindow{})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRecordBodyReportsMissing(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	if _, err := db.RecordBody(t.Context(), 1, "https://api.test/v2/bills/404"); err == nil {
		t.Fatal("reading an unarchived record succeeded")
	}
}
