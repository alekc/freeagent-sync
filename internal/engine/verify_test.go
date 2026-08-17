package engine

import (
	"strings"
	"testing"

	"github.com/alekc/freeagent-sync/internal/store"
)

func checkNamed(t *testing.T, result VerifyResult, name string) Check {
	t.Helper()
	for _, c := range result.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, result.Checks)
	return Check{}
}

// trialBalance returns a snapshot body that balances, which is what
// double-entry guarantees and therefore what the archive should see.
func trialBalance(entries ...string) string {
	return `{"trial_balance_summary":[` + strings.Join(entries, ",") + `]}`
}

func tbEntry(code, name, total string) string {
	return `{"nominal_code":"` + code + `","name":"` + name + `","total":"` + total + `"}`
}

func txn(id int, code, debit, date string) string {
	return `{"url":"https://api.test/v2/transactions/` + itoa(id) +
		`","nominal_code":"` + code + `","debit_value":"` + debit +
		`","dated_on":"` + date + `"}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

func TestVerifyTrialBalanceSumsToZero(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("trial_balance", trialBalance(
		tbEntry("750", "Software", "1440.00"),
		tbEntry("800", "Bank", "-1440.00"),
	))
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"trial_balance"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "trial balance sums to zero")
	if got.Status != CheckPass {
		t.Errorf("status = %s (%s), want pass", got.Status, got.Summary)
	}
}

// A trial balance that does not balance means either FreeAgent's books are
// broken or the snapshot decoded wrongly. Either is worth failing over.
func TestVerifyCatchesAnUnbalancedTrialBalance(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("trial_balance", trialBalance(
		tbEntry("750", "Software", "1440.00"),
		tbEntry("800", "Bank", "-1000.00"),
	))
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"trial_balance"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Failed() {
		t.Error("an unbalanced trial balance did not fail verification")
	}

	got := checkNamed(t, result, "trial balance sums to zero")
	if got.Status != CheckFail || !strings.Contains(got.Summary, "440") {
		t.Errorf("check = %+v, want a failure naming the difference", got)
	}
}

// This is the strongest check available and it needs no network: a reference
// to a family this tool archives that is not in the archive is a gap.
func TestVerifyCatchesADanglingReference(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A bill that points at a contact nothing ever archived.
	h.fake.setRaw("bills", `{"url":"`+h.apiURL+`/v2/bills/1",`+
		`"contact":"`+h.apiURL+`/v2/contacts/404"}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"bills"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "cross-references resolve")
	if got.Status != CheckFail {
		t.Fatalf("status = %s (%s), want fail", got.Status, got.Summary)
	}
	if !strings.Contains(strings.Join(got.Detail, " "), "contacts") {
		t.Errorf("detail = %v, want it to name the missing family", got.Detail)
	}
	if !result.Failed() {
		t.Error("a dangling reference did not fail verification")
	}
}

func TestVerifyPassesWhenReferencesResolve(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.fake.setRaw("contacts", `{"url":"`+h.apiURL+`/v2/contacts/7",`+
		`"organisation_name":"Acme Ltd"}`)
	h.fake.setRaw("bills", `{"url":"`+h.apiURL+`/v2/bills/1",`+
		`"contact":"`+h.apiURL+`/v2/contacts/7"}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"contacts", "bills"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "cross-references resolve")
	if got.Status != CheckPass {
		t.Errorf("status = %s (%s), want pass", got.Status, got.Summary)
	}
}

// A reference into a family the SDK does not model at all is expected, not a
// gap, so it is counted rather than failed. Currencies is a real example: the
// API has it and the registry does not.
func TestVerifyToleratesReferencesIntoUnarchivedFamilies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.fake.setRaw("bills", `{"url":"`+h.apiURL+`/v2/bills/1",`+
		`"currency_details":"`+h.apiURL+`/v2/currencies/GBP"}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"bills"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "cross-references resolve")
	if got.Status != CheckPass {
		t.Errorf("status = %s (%s), want pass", got.Status, got.Summary)
	}
	if !strings.Contains(got.Summary, "does not archive") {
		t.Errorf("summary = %q, want it to account for the unarchived family", got.Summary)
	}
}

// A nominal code with a balance and no archived transactions means a whole
// category of entries never arrived.
func TestVerifyCatchesAnUncoveredNominalCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("trial_balance", trialBalance(
		tbEntry("750", "Software", "100.00"),
		tbEntry("800", "Bank", "-100.00"),
	))
	// Only one of the two codes has any transaction archived.
	h.fake.setRaw("transactions", txn(1, "750", "100.00", "2026-03-14"))

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"transactions", "trial_balance"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "nominal codes covered")
	if got.Status != CheckFail {
		t.Fatalf("status = %s (%s), want fail", got.Status, got.Summary)
	}
	if !strings.Contains(strings.Join(got.Detail, " "), "800") {
		t.Errorf("detail = %v, want it to name code 800", got.Detail)
	}
}

func TestVerifyPassesFullCoverage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("trial_balance", trialBalance(
		tbEntry("750", "Software", "100.00"),
		tbEntry("800", "Bank", "-100.00"),
	))
	h.fake.setRaw("transactions",
		txn(1, "750", "100.00", "2026-03-14"),
		txn(2, "800", "-100.00", "2026-03-14"))

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"transactions", "trial_balance"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed() {
		for _, c := range result.Checks {
			if c.Status == CheckFail {
				t.Errorf("check %q failed: %s %v", c.Name, c.Summary, c.Detail)
			}
		}
	}

	coverage := checkNamed(t, result, "nominal codes covered")
	if coverage.Status != CheckPass {
		t.Errorf("coverage = %s (%s), want pass", coverage.Status, coverage.Summary)
	}
	totals := checkNamed(t, result, "totals match the trial balance")
	if totals.Status != CheckPass {
		t.Errorf("totals = %s (%s), want pass", totals.Status, totals.Summary)
	}
}

// A total that differs is reported, but as advisory: a trial balance runs from
// the accounting period start, so an opening balance can explain it. Failing
// on that would cry wolf on every real company.
func TestVerifyReportsTotalDifferencesAsAdvisory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("trial_balance", trialBalance(
		tbEntry("750", "Software", "500.00"),
		tbEntry("800", "Bank", "-500.00"),
	))
	// Both codes are covered, but the amounts do not add up to the report.
	h.fake.setRaw("transactions",
		txn(1, "750", "100.00", "2026-03-14"),
		txn(2, "800", "-100.00", "2026-03-14"))

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"transactions", "trial_balance"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "totals match the trial balance")
	if got.Status != CheckAdvisory {
		t.Fatalf("status = %s (%s), want advisory", got.Status, got.Summary)
	}
	if !strings.Contains(strings.Join(got.Detail, " "), "400") {
		t.Errorf("detail = %v, want it to show the difference", got.Detail)
	}
	// Advisory must not fail the command, or every real archive fails.
	if result.Failed() {
		t.Error("an advisory difference failed verification")
	}
}

// Money is compared exactly. A float round trip would make 0.1 + 0.2 differ
// from 0.3 and report a difference that is not there.
func TestVerifyComparesMoneyExactly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("trial_balance", trialBalance(
		tbEntry("750", "Software", "0.30"),
		tbEntry("800", "Bank", "-0.30"),
	))
	h.fake.setRaw("transactions",
		txn(1, "750", "0.10", "2026-03-14"),
		txn(2, "750", "0.20", "2026-03-14"),
		txn(3, "800", "-0.30", "2026-03-14"))

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"transactions", "trial_balance"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "totals match the trial balance")
	if got.Status != CheckPass {
		t.Errorf("status = %s (%s), want pass; 0.10 + 0.20 must equal 0.30 exactly",
			got.Status, got.Summary)
	}
}

// Reports send money as a bare number in some places and a quoted string in
// others, and both have to be read.
func TestVerifyReadsBareNumberTotals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("trial_balance",
		`{"trial_balance_summary":[`+
			`{"nominal_code":"750","name":"Software","total":100.5},`+
			`{"nominal_code":"800","name":"Bank","total":-100.5}]}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"trial_balance"},
	}); err != nil {
		t.Fatal(err)
	}

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "trial balance sums to zero")
	if got.Status != CheckPass {
		t.Errorf("status = %s (%s), want pass", got.Status, got.Summary)
	}
}

func TestVerifySkipsWhatItCannotCheck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	result, err := h.engine.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed() {
		t.Error("an empty archive failed verification rather than skipping checks")
	}

	got := checkNamed(t, result, "trial balance sums to zero")
	if got.Status != CheckSkipped {
		t.Errorf("status = %s, want skipped with no snapshot", got.Status)
	}
	if !strings.Contains(got.Summary, "fasync pull") {
		t.Errorf("summary = %q, want it to say how to get a snapshot", got.Summary)
	}
}

func TestVerifyChecksBlobsWhenGivenAStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	blobs := newBlobStore(t)

	result, err := h.engine.Verify(t.Context(), VerifyOptions{Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	got := checkNamed(t, result, "attachment bytes intact")
	if got.Status != CheckSkipped {
		t.Errorf("status = %s, want skipped with no attachments", got.Status)
	}
}

// Verification is entirely local, so it has to work on an engine built with no
// API client. Every other test has one, which is how a nil dereference here
// survived until the command was run by hand.
func TestVerifyWorksWithNoAPIClient(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	h.fake.setRaw("contacts", `{"url":"`+h.apiURL+`/v2/contacts/7","organisation_name":"Acme"}`)
	h.fake.setRaw("bills", `{"url":"`+h.apiURL+`/v2/bills/1",`+
		`"contact":"`+h.apiURL+`/v2/contacts/404"}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"contacts", "bills"},
	}); err != nil {
		t.Fatal(err)
	}

	offline := NewOffline(h.db, discardReporter(t), h.account)
	result, err := offline.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// And it still finds the gap: the base host comes from the archived URLs,
	// not from a client it does not have.
	got := checkNamed(t, result, "cross-references resolve")
	if got.Status != CheckFail {
		t.Errorf("status = %s (%s), want fail", got.Status, got.Summary)
	}
}

// An archive with nothing in it cannot have its references checked, and saying
// so is the only honest answer. Reporting a pass would be worse than useless.
func TestVerifySkipsReferencesOnAnEmptyArchive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	offline := NewOffline(h.db, discardReporter(t), h.account)
	result, err := offline.Verify(t.Context(), VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}

	got := checkNamed(t, result, "cross-references resolve")
	if got.Status != CheckSkipped {
		t.Errorf("status = %s (%s), want skipped", got.Status, got.Summary)
	}
	for _, c := range result.Checks {
		if c.Status == CheckPass {
			t.Errorf("check %q passed on an empty archive: %s", c.Name, c.Summary)
		}
	}
}
