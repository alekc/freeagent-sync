package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// Payroll families are addressed by tax year, with no endpoint that lists
// which years exist.
const (
	payrollFamily        = "payroll"
	payrollProfileFamily = "payroll_profiles"
	companyFamily        = "company"
)

// DefaultPayrollYears is how far back to look when the company record does not
// say when the books start. Seven years is the UK record-retention window, so
// it covers what anyone is obliged to keep.
const DefaultPayrollYears = 7

// maxPayrollYears bounds the fan-out however old the company is. Each year
// costs a request plus one per period, and nobody needs a mirror to walk back
// through twenty years of payroll on every run.
const maxPayrollYears = 12

// taxYearEnd returns the year a UK tax year ends in, which is how FreeAgent
// addresses payroll: April 2025 to March 2026 is year 2026.
func taxYearEnd(t time.Time) int {
	sixthOfApril := time.Date(t.Year(), time.April, 6, 0, 0, 0, 0, t.Location())
	if t.Before(sixthOfApril) {
		return t.Year()
	}
	return t.Year() + 1
}

// payrollYears is the range to try, newest first.
//
// There is no endpoint that lists the years a company has payroll for, so the
// range is derived from when its books begin and each year is simply attempted.
// A year with no payroll is not an error: it is the normal answer for most of
// the range.
func (e *Engine) payrollYears(ctx context.Context, now time.Time) []int {
	latest := taxYearEnd(now)
	earliest := latest - DefaultPayrollYears + 1

	if first, ok := e.firstAccountingYear(ctx); ok && first < earliest {
		earliest = first
	}
	if latest-earliest+1 > maxPayrollYears {
		earliest = latest - maxPayrollYears + 1
	}

	years := make([]int, 0, latest-earliest+1)
	for year := latest; year >= earliest; year-- {
		years = append(years, year)
	}
	return years
}

// firstAccountingYear reads when the books begin from the archived company
// document, so the payroll range matches the company rather than a guess.
func (e *Engine) firstAccountingYear(ctx context.Context) (int, bool) {
	bodies, err := e.db.LiveRecordBodies(ctx, e.account.ID, companyFamily)
	if err != nil || len(bodies) == 0 {
		return 0, false
	}

	var envelope struct {
		Company struct {
			FirstAccountingYearEnd string `json:"first_accounting_year_end"`
		} `json:"company"`
	}
	if json.Unmarshal(bodies[0], &envelope) != nil {
		return 0, false
	}
	when, err := time.Parse(time.DateOnly, envelope.Company.FirstAccountingYearEnd)
	if err != nil {
		return 0, false
	}
	return when.Year(), true
}

// errNoPayrollForYear reports a year the company has no payroll for. Expected
// across most of the range, so it is recognised rather than reported.
var errNoPayrollForYear = errors.New("engine: no payroll for that year")

// pullPayrollYear archives one tax year of payroll: the year itself, and then
// each of its periods, because payslips only arrive on the per-period fetch.
func (e *Engine) pullPayrollYear(ctx context.Context, j job, budget *budget) FamilyResult {
	out := FamilyResult{Family: j.meta.Name, Scope: j.scope, Label: j.label, FullScan: true}
	tracker := e.report.Track(j.key(), 0, ui.UnitsCount)

	body, err := e.fetchYear(ctx, j.path)
	if errors.Is(err, errNoPayrollForYear) {
		out.completed = true
		tracker.Message("no payroll")
		tracker.Done()
		return out
	}
	if err != nil {
		out.Err = err
		tracker.Fail(err)
		return out
	}

	periods, err := periodURLs(body)
	if err != nil {
		out.Err = err
		tracker.Fail(err)
		return out
	}
	tracker.SetTotal(int64(len(periods)) + 1)

	stats, err := e.archiveDocument(ctx, j.meta.Name, e.documentURL(j.path), body)
	if err != nil {
		out.Err = err
		tracker.Fail(err)
		return out
	}
	out.Stats.Add(stats)
	out.Pages = 1
	tracker.Add(1)

	// Each period is fetched from its own URL, which is where the payslips
	// are: the year response lists periods without them.
	for _, periodURL := range periods {
		if reason := budget.exceeded(ctx); reason != nil {
			out.Err = reason
			tracker.Fail(reason)
			return out
		}
		periodStats, err := e.archivePeriod(ctx, j.meta.Name, periodURL)
		if err != nil {
			out.Err = err
			tracker.Fail(err)
			return out
		}
		out.Stats.Add(periodStats)
		out.Pages++
		tracker.Add(1)
	}

	out.completed = true
	tracker.Done()
	return out
}

// fetchYear reads a year-addressed endpoint, turning "this company has no
// payroll then" into a recognisable outcome rather than a failure.
func (e *Engine) fetchYear(ctx context.Context, path string) ([]byte, error) {
	body, _, err := e.client.Get(ctx, path, nil)
	if err != nil {
		if errors.Is(err, freeagent.ErrNotFound) {
			return nil, errNoPayrollForYear
		}
		return nil, err
	}
	if isEmptyPayroll(body) {
		return nil, errNoPayrollForYear
	}
	return body, nil
}

// isEmptyPayroll recognises a year that answered with no content. FreeAgent
// does not document what an unused year returns, so both an empty envelope and
// empty arrays are treated as nothing rather than archived as a document that
// says nothing.
func isEmptyPayroll(body []byte) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	if len(envelope) == 0 {
		return true
	}
	for _, raw := range envelope {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
			return false
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil && len(object) > 0 {
			return false
		}
	}
	return true
}

// periodURLs pulls the period links out of a payroll year.
func periodURLs(body []byte) ([]string, error) {
	var envelope struct {
		Periods []struct {
			URL string `json:"url"`
		} `json:"periods"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("engine: reading payroll periods: %w", err)
	}

	out := make([]string, 0, len(envelope.Periods))
	for _, period := range envelope.Periods {
		if period.URL != "" {
			out = append(out, period.URL)
		}
	}
	return out, nil
}

// archivePeriod stores one payroll period, which is the record that carries
// the payslips.
func (e *Engine) archivePeriod(
	ctx context.Context, family, periodURL string,
) (store.UpsertStats, error) {
	body, _, err := e.client.GetURL(ctx, freeagent.ResourceURL(periodURL), nil)
	if err != nil {
		return store.UpsertStats{}, err
	}
	return e.archiveDocument(ctx, family, periodURL, body)
}

// archiveDocument stores one response as a single record under a given URL.
func (e *Engine) archiveDocument(
	ctx context.Context, family, url string, body []byte,
) (store.UpsertStats, error) {
	rec, err := store.NewDocumentRecord(family, url, body)
	if err != nil {
		return store.UpsertStats{}, err
	}
	return e.db.UpsertRecords(ctx, e.account.ID, []store.Record{rec})
}

// payrollJobs expands a year-addressed family into one job per tax year.
func (e *Engine) payrollJobs(
	ctx context.Context, meta freeagent.ResourceMeta, now time.Time,
) []job {
	years := e.payrollYears(ctx, now)
	jobs := make([]job, 0, len(years))

	for _, year := range years {
		label := strconv.Itoa(year)
		jobKind := kindDocument
		if meta.Name == payrollFamily {
			jobKind = kindPayrollYear
		}
		jobs = append(jobs, job{
			kind:        jobKind,
			meta:        meta,
			scope:       label,
			label:       label,
			path:        meta.Path + "/" + label,
			tolerate404: true,
		})
	}
	return jobs
}
