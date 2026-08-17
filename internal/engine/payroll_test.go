package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alekc/freeagent-sync/internal/store"
)

// FreeAgent addresses a payroll year by the year it ends in: April 2025 to
// March 2026 is 2026. Getting this wrong reads the wrong year entirely.
func TestTaxYearEnd(t *testing.T) {
	t.Parallel()
	tests := map[string]int{
		// The tax year turns on 6 April.
		"2026-04-05": 2026,
		"2026-04-06": 2027,
		"2026-01-01": 2026,
		"2026-12-31": 2027,
		"2025-08-17": 2026,
	}
	for date, want := range tests {
		when, err := time.Parse(time.DateOnly, date)
		if err != nil {
			t.Fatal(err)
		}
		if got := taxYearEnd(when); got != want {
			t.Errorf("taxYearEnd(%s) = %d, want %d", date, got, want)
		}
	}
}

func TestPayrollYearsDefaultRange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	years := h.engine.payrollYears(t.Context(), now)

	if len(years) != DefaultPayrollYears {
		t.Fatalf("got %d years, want %d: %v", len(years), DefaultPayrollYears, years)
	}
	// Newest first, so the years anyone actually cares about are read before a
	// budget can run out.
	if years[0] != 2027 {
		t.Errorf("first year = %d, want the current tax year end 2027", years[0])
	}
	if years[len(years)-1] != 2027-DefaultPayrollYears+1 {
		t.Errorf("last year = %d, want %d", years[len(years)-1], 2027-DefaultPayrollYears+1)
	}
}

// A company older than the default window should have its whole history read,
// which is what the archived company record is for.
func TestPayrollYearsExtendToTheCompanyStart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("company",
		`{"company":{"first_accounting_year_end":"2018-03-31"}}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"company"},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	years := h.engine.payrollYears(t.Context(), now)

	if years[len(years)-1] != 2018 {
		t.Errorf("earliest year = %d, want 2018 from the company record", years[len(years)-1])
	}
	if len(years) > maxPayrollYears {
		t.Errorf("got %d years, want no more than %d", len(years), maxPayrollYears)
	}
}

// However old the company, the fan-out is bounded: each year costs a request
// plus one per period.
func TestPayrollYearsAreBounded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setDocument("company",
		`{"company":{"first_accounting_year_end":"1999-03-31"}}`)
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"company"},
	}); err != nil {
		t.Fatal(err)
	}

	years := h.engine.payrollYears(t.Context(), time.Now())
	if len(years) != maxPayrollYears {
		t.Errorf("got %d years, want the cap of %d", len(years), maxPayrollYears)
	}
}

// payrollYear serves a year with two periods, each with its own URL, which is
// how the real API presents them.
func (f *fakeAPI) setPayrollYear(api string, year int, periods ...int) {
	var links []string
	for _, period := range periods {
		links = append(links, fmt.Sprintf(
			`{"url":"%s/v2/payroll/%d/%d","period":%d,"frequency":"Monthly"}`,
			api, year, period, period))
	}
	f.setDocument(fmt.Sprintf("payroll/%d", year),
		fmt.Sprintf(`{"periods":[%s],"payments":[]}`, strings.Join(links, ",")))

	for _, period := range periods {
		f.setDocument(fmt.Sprintf("payroll/%d/%d", year, period), fmt.Sprintf(
			`{"period":{"url":"%s/v2/payroll/%d/%d","payslips":[{"gross_pay":"2500.00"}]}}`,
			api, year, period))
	}
}

// Payslips arrive only on the per-period fetch, so archiving the year alone
// would mirror the shape of payroll without any of its content.
func TestPayrollArchivesYearsAndPeriods(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setPayrollYear(h.apiURL, taxYearEnd(time.Now()), 1, 2)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"payroll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}

	// One year document plus one per period.
	if got := h.liveCount("payroll"); got != 3 {
		t.Errorf("archived %d payroll records, want 3", got)
	}

	year := taxYearEnd(time.Now())
	body, err := h.db.RecordBody(t.Context(), h.account.ID,
		fmt.Sprintf("%s/v2/payroll/%d/1", h.apiURL, year))
	if err != nil {
		t.Fatalf("the period was not archived under its own URL: %v", err)
	}
	if !strings.Contains(string(body), "payslips") {
		t.Errorf("the period body has no payslips: %s", body)
	}
}

// Most of the year range has no payroll for most companies. That is the normal
// answer, not a failure, or every run of a company without payroll reports
// seven failures.
func TestPayrollTreatsAMissingYearAsNormal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// Nothing configured: every year 404s.

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"payroll"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok for a company with no payroll", result.Outcome)
	}
	if len(result.Failed()) != 0 {
		t.Errorf("%d payroll years reported as failures", len(result.Failed()))
	}
	if got := h.liveCount("payroll"); got != 0 {
		t.Errorf("archived %d records for a company with no payroll, want 0", got)
	}
}

// An empty envelope is not worth archiving as a document that says nothing.
func TestPayrollSkipsAnEmptyYear(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	year := taxYearEnd(time.Now())
	h.fake.setDocument(fmt.Sprintf("payroll/%d", year), `{"periods":[],"payments":[]}`)

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"payroll"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.liveCount("payroll"); got != 0 {
		t.Errorf("archived %d records for an empty year, want 0", got)
	}
}

func TestPayrollProfilesArchivedPerYear(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	year := taxYearEnd(time.Now())
	h.fake.setDocument(fmt.Sprintf("payroll_profiles/%d", year),
		`{"profiles":[{"url":"https://api.test/v2/payroll_profiles/1","tax_code":"1257L"}]}`)

	result, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: []string{"payroll_profiles"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}
	if got := h.liveCount("payroll_profiles"); got != 1 {
		t.Errorf("archived %d profile documents, want 1", got)
	}

	body, err := h.db.RecordBody(t.Context(), h.account.ID,
		fmt.Sprintf("%s/payroll_profiles/%d", h.apiURL, year))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "1257L") {
		t.Errorf("profile body = %s", body)
	}
}

// A year-addressed family is one document per year that every run rewrites, so
// sweeping it would delete the years this run happened not to reach.
func TestPayrollIsNeverSwept(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setPayrollYear(h.apiURL, taxYearEnd(time.Now()), 1)

	for range 2 {
		result, err := h.engine.Pull(t.Context(), Options{
			Mode: store.ModeFull, Reconcile: true, Families: []string{"payroll"},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range result.Families {
			if f.Family == "payroll" && f.Swept {
				t.Error("a year-scoped family was swept")
			}
		}
	}

	if got := h.liveCount("payroll"); got != 2 {
		t.Errorf("payroll records = %d after two reconciling runs, want 2", got)
	}
}

// Periods are fetched one at a time, so the budget has to be able to stop
// between them rather than only between years.
func TestPayrollBudgetStopsBetweenPeriods(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setPayrollYear(h.apiURL, taxYearEnd(time.Now()), 1, 2, 3, 4, 5, 6)

	// Sequential, so the count is exact rather than bounded by however many
	// requests happened to be in flight when the budget tripped.
	result, err := h.engine.Pull(t.Context(), Options{
		Mode:        store.ModeFull,
		Families:    []string{"payroll"},
		MaxRequests: 3,
		Concurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeBudget {
		t.Errorf("outcome = %s, want the budget outcome", result.Outcome)
	}
	if got := h.fake.requestCount(); got != 3 {
		t.Errorf("made %d requests against a budget of 3", got)
	}
}
