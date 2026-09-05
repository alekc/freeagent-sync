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

// DefaultMaxSweepFraction is the share of a family's live records a sweep may
// remove before it is refused. Genuine upstream deletion is a trickle; a bulk
// hit is nearly always a read that came back short.
const DefaultMaxSweepFraction = 0.10

// MinSweepFloor is the live-record count below which a fraction says nothing:
// a three-record family losing one is a third of it. Smaller families are
// swept without a bound.
const MinSweepFloor = 20

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
	// DryRun computes each sweep and reports it without deleting anything.
	// The read still archives: last_seen_at is what the sweep set is derived
	// from, so there is nothing to preview without it.
	DryRun bool
	// MaxSweepFraction bounds a sweep to a share of a family's live records.
	// Zero means DefaultMaxSweepFraction, so a caller that never heard of the
	// bound still gets it. One or more removes it.
	MaxSweepFraction float64
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
	if o.MaxSweepFraction == 0 {
		o.MaxSweepFraction = DefaultMaxSweepFraction
	}
	if o.Mode == "" {
		o.Mode = store.ModeIncremental
	}
}

// SweepState is what happened to one family's sweep. A bool cannot separate
// "checked, nothing gone" from "not checked", and reporting both as zero is
// how a sweep that never ran reads as a clean one.
type SweepState string

// The terminal states a family's sweep can reach.
const (
	// SweepNone: not asked for, or the family has nothing to sweep.
	SweepNone SweepState = ""
	// SweepSkipped: asked for, but the read did not cover the whole family.
	SweepSkipped SweepState = "skipped"
	// SweepRefused: the sweep set exceeded MaxSweepFraction of live records.
	SweepRefused SweepState = "refused"
	// SweepPreview: computed under DryRun, with the delete withheld.
	SweepPreview SweepState = "dry-run"
	// SweepDone: the sweep ran and its count is real.
	SweepDone SweepState = "swept"
)

// FamilyResult is what happened to one job: a family, or one scope of one.
type FamilyResult struct {
	Family string
	Scope  string
	Label  string
	Pages  int
	Stats  store.UpsertStats
	// Deleted is the size of the sweep set: removed when Sweep is SweepDone,
	// and what would have been removed under SweepPreview or SweepRefused.
	// Read it with Sweep, never on its own.
	Deleted       int64
	Cursor        time.Time
	CursorAdvance bool
	Sweep         SweepState
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
		// Only a sweep that ran contributes: a preview or a refusal carries a
		// count of what it would have removed, which is not a deletion.
		if f.Sweep == SweepDone {
			result.Deleted += f.Deleted
		}
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

		asked, err := e.sweepAsked(ctx, family, opts)
		if err != nil {
			markFamilyError(results, family, err)
			continue
		}
		// A family this company does not have is not a family the read failed
		// to cover, and reporting it as one would put every absent feature in
		// the "not fully read" count on every run.
		if !asked || familyUnavailable(jobs) {
			continue
		}
		if !readCoveredFamily(jobs) {
			// Asked for and refused, which must not read as swept clean.
			setSweep(results, family, SweepSkipped, 0)
			continue
		}

		start := earliestSweepStart(jobs)
		unseen, err := e.db.CountUnseen(ctx, e.account.ID, family, start)
		if err != nil {
			markFamilyError(results, family, err)
			continue
		}

		// Bounded before the write and under a dry run alike, so a preview is
		// a faithful answer to what a real sweep would do rather than a more
		// permissive one.
		bound := sweepBound{
			unseen: unseen, added: addedThisRun(jobs), max: opts.MaxSweepFraction,
		}
		if err := e.checkSweepBound(ctx, family, bound); err != nil {
			setSweep(results, family, SweepRefused, unseen)
			markFamilyError(results, family, err)
			continue
		}

		if opts.DryRun {
			// No SaveReconcile either: recording a sweep that did not happen
			// would silence --reconcile-if-due for a whole interval.
			setSweep(results, family, SweepPreview, unseen)
			continue
		}

		deleted, err := e.db.SoftDeleteUnseen(ctx, e.account.ID, family, start, runID)
		if err != nil {
			markFamilyError(results, family, err)
			continue
		}
		setSweep(results, family, SweepDone, deleted)

		if err := e.db.SaveReconcile(
			ctx, e.account.ID, family, "", start, runID); err != nil {
			markFamilyError(results, family, err)
		}
	}
}

// sweepAsked reports whether this run wants a sweep of this family: a question
// about the caller's flags and the cadence, never about what the read managed
// to cover. Those two were one condition once, which is how a short read swept
// anyway.
func (e *Engine) sweepAsked(
	ctx context.Context, family string, opts Options,
) (bool, error) {
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

// familyUnavailable reports whether this company has the family at all. One
// bank account answering 403 does not mean the company has no bank
// transactions, so every job has to say so.
func familyUnavailable(jobs []FamilyResult) bool {
	for _, j := range jobs {
		if !j.Unavailable {
			return false
		}
	}
	return len(jobs) > 0
}

// readCoveredFamily reports whether every one of a family's jobs completed a
// full read. Only then does "the read did not see it" mean "the far end no
// longer has it".
func readCoveredFamily(jobs []FamilyResult) bool {
	for _, j := range jobs {
		if !j.completed || !j.FullScan || j.Unavailable {
			return false
		}
	}
	return true
}

// sweepBound is what the bound is measured from: the size of the sweep set,
// how much of the family this run itself made live, and the share allowed.
type sweepBound struct {
	unseen int64
	added  int64
	max    float64
}

// addedThisRun counts the records this run made live in a family, whether new
// or brought back. Neither can be in the sweep set, so leaving them in the
// denominator would let a read that replaced a family wholesale look small.
func addedThisRun(jobs []FamilyResult) int64 {
	var n int64
	for _, j := range jobs {
		n += int64(j.Stats.Inserted + j.Stats.Restored)
	}
	return n
}

// checkSweepBound refuses a sweep that would remove more of a family than the
// caller allowed. Upstream deletion is a trickle; a bulk hit is nearly always
// a read that came back short, and that is the case a sweep must not act on.
func (e *Engine) checkSweepBound(
	ctx context.Context, family string, b sweepBound,
) error {
	if b.unseen == 0 || b.max >= 1 {
		return nil
	}
	live, err := e.db.LiveRecordCount(ctx, e.account.ID, family)
	if err != nil {
		return err
	}
	// Measured against what the family held before this run, not after it: a
	// far end answering four hundred new records instead of the forty it had
	// would otherwise dilute its own deletion to under a tenth.
	before := live - b.added
	// Below the floor a fraction is noise, so the bound does not apply.
	if before < MinSweepFloor {
		return nil
	}
	if fraction := float64(b.unseen) / float64(before); fraction > b.max {
		return fmt.Errorf(
			"engine: refusing to sweep %s: %d of the %d records held before this "+
				"run (%.1f%%) is over the %.1f%% bound, which usually means the "+
				"read came back short; check the run, or raise "+
				"-max-sweep-fraction deliberately",
			family, b.unseen, before, fraction*100, b.max*100)
	}
	return nil
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

// setSweep records a family's sweep outcome on every one of its jobs and the
// count on the first, so a fan-out reports the total once rather than once per
// scope.
func setSweep(results []FamilyResult, family string, state SweepState, n int64) {
	first := true
	for i := range results {
		if results[i].Family != family {
			continue
		}
		results[i].Sweep = state
		if first {
			results[i].Deleted = n
			first = false
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
