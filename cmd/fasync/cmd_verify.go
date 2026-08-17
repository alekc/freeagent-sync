package main

import (
	"context"
	"time"

	"github.com/alekc/freeagent-sync/internal/engine"
	"github.com/alekc/freeagent-sync/internal/timeframe"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// verifyMarks are the glyphs each status prints with, so a long report can be
// scanned rather than read.
var verifyMarks = map[engine.CheckStatus]string{
	engine.CheckPass:     "ok      ",
	engine.CheckFail:     "FAILED  ",
	engine.CheckAdvisory: "advisory",
	engine.CheckSkipped:  "skipped ",
}

// cmdVerify checks the archive against itself and against FreeAgent's own
// arithmetic. Offline except for nothing: every check reads the archive, so
// this costs no API requests at all.
func cmdVerify(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("verify", e)
	e.g.register(fs)
	from := fs.String("from", "",
		"lower bound for the reconciliation (default: the snapshot's own window)")
	to := fs.String("to", "", "upper bound for the reconciliation")
	noBlobs := fs.Bool("no-blobs", false, "skip re-hashing stored attachments")
	detail := fs.Int("detail", 20, "how many lines each check may report")
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	window, err := timeframe.ParseDateWindow(*from, *to, time.Now())
	if err != nil {
		return e.fail(err)
	}

	cfg, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	account, err := e.resolveAccount(ctx, db)
	if err != nil {
		return e.fail(err)
	}

	// No API client: verification is entirely local, which is what makes it
	// safe to run against a production archive as often as you like.
	eng := engine.NewOffline(db, nopReporter{}, *account)

	opts := engine.VerifyOptions{
		FromDate:  formatDate(window.From),
		ToDate:    formatDate(window.To),
		MaxDetail: *detail,
	}
	if !*noBlobs {
		blobs, err := openBlobStore(cfg)
		if err != nil {
			return e.fail(err)
		}
		opts.Blobs = blobs
	}

	result, err := eng.Verify(ctx, opts)
	if err != nil {
		return e.fail(err)
	}

	e.printVerify(result)
	if result.Failed() {
		return exitPartial
	}
	return exitOK
}

func (e *env) printVerify(result engine.VerifyResult) {
	var advisory, passed int
	for _, check := range result.Checks {
		fprintf(e.out, "%s  %-34s %s\n",
			verifyMarks[check.Status], check.Name, check.Summary)
		for _, line := range check.Detail {
			fprintf(e.out, "              %s\n", line)
		}
		switch check.Status {
		case engine.CheckAdvisory:
			advisory++
		case engine.CheckPass:
			passed++
		}
	}

	fprintln(e.out)
	switch {
	case result.Failed():
		fprintln(e.out, "The archive has a gap. See the failed checks above.")
	case advisory > 0:
		fprintln(e.out,
			"No failures. The advisory differences need a human's judgement, not a fix.")
	case passed == 0:
		// Reporting success here would be the worst possible answer: nothing
		// was checked, which is not the same as nothing being wrong.
		fprintln(e.out, "Nothing could be checked yet. Run a pull first.")
	default:
		fprintf(e.out, "All %d checks that could run passed.\n", passed)
	}
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.DateOnly)
}

// nopReporter satisfies the progress interface for commands that do no work
// worth reporting on.
type nopReporter struct{}

func (nopReporter) Track(string, int64, ui.Units) ui.Tracker { return nopTracker{} }
func (nopReporter) Logf(string, ...any)                      {}
func (nopReporter) Close()                                   {}

type nopTracker struct{}

func (nopTracker) Add(int64)      {}
func (nopTracker) SetTotal(int64) {}
func (nopTracker) Message(string) {}
func (nopTracker) Done()          {}
func (nopTracker) Fail(error)     {}
