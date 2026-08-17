package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"

	"github.com/alekc/freeagent-sync/internal/engine"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/timeframe"
)

// pullFlags are the knobs shared by pull and reconcile.
type pullFlags struct {
	families     string
	full         bool
	reconcile    bool
	reconcileDue bool

	from         string
	to           string
	changedSince string
	changedUntil string

	overlap     time.Duration
	concurrency int
	maxRequests int64
	maxDuration time.Duration

	noBlobs bool
	noFiles bool
	byScope bool
}

func (p *pullFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&p.families, "family", "",
		"comma-separated families to read (default: every archivable family)")
	fs.BoolVar(&p.full, "full", false, "ignore the stored cursor and read everything")
	fs.BoolVar(&p.reconcile, "reconcile", false,
		"also sweep for records the far end no longer has")
	fs.BoolVar(&p.reconcileDue, "reconcile-if-due", false,
		"sweep only families whose last sweep is older than the interval")

	fs.StringVar(&p.from, "from", "", "business date lower bound ("+timeframe.Syntax+")")
	fs.StringVar(&p.to, "to", "", "business date upper bound")
	fs.StringVar(&p.changedSince, "changed-since", "",
		"only records modified at or after this point")
	fs.StringVar(&p.changedUntil, "changed-until", "",
		"only records modified at or before this point (filtered locally: the API has no such filter)")

	fs.DurationVar(&p.overlap, "overlap", engine.DefaultOverlap,
		"how far back beyond the cursor an incremental run reaches")
	fs.IntVar(&p.concurrency, "concurrency", engine.DefaultConcurrency,
		"how many families to read at once")
	fs.Int64Var(&p.maxRequests, "max-requests", 0, "stop after this many API calls (0: no limit)")
	fs.DurationVar(&p.maxDuration, "max-duration", 0,
		"stop after this long, so a run cannot overrun the next scheduled tick")

	fs.BoolVar(&p.noBlobs, "no-blobs", false, "skip downloading attachments")
	fs.BoolVar(&p.noFiles, "no-files", false,
		"skip regenerating the record and file trees")
	fs.BoolVar(&p.byScope, "by-scope", false,
		"one row per bank account, parent or tax year instead of one per family")
}

// options resolves the flags into engine options, deciding the run mode from
// what the caller actually asked for.
func (p *pullFlags) options(now time.Time) (engine.Options, error) {
	dates, err := timeframe.ParseDateWindow(p.from, p.to, now)
	if err != nil {
		return engine.Options{}, err
	}
	changes, err := timeframe.ParseWindow(p.changedSince, p.changedUntil, now)
	if err != nil {
		return engine.Options{}, err
	}

	opts := engine.Options{
		Window: store.RunWindow{
			From:         dates.From,
			To:           dates.To,
			ChangedSince: changes.From,
			ChangedUntil: changes.To,
		},
		Overlap:        p.overlap,
		Concurrency:    p.concurrency,
		MaxRequests:    p.maxRequests,
		Reconcile:      p.reconcile,
		ReconcileIfDue: p.reconcileDue,
	}
	if p.families != "" {
		opts.Families = splitList(p.families)
	}
	if p.maxDuration > 0 {
		opts.Deadline = now.Add(p.maxDuration)
	}

	// An explicit window makes the run ad-hoc, which is what keeps it from
	// advancing the cursor. A narrow manual pull must never leave the next
	// scheduled run believing it is caught up.
	switch {
	case !dates.IsZero() || !changes.IsZero():
		opts.Mode = store.ModeAdHoc
	case p.full:
		opts.Mode = store.ModeFull
	default:
		opts.Mode = store.ModeIncremental
	}
	return opts, nil
}

func cmdPull(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("pull", e)
	e.g.register(fs)
	var flags pullFlags
	flags.register(fs)
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	opts, err := flags.options(time.Now())
	if err != nil {
		return e.fail(err)
	}
	return e.runEngine(ctx, opts, flags)
}

func cmdReconcile(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("reconcile", e)
	e.g.register(fs)
	var flags pullFlags
	flags.register(fs)
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	opts, err := flags.options(time.Now())
	if err != nil {
		return e.fail(err)
	}
	// A sweep has to see everything to know what is missing, so it is always
	// a full read regardless of the cursor.
	opts.Mode = store.ModeFull
	opts.Reconcile = true
	return e.runEngine(ctx, opts, flags)
}

// runEngine is the shared body of pull and reconcile: take the lock, build a
// read-only client, run, report, and map the outcome to an exit code.
func (e *env) runEngine(ctx context.Context, opts engine.Options, flags pullFlags) int {
	session, code := e.openSession(ctx)
	if session == nil {
		return code
	}
	defer session.Close()

	result, err := session.engine.Pull(ctx, opts)
	if err != nil {
		return e.fail(err)
	}
	e.printRun(result, flags.byScope)

	code = exitFor(result)
	if !flags.noBlobs {
		if blobCode := e.fetchBlobsAfterPull(ctx, session, opts); blobCode != exitOK {
			code = worse(code, blobCode)
		}
	}
	// Derived last, so the trees reflect the attachments this run downloaded
	// rather than the state they were in before it.
	if !flags.noFiles {
		e.derivedAfterPull(ctx, session)
	}
	return code
}

// fetchBlobsAfterPull downloads whatever the archive pass queued. Attachments
// come off a third-party host and spend no API budget, so this runs by
// default rather than needing its own command.
func (e *env) fetchBlobsAfterPull(
	ctx context.Context, s *session, opts engine.Options,
) int {
	blobs, err := openBlobStore(s.cfg)
	if err != nil {
		return e.fail(err)
	}
	result, err := s.engine.FetchBlobs(ctx, blobs, engine.BlobOptions{
		Deadline: opts.Deadline,
	})
	if err != nil {
		return e.fail(err)
	}
	if result.Attempted == 0 {
		return exitOK
	}
	fprintln(e.out)
	return e.reportBlobs(ctx, s, result)
}

// worse keeps the more serious of two exit codes, so a clean archive pass
// followed by a failed blob pass still reports partial.
func worse(a, b int) int {
	if b > a {
		return b
	}
	return a
}

func (e *env) printRun(result engine.Result, byScope bool) {
	t := newTable(e)
	t.AppendHeader(table.Row{
		"Family", "Pages", "New", "Changed", "Same", "Restored", "Gone", "Note",
	})

	// One row per family by default. A scoped family runs one job per bank
	// account, parent or tax year, and printing each of those produced a dozen
	// identically named rows with no way to tell them apart.
	rows := result.Families
	if !byScope {
		rows = mergeByFamily(rows)
	}
	for _, f := range rows {
		label := f.Family
		if byScope {
			label = f.Name()
		}
		t.AppendRow(table.Row{
			label, f.Pages, f.Stats.Inserted, f.Stats.Updated, f.Stats.Unchanged,
			f.Stats.Restored, f.Deleted, familyNote(f),
		})
	}
	t.Render()

	fprintf(e.out, "\n%s: %d records archived, %d gone, %d requests\n",
		result.Outcome, result.Stats.Total(), result.Deleted, result.Requests)

	// A family this company does not have is worth knowing about but is not
	// something to fix, so it is listed apart from the failures.
	if unavailable := result.Unavailable(); len(unavailable) > 0 {
		fprintf(e.out, "\nNot available to this company (%d):\n", len(unavailable))
		for _, f := range unavailable {
			fprintf(e.out, "  %s\n", f.Name())
		}
	}

	// Anything not read directly is named, not silently omitted, so the shape
	// of the coverage is visible on every run.
	if len(result.Deferred) > 0 {
		fprintf(e.out, "\nNot read directly (%d):\n", len(result.Deferred))
		for _, name := range sortedKeys(result.Deferred) {
			fprintf(e.out, "  %-32s %-16s %s\n",
				name, result.Deferred[name], coverageOf(result.Deferred[name]))
		}
	}
	if len(result.Failed()) > 0 {
		fprintln(e.out)
		for _, f := range result.Failed() {
			fprintf(e.err, "failed: %s: %v\n", f.Family, f.Err)
		}
	}
}

// mergeByFamily collapses a family's jobs into one row, summing the counts and
// keeping the latest cursor. Scope detail is available with --by-scope; a run
// summary should answer what happened, not enumerate every fan-out.
func mergeByFamily(results []engine.FamilyResult) []engine.FamilyResult {
	var order []string
	merged := map[string]*engine.FamilyResult{}
	scopes := map[string]int{}

	for _, f := range results {
		existing, seen := merged[f.Family]
		if !seen {
			copied := f
			merged[f.Family] = &copied
			order = append(order, f.Family)
		} else {
			existing.Pages += f.Pages
			existing.Stats.Add(f.Stats)
			existing.Deleted += f.Deleted
			existing.Swept = existing.Swept || f.Swept
			existing.FullScan = existing.FullScan || f.FullScan
			existing.CursorAdvance = existing.CursorAdvance || f.CursorAdvance
			// A family is only unavailable if every one of its jobs was.
			existing.Unavailable = existing.Unavailable && f.Unavailable
			if f.Cursor.After(existing.Cursor) {
				existing.Cursor = f.Cursor
			}
			if existing.Err == nil {
				existing.Err = f.Err
			}
		}
		if f.Scope != "" {
			scopes[f.Family]++
		}
	}

	out := make([]engine.FamilyResult, 0, len(order))
	for _, family := range order {
		f := *merged[family]
		if count := scopes[family]; count > 1 {
			// The label carries the fan-out, so a family that cost sixty
			// requests does not look like one that cost one.
			f.Label = fmt.Sprintf("%d scopes", count)
			f.Scope = "merged"
		} else {
			f.Scope = ""
		}
		out = append(out, f)
	}
	return out
}

func familyNote(f engine.FamilyResult) string {
	if f.Unavailable {
		return "not available"
	}
	var notes []string
	if f.Scope == "merged" {
		notes = append(notes, f.Label)
	}
	if f.FullScan {
		notes = append(notes, "full")
	}
	if f.Swept {
		notes = append(notes, "swept")
	}
	if f.CursorAdvance {
		notes = append(notes, "cursor "+f.Cursor.Format(time.DateOnly))
	}
	if f.Err != nil {
		notes = append(notes, "failed")
	}
	return strings.Join(notes, ", ")
}

// exitFor maps a run outcome to a process exit code, so a cron job can tell
// a broken run from one flaky family without reading the output.
func exitFor(result engine.Result) int {
	switch result.Outcome {
	case store.OutcomeOK:
		return exitOK
	case store.OutcomeBudget:
		return exitBudget
	case store.OutcomePartial:
		return exitPartial
	default:
		return exitPartial
	}
}

func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
