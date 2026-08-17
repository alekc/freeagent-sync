package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// DefaultReportWindow is how far back a report is taken when the run gives no
// window. A rolling year covers the current accounting period for most
// companies without asking the caller to know their own year end.
const DefaultReportWindow = 365 * 24 * time.Hour

// pullDocument archives a singleton endpoint as one record.
//
// The whole envelope is stored rather than the object inside it. Company,
// email addresses and CIS bands each wrap their content differently, and one
// of them wraps it under a key that matches neither the singular nor the
// plural name, so unwrapping here would be three special cases that could
// each lose data on a shape change.
func (e *Engine) pullDocument(ctx context.Context, j job, runID int64) FamilyResult {
	out := FamilyResult{Family: j.meta.Name, Label: j.label, FullScan: true}
	tracker := e.report.Track(j.key(), 1, ui.UnitsCount)

	path := j.requestPath()
	body, _, err := e.client.Get(ctx, path, &freeagent.ListOptions{Extra: j.extra})
	switch {
	case err != nil && j.tolerate404 && errors.Is(err, freeagent.ErrNotFound):
		// A tax year the company had no data for is the normal answer across
		// most of the range, not a failure.
		out.completed = true
		tracker.Message("not present")
		tracker.Done()
		return out
	case err != nil:
		out.Err = err
		tracker.Fail(err)
		return out
	}
	if j.tolerate404 && isEmptyPayroll(body) {
		out.completed = true
		tracker.Message("empty")
		tracker.Done()
		return out
	}

	stats, err := e.archiveDocument(ctx, j.meta.Name, e.documentURL(path), body)
	if err != nil {
		out.Err = err
		tracker.Fail(err)
		return out
	}

	out.Stats, out.Pages, out.completed = stats, 1, true
	tracker.Add(1)
	tracker.Done()
	_ = runID
	return out
}

// pullReport snapshots a derived report for a window.
//
// Reports are point in time, so they are appended rather than upserted: last
// quarter's profit and loss is a different answer to a different question, not
// a stale version of this quarter's.
func (e *Engine) pullReport(ctx context.Context, j job, opts Options) FamilyResult {
	out := FamilyResult{Family: j.meta.Name, Label: j.label, FullScan: true}
	tracker := e.report.Track(j.key(), 1, ui.UnitsCount)

	from, to := reportWindow(opts)
	list := &freeagent.ListOptions{
		FromDate: freeagent.DateOf(from),
		ToDate:   freeagent.DateOf(to),
		Extra:    j.extra,
	}

	body, _, err := e.client.Get(ctx, j.requestPath(), list)
	if err != nil {
		out.Err = err
		tracker.Fail(err)
		return out
	}

	changed, err := e.db.SaveReportSnapshot(ctx, e.account.ID, j.meta.Name,
		store.FormatDate(from), store.FormatDate(to), body)
	if err != nil {
		out.Err = err
		tracker.Fail(err)
		return out
	}

	// An unchanged report is not a new snapshot, so the run reports it as
	// seen rather than as newly archived.
	if changed {
		out.Stats.Inserted = 1
	} else {
		out.Stats.Unchanged = 1
	}
	out.Pages, out.completed = 1, true
	tracker.Add(1)
	tracker.Done()
	return out
}

// reportWindow decides the range a report covers: the run's explicit window
// when it has one, a rolling year otherwise.
func reportWindow(opts Options) (from, to time.Time) {
	to = opts.Window.To
	if to.IsZero() {
		to = time.Now()
	}
	from = opts.Window.From
	if from.IsZero() {
		from = to.Add(-DefaultReportWindow)
	}
	return from, to
}

// documentURL is the identity of a singleton: the endpoint that produced it.
func (e *Engine) documentURL(path string) string {
	base := strings.TrimSuffix(e.client.Environment().BaseURL, "/")
	return fmt.Sprintf("%s/%s", base, strings.TrimPrefix(path, "/"))
}
