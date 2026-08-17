package engine

import (
	"net/http"
	"strings"
	"testing"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/store"
)

// Every case here comes from a probe against a real company, where each of
// these appeared as a failure that was either a bug in the planner or a fact
// about the company being reported as something to fix.

// notes answers 400 without a contact or project, so it has to be read through
// one. Read as a plain collection it fails on every run.
func TestNotesAreReadThroughTheirParents(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("contacts", `{"url":"`+h.apiURL+`/v2/contacts/7","organisation_name":"Acme"}`)
	h.fake.setRaw("projects", `{"url":"`+h.apiURL+`/v2/projects/3","name":"Rebuild"}`)
	h.fake.requireParam("notes", "contact", "project")
	h.fake.setScopedParam("notes", "contact", h.apiURL+"/v2/contacts/7",
		`{"url":"`+h.apiURL+`/v2/notes/1","note":"called about the invoice"}`)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeFull,
		Families: []string{"contacts", "projects", "notes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		for _, f := range result.Failed() {
			t.Errorf("%s failed: %v", f.Name(), f.Err)
		}
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}
	if got := h.liveCount("notes"); got != 1 {
		t.Errorf("archived %d notes, want 1", got)
	}

	// One job per parent, so the cost is visible rather than hidden.
	var jobs int
	for _, f := range result.Families {
		if f.Family == "notes" {
			jobs++
		}
	}
	if jobs != 2 {
		t.Errorf("notes ran as %d jobs, want one per contact and project", jobs)
	}
}

// Only /v2/users/:id/self_assessment_returns exists. The registry Path is the
// suffix, so reading it directly is a guaranteed 404.
func TestIncomeTaxReturnsAreReadUnderTheUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("users", `{"url":"`+h.apiURL+`/v2/users/2","first_name":"Ada","last_name":"L"}`)
	h.fake.setRaw("users/2/self_assessment_returns",
		`{"url":"`+h.apiURL+`/v2/users/2/self_assessment_returns/2026-04-05",
		  "period_ends_on":"2026-04-05"}`)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeFull,
		Families: []string{"users", "income_tax_returns"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		for _, f := range result.Failed() {
			t.Errorf("%s failed: %v", f.Name(), f.Err)
		}
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}
	if got := h.liveCount("income_tax_returns"); got != 1 {
		t.Errorf("archived %d returns, want 1", got)
	}

	// The bare path must never be requested: it does not exist.
	if h.fake.sawPath("self_assessment_returns") {
		t.Error("the bare self_assessment_returns path was requested")
	}
}

// A company without a feature answers 403 or 404. Treating that as a failure
// makes every run of an ordinary company report partial forever.
func TestUnavailableFamilyIsNotAFailure(t *testing.T) {
	t.Parallel()
	tests := map[string]int{
		"403 forbidden": http.StatusForbidden,
		"404 not found": http.StatusNotFound,
	}
	for name, status := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.fake.setRaw("users", `{"url":"`+h.apiURL+`/v2/users/2"}`)
			h.fake.failWith("users/2/self_assessment_returns", status)

			result, err := h.engine.Pull(t.Context(), Options{
				Mode:     store.ModeFull,
				Families: []string{"users", "income_tax_returns"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != store.OutcomeOK {
				t.Errorf("outcome = %s, want ok", result.Outcome)
			}
			if len(result.Failed()) != 0 {
				t.Errorf("%d families reported as failures", len(result.Failed()))
			}
			// Reported, not hidden: a family the company lacks is worth
			// knowing about, just not worth fixing.
			if len(result.Unavailable()) != 1 {
				t.Errorf("unavailable = %d, want 1", len(result.Unavailable()))
			}
		})
	}
}

// An unavailable family has nothing to sweep, and sweeping it would delete
// whatever an earlier run managed to archive.
func TestUnavailableFamilyIsNotSwept(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("users", `{"url":"`+h.apiURL+`/v2/users/2"}`)
	h.fake.setRaw("users/2/self_assessment_returns",
		`{"url":"`+h.apiURL+`/v2/users/2/self_assessment_returns/2026-04-05"}`)

	families := []string{"users", "income_tax_returns"}
	opts := Options{Mode: store.ModeFull, Reconcile: true, Families: families}
	if _, err := h.engine.Pull(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	if got := h.liveCount("income_tax_returns"); got != 1 {
		t.Fatalf("archived %d returns, want 1", got)
	}

	// Access is revoked, so the family now answers 403.
	h.fake.failWith("users/2/self_assessment_returns", http.StatusForbidden)
	result, err := h.engine.Pull(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range result.Families {
		if f.Family == "income_tax_returns" && f.Swept {
			t.Error("an unavailable family was swept")
		}
	}
	if got := h.liveCount("income_tax_returns"); got != 1 {
		t.Errorf("live count = %d, want the earlier record kept", got)
	}
}

// updated_since means nothing for a singleton or a report. Probing them
// produced six failures that said nothing about the API.
func TestProbeSkipsFamiliesWithNothingToFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	families := []string{"company", "trial_balance", "balance_sheet",
		"profit_and_loss", "cashflow", "cis_bands", "payroll"}
	results, err := h.engine.Probe(t.Context(), families)
	if err != nil {
		t.Fatal(err)
	}

	for _, got := range results {
		if got.Result != ProbeNotApplicable {
			t.Errorf("%s = %s (%s), want not applicable",
				got.Family, got.Result, got.Detail)
		}
		if got.Err != nil {
			t.Errorf("%s reported an error: %v", got.Family, got.Err)
		}
	}
	// And no request was made: there was nothing worth asking.
	if h.fake.requestCount() != 0 {
		t.Errorf("the probe made %d requests for families it cannot probe",
			h.fake.requestCount())
	}
}

// A bank-scoped probe without the filter answers 404, which looked like a
// broken endpoint rather than a missing parameter.
func TestProbeScopesBankFamilies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedBankAccounts(h)
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/1",
		fakeRecord{ID: 10, UpdatedAt: march})

	results, err := h.engine.Probe(t.Context(), []string{"bank_transactions"})
	if err != nil {
		t.Fatal(err)
	}
	got := probeResultFor(t, results, "bank_transactions")
	if got.Result != ProbeHonoured {
		t.Errorf("result = %s (%s), want honoured", got.Result, got.Detail)
	}
}

// A probe on a family the company does not have is a fact, not a failure.
func TestProbeReportsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.failWith("capital_asset_types", http.StatusForbidden)

	results, err := h.engine.Probe(t.Context(), []string{"capital_asset_types"})
	if err != nil {
		t.Fatal(err)
	}
	got := probeResultFor(t, results, "capital_asset_types")
	if got.Result != ProbeUnavailable {
		t.Fatalf("result = %s (%s), want unavailable", got.Result, got.Detail)
	}
	if got.Err != nil {
		t.Errorf("unavailable reported an error: %v", got.Err)
	}
	if !strings.Contains(got.Detail, "does not have") {
		t.Errorf("detail = %q, want it to say the company lacks the family", got.Detail)
	}
}

// A scoped family with no scopes cannot be probed, and saying so beats
// asserting a capability from no evidence.
func TestProbeWithoutScopesConcludesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bank_accounts")

	results, err := h.engine.Probe(t.Context(), []string{"bank_transactions"})
	if err != nil {
		t.Fatal(err)
	}
	got := probeResultFor(t, results, "bank_transactions")
	if got.Result != ProbeNoData {
		t.Errorf("result = %s (%s), want no-data", got.Result, got.Detail)
	}

	state, err := h.db.FamilyState(t.Context(), h.account.ID, "bank_transactions", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.SupportsUpdatedSince != nil {
		t.Error("an unprobeable family recorded a capability")
	}
}

// The classes have to match what the API actually requires, since every one of
// these was a live failure.
func TestClassificationMatchesTheAPI(t *testing.T) {
	t.Parallel()
	tests := map[string]Class{
		"notes":              ClassParentScoped,
		"income_tax_returns": ClassUserScoped,
		"bank_transactions":  ClassBankScoped,
		"payroll":            ClassYearScoped,
		"company":            ClassSingleton,
		"trial_balance":      ClassReport,
		"invoices":           ClassCollection,
		"categories":         ClassGrouped,
		"attachments":        ClassChildOnly,
	}
	for family, want := range tests {
		meta, ok := freeagent.Resources[family]
		if !ok {
			t.Errorf("the SDK has no %s entry", family)
			continue
		}
		if got := Classify(meta); got != want {
			t.Errorf("Classify(%s) = %s, want %s", family, got, want)
		}
	}
}

func TestProbeableExcludesTheUnfilterable(t *testing.T) {
	t.Parallel()
	probeable := map[string]bool{
		"invoices": true, "notes": true, "bank_transactions": true,
		"income_tax_returns": true,
		"company":            false, "trial_balance": false, "payroll": false,
		"attachments": false,
	}
	for family, want := range probeable {
		meta, ok := freeagent.Resources[family]
		if !ok {
			t.Fatalf("the SDK has no %s entry", family)
		}
		if got := Probeable(meta); got != want {
			t.Errorf("Probeable(%s) = %v, want %v (%s)",
				family, got, want, Classify(meta))
		}
	}
}
