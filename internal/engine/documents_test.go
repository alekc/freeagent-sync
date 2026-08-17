package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/alekc/freeagent-sync/internal/store"
)

// A singleton has no url field, because the endpoint is the identity. The
// whole envelope is archived, so a shape change cannot lose anything.
func TestSingletonIsArchivedAsOneDocument(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("company", `{"company":{"name":"Home Co","currency":"GBP",`+
		`"first_accounting_year_end":"2026-03-31"}}`)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"company"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}
	if got := h.liveCount("company"); got != 1 {
		t.Fatalf("archived %d company records, want 1", got)
	}

	body, err := h.db.RecordBody(t.Context(), h.account.ID, h.apiURL+"/company")
	if err != nil {
		t.Fatalf("the document is not filed under its endpoint: %v", err)
	}
	// The envelope is kept, not unwrapped: three singletons wrap their content
	// differently and one uses a key matching neither name.
	if !strings.Contains(string(body), `"company":{`) {
		t.Errorf("the envelope was unwrapped: %s", body)
	}
	if !strings.Contains(string(body), "Home Co") {
		t.Errorf("the content was lost: %s", body)
	}
}

// Re-reading an unchanged singleton must not append a version, or the history
// grows by one on every scheduled run forever.
func TestSingletonUnchangedDoesNotVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("company", `{"company":{"name":"Home Co"}}`)

	families := []string{"company"}
	for range 3 {
		if _, err := h.engine.Pull(t.Context(), Options{
			Mode: store.ModeFull, Families: families,
		}); err != nil {
			t.Fatal(err)
		}
	}

	versions, err := h.db.VersionCount(t.Context(), h.account.ID, h.apiURL+"/company")
	if err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Errorf("versions = %d after three identical reads, want 1", versions)
	}
}

func TestSingletonChangeIsVersioned(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	families := []string{"company"}

	h.fake.setDocument("company", `{"company":{"name":"Home Co"}}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: families,
	}); err != nil {
		t.Fatal(err)
	}

	h.fake.setDocument("company", `{"company":{"name":"Home Co Ltd"}}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: families,
	}); err != nil {
		t.Fatal(err)
	}

	versions, err := h.db.VersionCount(t.Context(), h.account.ID, h.apiURL+"/company")
	if err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Errorf("versions = %d after a rename, want 2", versions)
	}
}

// A singleton is one row every run rewrites, so sweeping it would delete the
// only copy on any run that read a different family.
func TestSingletonIsNeverSwept(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("company", `{"company":{"name":"Home Co"}}`)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	families := []string{"company", "bills"}
	for range 2 {
		result, err := h.engine.Pull(t.Context(), Options{
			Mode: store.ModeFull, Reconcile: true, Families: families,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range result.Families {
			if f.Family == "company" && f.Swept {
				t.Error("the company document was swept")
			}
		}
	}

	if got := h.liveCount("company"); got != 1 {
		t.Errorf("company records = %d after two reconciling runs, want 1", got)
	}
}

// email_addresses is a list with no per-item addressing, and cis_bands wraps
// its content under a key matching neither its singular nor its plural name.
// Both are archived whole for exactly that reason.
func TestListShapedSingletonsAreArchivedWhole(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("email_addresses",
		`{"email_addresses":["books@example.test","vat@example.test"]}`)
	h.fake.setDocument("cis_bands",
		`{"available_bands":[{"band":"cis_standard","rate":"20.0"}]}`)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"email_addresses", "cis_bands"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}

	body, err := h.db.RecordBody(t.Context(), h.account.ID, h.apiURL+"/cis_bands")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "available_bands") {
		t.Errorf("the unconventional envelope key was lost: %s", body)
	}
}

func TestReportIsSnapshotted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("trial_balance",
		`{"trial_balance_summary":[{"nominal_code":"750","total":"1440.00"}]}`)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"trial_balance"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}

	// A report is not a record: it never enters the records table.
	if got := h.liveCount("trial_balance"); got != 0 {
		t.Errorf("a report was archived as %d records, want 0", got)
	}

	snap, err := h.db.LatestReportSnapshot(t.Context(), h.account.ID, "trial_balance")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap.Body), "1440.00") {
		t.Errorf("snapshot body = %s", snap.Body)
	}
	if snap.FromDate == "" || snap.ToDate == "" {
		t.Errorf("snapshot window = %q to %q, want both recorded", snap.FromDate, snap.ToDate)
	}
}

// A report is asked for with the window it covers, or the numbers mean
// something different from what the caller assumes.
func TestReportSendsItsWindow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("profit_and_loss",
		`{"profit_and_loss_summary":{"income":"100.00"}}`)

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeAdHoc,
		Families: []string{"profit_and_loss"},
		Window:   store.RunWindow{From: from, To: to},
	}); err != nil {
		t.Fatal(err)
	}

	query := h.fake.queryFor("profit_and_loss")
	if got := query.Get("from_date"); got != "2026-01-01" {
		t.Errorf("from_date = %q, want 2026-01-01", got)
	}
	if got := query.Get("to_date"); got != "2026-03-31" {
		t.Errorf("to_date = %q, want 2026-03-31", got)
	}

	snap, err := h.db.LatestReportSnapshot(t.Context(), h.account.ID, "profit_and_loss")
	if err != nil {
		t.Fatal(err)
	}
	if snap.FromDate != "2026-01-01" || snap.ToDate != "2026-03-31" {
		t.Errorf("snapshot window = %q to %q, want the requested one",
			snap.FromDate, snap.ToDate)
	}
}

// A run with no window still has to ask for one, because a report with no
// dates answers for a different period than the caller expects.
func TestReportDefaultsToARollingYear(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("balance_sheet", `{"balance_sheet":{}}`)

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"balance_sheet"},
	}); err != nil {
		t.Fatal(err)
	}

	query := h.fake.queryFor("balance_sheet")
	from, to := query.Get("from_date"), query.Get("to_date")
	if from == "" || to == "" {
		t.Fatalf("window = %q to %q, want both sent", from, to)
	}

	parsedFrom, err := time.Parse(time.DateOnly, from)
	if err != nil {
		t.Fatal(err)
	}
	parsedTo, err := time.Parse(time.DateOnly, to)
	if err != nil {
		t.Fatal(err)
	}
	if span := parsedTo.Sub(parsedFrom); span < 360*24*time.Hour {
		t.Errorf("default window spans %s, want about a year", span)
	}
}

// Re-taking an unchanged report must not append a snapshot, or an hourly
// schedule fills the table with copies.
func TestUnchangedReportIsNotSnapshottedTwice(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("cashflow", `{"cashflow":{"opening_balance":"10.00"}}`)

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	opts := Options{
		Mode:     store.ModeAdHoc,
		Families: []string{"cashflow"},
		Window:   store.RunWindow{From: from, To: to},
	}

	for range 3 {
		if _, err := h.engine.Pull(t.Context(), opts); err != nil {
			t.Fatal(err)
		}
	}

	counts, err := h.db.ReportSnapshotCounts(t.Context(), h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["cashflow"] != 1 {
		t.Errorf("snapshots = %d after three identical reads, want 1", counts["cashflow"])
	}
}

// A changed report is a new answer, so it is appended rather than replacing
// what was there.
func TestChangedReportIsAppended(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	opts := Options{
		Mode:     store.ModeAdHoc,
		Families: []string{"cashflow"},
		Window:   store.RunWindow{From: from, To: to},
	}

	for _, balance := range []string{"10.00", "20.00"} {
		h.fake.setDocument("cashflow", `{"cashflow":{"opening_balance":"`+balance+`"}}`)
		if _, err := h.engine.Pull(t.Context(), opts); err != nil {
			t.Fatal(err)
		}
	}

	counts, err := h.db.ReportSnapshotCounts(t.Context(), h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts["cashflow"] != 2 {
		t.Errorf("snapshots = %d after a change, want 2", counts["cashflow"])
	}

	snap, err := h.db.LatestReportSnapshot(t.Context(), h.account.ID, "cashflow")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap.Body), "20.00") {
		t.Errorf("latest snapshot is not the newest: %s", snap.Body)
	}
}

func TestReportFailureDoesNotStopTheRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})
	h.fake.failWith("trial_balance", 500)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"bills", "trial_balance"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomePartial {
		t.Errorf("outcome = %s, want partial", result.Outcome)
	}
	if got := h.liveCount("bills"); got != 1 {
		t.Errorf("bills archived %d despite the report failure, want 1", got)
	}
}
