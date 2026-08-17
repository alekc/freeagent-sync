package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/api"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// DefaultOverlap is how far back an incremental run reaches beyond its stored
// cursor. It absorbs clock skew and any lag between a record's updated_at and
// its visibility. Upserts are idempotent, so overlap costs requests, nothing
// else.
const DefaultOverlap = time.Hour

// DefaultConcurrency is how many jobs are read at once. The rate limiter is
// the binding constraint, so this only hides latency; more workers would just
// queue against the same budget.
const DefaultConcurrency = 4

// DefaultReconcileInterval is how stale a family's last full sweep may get
// before --reconcile-if-due picks it up. FreeAgent has no deletions feed, so
// this cadence is the only thing that ever notices a removal.
const DefaultReconcileInterval = 7 * 24 * time.Hour

// Engine archives one account.
type Engine struct {
	db      *store.DB
	client  *api.Client
	report  ui.Reporter
	account store.Account
}

// New builds an engine. The client must be read-only, which is the only kind
// the api package can produce.
func New(db *store.DB, client *api.Client, report ui.Reporter, account store.Account) *Engine {
	return &Engine{db: db, client: client, report: report, account: account}
}

// NewOffline builds an engine with no API client, for the work that reads only
// the archive. Only Verify is safe to call on one: everything else needs a
// client, and the absence is deliberate so a local command cannot quietly
// start making requests.
func NewOffline(db *store.DB, report ui.Reporter, account store.Account) *Engine {
	return &Engine{db: db, report: report, account: account}
}

// Options configures one run.
type Options struct {
	// Mode is one of the store.Mode constants. It decides whether the stored
	// cursor is read, advanced, or left entirely alone.
	Mode string
	// Families limits the run. Empty means every archivable family.
	Families []string
	// Window is the caller's explicit time bounds, recorded on the run.
	Window store.RunWindow
	// Overlap is how far back beyond the cursor an incremental run reaches.
	Overlap time.Duration
	// Concurrency is how many jobs are read at once.
	Concurrency int
	// MaxRequests stops the run once this many API calls have been made.
	// Zero means no limit.
	MaxRequests int64
	// Deadline stops the run at a wall-clock time. Zero means no limit.
	Deadline time.Time
	// Reconcile sweeps each family for records the far end no longer has.
	Reconcile bool
	// ReconcileIfDue sweeps only families whose last sweep is older than
	// ReconcileInterval, so one scheduled command covers both cadences.
	ReconcileIfDue bool
	// ReconcileInterval overrides DefaultReconcileInterval.
	ReconcileInterval time.Duration
}

func (o *Options) applyDefaults() {
	if o.Overlap == 0 {
		o.Overlap = DefaultOverlap
	}
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.ReconcileInterval == 0 {
		o.ReconcileInterval = DefaultReconcileInterval
	}
	if o.Mode == "" {
		o.Mode = store.ModeIncremental
	}
}

// FamilyResult is what happened to one job: a family, or one scope of one.
type FamilyResult struct {
	Family        string
	Scope         string
	Label         string
	Pages         int
	Stats         store.UpsertStats
	Deleted       int64
	Cursor        time.Time
	CursorAdvance bool
	Swept         bool
	FullScan      bool
	// Unavailable marks a family this company does not have: the API answered
	// 403 or 404. A fact about the company, not a failure.
	Unavailable bool
	Err         error

	// sweepStart is when this job began reading, which bounds what a sweep
	// may consider missing.
	sweepStart time.Time
	completed  bool
}

// Name is how this result is displayed.
func (f FamilyResult) Name() string {
	if f.Scope == "" {
		return f.Family
	}
	return f.Family + " [" + f.Label + "]"
}

// Result is what happened to the run.
type Result struct {
	RunID    int64
	Mode     string
	Families []FamilyResult
	Stats    store.UpsertStats
	Deleted  int64
	Requests int64
	Outcome  string
	Deferred map[string]Class
}

// Failed lists the jobs that errored.
func (r Result) Failed() []FamilyResult {
	var out []FamilyResult
	for _, f := range r.Families {
		if f.Err != nil {
			out = append(out, f)
		}
	}
	return out
}

// ErrBudgetExhausted ends a run that hit its request or time limit. It is not
// a failure: the archive is consistent, there is simply more to do.
var ErrBudgetExhausted = errors.New("engine: run budget exhausted")

// Pull archives the selected families. A job that fails does not stop the
// others; the run reports partial and names what broke, because one flaky
// endpoint should not cost a night's sync of everything else.
func (e *Engine) Pull(ctx context.Context, opts Options) (Result, error) {
	opts.applyDefaults()

	families, err := SelectFamilies(opts.Families)
	if err != nil {
		return Result{}, err
	}

	runID, err := e.db.StartRun(ctx, e.account.ID, opts.Mode, opts.Window)
	if err != nil {
		return Result{}, err
	}
	result := Result{RunID: runID, Mode: opts.Mode, Deferred: Deferred()}

	jobs, err := e.plan(ctx, families, time.Now())
	if err != nil {
		return result, err
	}

	budget := newBudget(opts.MaxRequests, opts.Deadline, e.client)
	results := e.runJobs(ctx, jobs, opts, runID, budget)
	e.sweepFamilies(ctx, results, opts, runID)

	result.Families = results
	for _, f := range results {
		result.Stats.Add(f.Stats)
		result.Deleted += f.Deleted
	}
	result.Requests = e.client.Requests()
	result.Outcome = outcomeFor(ctx, results, budget)

	summary := store.RunSummary{
		Families:        namesOf(families),
		Requests:        result.Requests,
		RecordsUpserted: int64(result.Stats.Total()),
		RecordsDeleted:  result.Deleted,
		Outcome:         result.Outcome,
		Err:             firstError(results),
	}
	if err := e.db.FinishRun(ctx, runID, summary); err != nil {
		return result, err
	}
	return result, nil
}

// runJobs reads jobs concurrently and returns their results in plan order, so
// output does not shuffle between runs.
func (e *Engine) runJobs(
	ctx context.Context, jobs []job, opts Options, runID int64, budget *budget,
) []FamilyResult {
	results := make([]FamilyResult, len(jobs))
	queue := make(chan int)

	var wg sync.WaitGroup
	for range opts.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				results[i] = e.runJob(ctx, jobs[i], opts, runID, budget)
			}
		}()
	}

	for i := range jobs {
		select {
		case queue <- i:
		case <-ctx.Done():
			close(queue)
			wg.Wait()
			return results
		}
	}
	close(queue)
	wg.Wait()
	return results
}

// runJob dispatches on how the family has to be read.
//
// The budget is checked before the job starts as well as inside it. Without
// this, every job would get at least one request through, so a small budget
// would be overrun by however many jobs the plan contains rather than by the
// handful still in flight.
func (e *Engine) runJob(
	ctx context.Context, j job, opts Options, runID int64, budget *budget,
) FamilyResult {
	if reason := budget.exceeded(ctx); reason != nil {
		return FamilyResult{
			Family: j.meta.Name, Scope: j.scope, Label: j.label, Err: reason,
		}
	}

	switch j.kind {
	case kindDocument:
		return e.pullDocument(ctx, j, runID)
	case kindReport:
		return e.pullReport(ctx, j, opts)
	case kindPayrollYear:
		return e.pullPayrollYear(ctx, j, budget)
	default:
		return e.pullCollection(ctx, j, opts, runID, budget)
	}
}

func (e *Engine) pullCollection(
	ctx context.Context, j job, opts Options, runID int64, budget *budget,
) FamilyResult {
	out := FamilyResult{Family: j.meta.Name, Scope: j.scope, Label: j.label}
	tracker := e.report.Track(j.key(), 0, ui.UnitsCount)

	state, err := e.db.FamilyState(ctx, e.account.ID, j.meta.Name, j.scope)
	if err != nil {
		out.Err = err
		tracker.Fail(err)
		return out
	}

	listOpts, fullScan := e.listOptions(j, state, opts)
	out.FullScan = fullScan

	// Recorded before the first request: anything not touched after this
	// point is what a sweep considers gone.
	out.sweepStart = time.Now()

	var highWater time.Time
	out.completed = true

	for page, err := range e.client.PagesAt(ctx, j.meta, j.requestPath(), listOpts) {
		if err != nil {
			// A 403 or 404 on a collection endpoint means this company does
			// not have the feature: a plan or role excludes it, or its company
			// type never had it. Recorded so it shows in the run, but not as
			// something to fix. A genuinely broken endpoint answers 5xx.
			if isUnavailable(err) {
				out.Unavailable, out.completed = true, true
				break
			}
			out.Err, out.completed = err, false
			break
		}
		out.Pages = page.Number

		stats, latest, err := e.archivePage(ctx, j.meta.Name, page)
		if err != nil {
			out.Err, out.completed = err, false
			break
		}
		out.Stats.Add(stats)

		// Before the last page all that is known is an upper bound; on the
		// last page the exact count is known, so the bar finishes at 100 rather
		// than at whatever fraction of a full page the family happened to be.
		switch {
		case page.Last > 0 && page.Number >= page.Last:
			tracker.SetTotal(int64(out.Stats.Total()))
		case page.Last > 0 && listOpts.PerPage > 0:
			tracker.SetTotal(int64(page.Last) * int64(listOpts.PerPage))
		}
		if latest.After(highWater) {
			highWater = latest
		}
		tracker.Add(int64(stats.Total()))

		if reason := budget.exceeded(ctx); reason != nil {
			out.Err, out.completed = reason, false
			break
		}
	}

	// A partial walk cannot move the cursor. Pagination is by page number, so
	// a stopped walk has no guarantee about which records it saw; re-reading
	// next time is the cheap option.
	if out.completed && store.AdvancesCursor(opts.Mode) && !highWater.IsZero() {
		if err := e.db.SaveCursor(
			ctx, e.account.ID, j.meta.Name, j.scope, highWater, runID); err != nil {
			out.Err = err
		} else {
			out.Cursor, out.CursorAdvance = highWater, true
		}
	}

	switch {
	case out.Err != nil:
		tracker.Fail(out.Err)
	case out.Unavailable:
		tracker.Message("not available")
		tracker.Done()
	default:
		tracker.Done()
	}
	return out
}

// isUnavailable recognises the API saying a company does not have something:
// 403 for a feature the role or plan excludes, 404 for one the company type
// does not have at all.
func isUnavailable(err error) bool {
	return errors.Is(err, freeagent.ErrNotFound) || errors.Is(err, freeagent.ErrForbidden)
}

// Unavailable lists the families this company does not have.
func (r Result) Unavailable() []FamilyResult {
	var out []FamilyResult
	for _, f := range r.Families {
		if f.Unavailable {
			out = append(out, f)
		}
	}
	return out
}

// sweepFamilies marks records the far end no longer has.
//
// A sweep is per family, not per job, because deleted_at has no scope. A
// bank-scoped family is therefore swept only once every one of its accounts
// has been read in full: sweeping after a partial fan-out would delete the
// accounts that had not been reached yet.
func (e *Engine) sweepFamilies(
	ctx context.Context, results []FamilyResult, opts Options, runID int64,
) {
	for _, family := range familiesIn(results) {
		// Documents and reports have nothing to sweep: a document is a single
		// row that every run rewrites, and a report never enters records.
		if meta, ok := freeagent.Resources[family]; ok {
			switch Classify(meta) {
			case ClassSingleton, ClassReport, ClassYearScoped:
				continue
			}
		}
		jobs := jobsForFamily(results, family)

		due, err := e.sweepDue(ctx, family, jobs, opts)
		if err != nil {
			markFamilyError(results, family, err)
			continue
		}
		if !due {
			continue
		}

		deleted, err := e.db.SoftDeleteUnseen(
			ctx, e.account.ID, family, earliestSweepStart(jobs), runID)
		if err != nil {
			markFamilyError(results, family, err)
			continue
		}
		recordSweep(results, family, deleted)

		if err := e.db.SaveReconcile(
			ctx, e.account.ID, family, "", earliestSweepStart(jobs), runID); err != nil {
			markFamilyError(results, family, err)
		}
	}
}

// sweepDue decides whether a family may be swept: every one of its jobs must
// have completed a full read, and a sweep must have been asked for.
func (e *Engine) sweepDue(
	ctx context.Context, family string, jobs []FamilyResult, opts Options,
) (bool, error) {
	for _, j := range jobs {
		if !j.completed || !j.FullScan || j.Unavailable {
			return false, nil
		}
	}
	if opts.Reconcile {
		return true, nil
	}
	if !opts.ReconcileIfDue {
		return false, nil
	}

	state, err := e.db.FamilyState(ctx, e.account.ID, family, "")
	if err != nil {
		return false, err
	}
	if state.LastFullReconcile.IsZero() {
		return true, nil
	}
	return time.Since(state.LastFullReconcile) >= opts.ReconcileInterval, nil
}

// archivePage converts a page into records and writes them, returning the
// latest updated_at it saw. The high-water mark comes from the payloads
// themselves, never from the clock, so a record written while the run was in
// flight is picked up next time instead of being skipped.
func (e *Engine) archivePage(
	ctx context.Context, family string, page api.Page,
) (store.UpsertStats, time.Time, error) {
	var stats store.UpsertStats
	var latest time.Time

	records := make([]store.Record, 0, len(page.Records))
	var attachments []store.Attachment
	for _, raw := range page.Records {
		rec, err := store.NewRecord(family, raw)
		if err != nil {
			return stats, latest, err
		}
		if rec.UpdatedAt.After(latest) {
			latest = rec.UpdatedAt
		}
		records = append(records, rec)

		found, err := extractAttachments(family, rec.Body)
		if err != nil {
			return stats, latest, err
		}
		attachments = append(attachments, found...)
	}

	stats, err := e.db.UpsertRecords(ctx, e.account.ID, records)
	if err != nil {
		return stats, latest, err
	}

	// Attachments are queued, not fetched. The bytes live on a third-party
	// host and downloading them mid-page would stall the archive behind them.
	if _, err := e.db.UpsertAttachments(ctx, e.account.ID, attachments); err != nil {
		return stats, latest, err
	}
	return stats, latest, nil
}

// listOptions decides how far back to read. It reports whether the result is
// a full scan, which is what makes a sweep meaningful.
func (e *Engine) listOptions(
	j job, state store.FamilyState, opts Options,
) (*freeagent.ListOptions, bool) {
	list := &freeagent.ListOptions{PerPage: freeagent.MaxPerPage, Extra: j.extra}

	switch opts.Mode {
	case store.ModeAdHoc:
		list.UpdatedSince = freeagent.TimeOf(opts.Window.ChangedSince)
		list.FromDate = freeagent.DateOf(opts.Window.From)
		list.ToDate = freeagent.DateOf(opts.Window.To)
		return list, opts.Window.ChangedSince.IsZero()

	case store.ModeFull, store.ModeReconcile:
		return list, true
	}

	// Incremental. A family the probe found ignores updated_since is read in
	// full instead, because filtering it would be a lie either way.
	if state.SupportsUpdatedSince != nil && !*state.SupportsUpdatedSince {
		return list, true
	}
	if state.Cursor.IsZero() {
		return list, true
	}

	list.UpdatedSince = freeagent.TimeOf(state.Cursor.Add(-opts.Overlap))
	// Ascending order makes the walk follow the cursor, which is what lets a
	// resumed run pick up where this one stopped. Bank transactions document
	// no sort parameter, so they are left in the server's own order.
	if Classify(j.meta) != ClassBankScoped {
		list.Sort = "updated_at"
	}
	return list, false
}

func outcomeFor(ctx context.Context, results []FamilyResult, budget *budget) string {
	switch {
	case ctx.Err() != nil:
		return store.OutcomeCancelled
	case budget.hit():
		return store.OutcomeBudget
	case firstError(results) != nil:
		return store.OutcomePartial
	default:
		return store.OutcomeOK
	}
}

func familiesIn(results []FamilyResult) []string {
	var out []string
	seen := make(map[string]bool, len(results))
	for _, r := range results {
		if r.Family != "" && !seen[r.Family] {
			seen[r.Family] = true
			out = append(out, r.Family)
		}
	}
	return out
}

func jobsForFamily(results []FamilyResult, family string) []FamilyResult {
	var out []FamilyResult
	for _, r := range results {
		if r.Family == family {
			out = append(out, r)
		}
	}
	return out
}

// earliestSweepStart is the safe bound: a record re-seen by any of a family's
// jobs must survive, so the sweep may only remove what was untouched before
// the first of them began.
func earliestSweepStart(jobs []FamilyResult) time.Time {
	var earliest time.Time
	for _, j := range jobs {
		if j.sweepStart.IsZero() {
			continue
		}
		if earliest.IsZero() || j.sweepStart.Before(earliest) {
			earliest = j.sweepStart
		}
	}
	return earliest
}

// recordSweep attributes a family's deletions to its first job, so the total
// is counted once rather than once per scope.
func recordSweep(results []FamilyResult, family string, deleted int64) {
	for i := range results {
		if results[i].Family == family {
			results[i].Swept = true
		}
	}
	for i := range results {
		if results[i].Family == family {
			results[i].Deleted = deleted
			return
		}
	}
}

func markFamilyError(results []FamilyResult, family string, err error) {
	for i := range results {
		if results[i].Family == family && results[i].Err == nil {
			results[i].Err = err
			return
		}
	}
}

func firstError(results []FamilyResult) error {
	var failed []string
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.Name()+": "+r.Err.Error())
		}
	}
	if len(failed) == 0 {
		return nil
	}
	if len(failed) == 1 {
		return errors.New(failed[0])
	}
	return fmt.Errorf("%d jobs failed: %v", len(failed), failed)
}

func namesOf(families []freeagent.ResourceMeta) []string {
	out := make([]string, len(families))
	for i, m := range families {
		out[i] = m.Name
	}
	return out
}
