package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alekc/freeagent-sync/internal/store"
)

var (
	march = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	april = time.Date(2026, 4, 2, 11, 30, 0, 0, time.UTC)
)

func TestPullArchivesACollection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills",
		fakeRecord{ID: 1, UpdatedAt: march, Extra: "B1"},
		fakeRecord{ID: 2, UpdatedAt: april, Extra: "B2"},
	)

	result := h.pull(Options{Mode: store.ModeFull})

	if result.Outcome != store.OutcomeOK {
		t.Errorf("outcome = %s, want ok", result.Outcome)
	}
	if got := h.liveCount("bills"); got != 2 {
		t.Errorf("archived %d bills, want 2", got)
	}
	bills := h.familyResult(result, "bills")
	if bills.Stats.Inserted != 2 {
		t.Errorf("stats = %+v, want two inserts", bills.Stats)
	}
}

func TestPullWalksEveryPage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.perPage = 2

	var records []fakeRecord
	for i := 1; i <= 7; i++ {
		records = append(records, fakeRecord{ID: i, UpdatedAt: march})
	}
	h.fake.set("bills", records...)

	result := h.pull(Options{Mode: store.ModeFull})

	if got := h.liveCount("bills"); got != 7 {
		t.Errorf("archived %d bills, want 7", got)
	}
	if pages := h.familyResult(result, "bills").Pages; pages != 4 {
		t.Errorf("walked %d pages, want 4", pages)
	}
}

// The cursor must be the latest updated_at actually seen, never the clock. A
// clock-based cursor would skip anything written while the run was in flight.
func TestPullCursorComesFromThePayloads(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills",
		fakeRecord{ID: 1, UpdatedAt: march},
		fakeRecord{ID: 2, UpdatedAt: april},
	)

	h.pull(Options{Mode: store.ModeFull})

	got := h.cursor("bills")
	if !got.Equal(april) {
		t.Errorf("cursor = %s, want the latest updated_at %s", got, april)
	}
	if got.After(time.Now().Add(-time.Minute)) {
		t.Error("the cursor looks like the clock rather than the data")
	}
}

func TestPullIncrementalSendsUpdatedSinceWithOverlap(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	h.pull(Options{Mode: store.ModeFull})
	h.pull(Options{Mode: store.ModeIncremental, Overlap: time.Hour})

	raw := h.fake.queryFor("bills").Get("updated_since")
	if raw == "" {
		t.Fatal("the second run did not send updated_since")
	}
	sent, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("updated_since %q is not RFC 3339: %v", raw, err)
	}
	if want := march.Add(-time.Hour); !sent.Equal(want) {
		t.Errorf("updated_since = %s, want the cursor less the overlap %s", sent, want)
	}
}

// The first run of a family has no cursor, so it must read everything rather
// than filtering against a zero time.
func TestPullFirstRunReadsEverything(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	result := h.pull(Options{Mode: store.ModeIncremental})

	if h.fake.queryFor("bills").Get("updated_since") != "" {
		t.Error("the first run filtered on a cursor it does not have")
	}
	if !h.familyResult(result, "bills").FullScan {
		t.Error("the first run was not treated as a full scan")
	}
}

// An ad-hoc window answers a narrow question. Advancing the cursor from it
// would leave the next scheduled run believing it is caught up, and the gap
// would only surface at the next reconcile.
func TestAdHocRunNeverAdvancesTheCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills",
		fakeRecord{ID: 1, UpdatedAt: march},
		fakeRecord{ID: 2, UpdatedAt: april},
	)

	h.pull(Options{Mode: store.ModeFull})
	before := h.cursor("bills")

	h.fake.set("bills", fakeRecord{ID: 3, UpdatedAt: april.Add(24 * time.Hour)})
	result := h.pull(Options{
		Mode:   store.ModeAdHoc,
		Window: store.RunWindow{ChangedSince: march},
	})

	if after := h.cursor("bills"); !after.Equal(before) {
		t.Errorf("cursor moved from %s to %s on an ad-hoc run", before, after)
	}
	if h.familyResult(result, "bills").CursorAdvance {
		t.Error("the ad-hoc result claims it advanced the cursor")
	}
	// It still archives what it found; only the bookkeeping is withheld.
	if got := h.liveCount("bills"); got != 3 {
		t.Errorf("archived %d bills, want 3", got)
	}
}

// FreeAgent has no deletions feed, so a sweep is the only way a removal is
// ever noticed.
func TestReconcileSoftDeletesWhatDisappeared(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills",
		fakeRecord{ID: 1, UpdatedAt: march},
		fakeRecord{ID: 2, UpdatedAt: march},
	)
	h.pull(Options{Mode: store.ModeFull})

	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})
	result := h.pull(Options{Mode: store.ModeFull, Reconcile: true})

	bills := h.familyResult(result, "bills")
	if bills.Deleted != 1 {
		t.Errorf("swept %d records, want 1", bills.Deleted)
	}
	if got := h.liveCount("bills"); got != 1 {
		t.Errorf("live count = %d, want 1", got)
	}

	// Soft: the body is still there, which is the whole point of an archive.
	body, err := h.db.RecordBody(t.Context(), h.account.ID, "https://api.test/v2/bills/2")
	if err != nil {
		t.Errorf("the swept record was actually removed: %v", err)
	}
	if !strings.Contains(string(body), "bills/2") {
		t.Errorf("swept body is wrong: %s", body)
	}
}

func TestReconcileRestoresARecordThatCameBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	both := []fakeRecord{{ID: 1, UpdatedAt: march}, {ID: 2, UpdatedAt: march}}

	h.fake.set("bills", both...)
	h.pull(Options{Mode: store.ModeFull, Reconcile: true})

	h.fake.set("bills", both[0])
	h.pull(Options{Mode: store.ModeFull, Reconcile: true})
	if got := h.liveCount("bills"); got != 1 {
		t.Fatalf("live count = %d after the sweep, want 1", got)
	}

	h.fake.set("bills", both...)
	result := h.pull(Options{Mode: store.ModeFull, Reconcile: true})

	if got := h.liveCount("bills"); got != 2 {
		t.Errorf("live count = %d after the record returned, want 2", got)
	}
	if restored := h.familyResult(result, "bills").Stats.Restored; restored != 1 {
		t.Errorf("restored %d records, want 1", restored)
	}
}

// An incremental run sees only what changed, so it cannot tell absence from
// unchanged and must never sweep.
func TestIncrementalRunDoesNotSweep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills",
		fakeRecord{ID: 1, UpdatedAt: march},
		fakeRecord{ID: 2, UpdatedAt: march},
	)
	h.pull(Options{Mode: store.ModeFull})

	h.fake.set("bills")
	result := h.pull(Options{Mode: store.ModeIncremental, Reconcile: true})

	if h.familyResult(result, "bills").Swept {
		t.Error("an incremental run swept, which would delete everything it did not re-read")
	}
	if got := h.liveCount("bills"); got != 2 {
		t.Errorf("live count = %d, want both records intact", got)
	}
}

// One flaky endpoint must not cost a night's sync of everything else.
func TestOneFailingFamilyDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})
	h.fake.set("contacts", fakeRecord{ID: 1, UpdatedAt: march})
	h.fake.failWith("bills", 500)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeFull,
		Families: []string{"bills", "contacts"},
	})
	if err != nil {
		t.Fatalf("Pull returned a hard error for one bad family: %v", err)
	}

	if result.Outcome != store.OutcomePartial {
		t.Errorf("outcome = %s, want partial", result.Outcome)
	}
	if len(result.Failed()) != 1 {
		t.Errorf("failed families = %d, want 1", len(result.Failed()))
	}
	if got := h.liveCount("contacts"); got != 1 {
		t.Errorf("contacts archived %d, want 1 despite the bills failure", got)
	}
}

// A family that failed has an unknown position, so its cursor must not move.
func TestAFailedFamilyKeepsItsCursor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})
	h.pull(Options{Mode: store.ModeFull})
	before := h.cursor("bills")

	h.fake.failWith("bills", 500)
	h.pull(Options{Mode: store.ModeIncremental})

	if after := h.cursor("bills"); !after.Equal(before) {
		t.Errorf("cursor moved from %s to %s despite the failure", before, after)
	}
}

func TestBudgetStopsTheRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.perPage = 1

	var records []fakeRecord
	for i := 1; i <= 20; i++ {
		records = append(records, fakeRecord{ID: i, UpdatedAt: march})
	}
	h.fake.set("bills", records...)

	result := h.pull(Options{Mode: store.ModeFull, MaxRequests: 3})

	if result.Outcome != store.OutcomeBudget {
		t.Errorf("outcome = %s, want the budget outcome", result.Outcome)
	}
	if got := h.fake.requestCount(); got > 4 {
		t.Errorf("made %d requests against a budget of 3", got)
	}
	if !h.cursor("bills").IsZero() {
		t.Error("a run stopped by its budget advanced the cursor anyway")
	}
	if !errors.Is(h.familyResult(result, "bills").Err, ErrBudgetExhausted) {
		t.Errorf("family error = %v, want ErrBudgetExhausted",
			h.familyResult(result, "bills").Err)
	}
}

// A family the probe found ignores updated_since is read in full instead,
// because filtering it would be a lie either way.
func TestAFamilyThatIgnoresUpdatedSinceIsReadInFull(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})
	h.pull(Options{Mode: store.ModeFull})

	if err := h.db.SaveUpdatedSinceSupport(
		t.Context(), h.account.ID, "bills", "", false); err != nil {
		t.Fatal(err)
	}

	result := h.pull(Options{Mode: store.ModeIncremental})

	if h.fake.queryFor("bills").Get("updated_since") != "" {
		t.Error("updated_since was sent to a family known to ignore it")
	}
	if !h.familyResult(result, "bills").FullScan {
		t.Error("the family was not read in full")
	}
}

func TestReconcileIfDueSkipsARecentSweep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	first := h.pull(Options{
		Mode: store.ModeFull, ReconcileIfDue: true, ReconcileInterval: time.Hour,
	})
	if !h.familyResult(first, "bills").Swept {
		t.Fatal("the first run did not sweep a never-swept family")
	}

	second := h.pull(Options{
		Mode: store.ModeFull, ReconcileIfDue: true, ReconcileInterval: time.Hour,
	})
	if h.familyResult(second, "bills").Swept {
		t.Error("a sweep ran again inside its interval")
	}
}

func TestPullRecordsTheRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	result := h.pull(Options{Mode: store.ModeFull})

	runs, err := h.db.RecentRuns(t.Context(), h.account.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d runs, want 1", len(runs))
	}
	run := runs[0]
	if run.ID != result.RunID {
		t.Errorf("run id = %d, want %d", run.ID, result.RunID)
	}
	if run.Outcome != store.OutcomeOK || run.FinishedAt.IsZero() {
		t.Errorf("run = %+v, want a closed successful run", run)
	}
	if run.Requests == 0 {
		t.Error("the run recorded no requests")
	}
}

// The families this build cannot archive are reported, not silently omitted.
func TestPullReportsDeferredFamilies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	result := h.pull(Options{Mode: store.ModeFull})

	for _, want := range []string{"attachments"} {
		if _, ok := result.Deferred[want]; !ok {
			t.Errorf("%s is not reported as deferred", want)
		}
	}
}

func TestPullRejectsAnUnknownFamily(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, err := h.engine.Pull(t.Context(), Options{Families: []string{"widgets"}})
	var unknown *UnknownFamilyError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want UnknownFamilyError", err)
	}
}

// Asking for a family that exists but needs a strategy this build lacks must
// say which, not archive nothing and report success.
func TestPullRejectsAnUnsupportedFamily(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	_, err := h.engine.Pull(t.Context(), Options{Families: []string{"attachments"}})
	var unsupported *UnsupportedFamilyError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want UnsupportedFamilyError", err)
	}
	if unsupported.Class != ClassChildOnly {
		t.Errorf("class = %s, want child-only", unsupported.Class)
	}
}

func TestPullAcrossManyFamilies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, family := range []string{"bills", "contacts", "invoices", "projects"} {
		h.fake.set(family, fakeRecord{ID: 1, UpdatedAt: march})
	}

	result, err := h.engine.Pull(t.Context(), Options{
		Mode:        store.ModeFull,
		Families:    []string{"invoices", "bills", "contacts", "projects"},
		Concurrency: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}

	// Results come back in pull order, not in the order they were requested
	// or the order the workers happened to finish.
	var got []string
	for _, f := range result.Families {
		got = append(got, f.Family)
	}
	want := []string{"contacts", "projects", "invoices", "bills"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("family order = %v, want dependency order %v", got, want)
	}
}

// The bar used to be sized at LastPage * PerPage, so a single page of six
// records claimed a total of a hundred and finished at six percent. The last
// page knows the real number.
func TestProgressTotalIsExactOnTheFinalPage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills",
		fakeRecord{ID: 1, UpdatedAt: march},
		fakeRecord{ID: 2, UpdatedAt: march},
		fakeRecord{ID: 3, UpdatedAt: march},
	)

	tracked := &recordingReporter{}
	engine := New(h.db, h.client, tracked, h.account)
	if _, err := engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"bills"},
	}); err != nil {
		t.Fatal(err)
	}

	total, value := tracked.finalFor("bills")
	if total != 3 {
		t.Errorf("final total = %d, want the exact record count 3", total)
	}
	if value != total {
		t.Errorf("bar finished at %d of %d, want it complete", value, total)
	}
}

// A 403 or 404 on a plain collection is the company not having the family, the
// same as on a scoped one. Only scoped jobs tolerated it, so an ordinary
// company reported a failure on every run.
func TestUnavailablePlainCollectionIsNotAFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})
	h.fake.failWith("sales_tax_periods", 404)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"bills", "sales_tax_periods"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Errorf("outcome = %s, want ok", result.Outcome)
	}
	if len(result.Failed()) != 0 {
		t.Errorf("%d failures, want none", len(result.Failed()))
	}
	if len(result.Unavailable()) != 1 {
		t.Errorf("unavailable = %d, want 1", len(result.Unavailable()))
	}
}

// A 5xx is a broken endpoint, not a missing feature, and must still fail.
func TestServerErrorStillFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.failWith("bills", 500)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"bills"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomePartial {
		t.Errorf("outcome = %s, want partial", result.Outcome)
	}
	if len(result.Unavailable()) != 0 {
		t.Error("a 500 was reported as the company not having the family")
	}
}
