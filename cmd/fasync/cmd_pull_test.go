package main

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekc/freeagent-sync/internal/engine"
	"github.com/alekc/freeagent-sync/internal/lock"
	"github.com/alekc/freeagent-sync/internal/store"
)

var pullNow = time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)

// An explicit window makes a run ad-hoc, and an ad-hoc run never advances the
// cursor. Getting this wrong would let a narrow manual pull convince the next
// scheduled run it was caught up, and the gap would surface only at the next
// reconcile.
func TestPullFlagsInferTheMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		flags pullFlags
		want  string
	}{
		{"no flags", pullFlags{}, store.ModeIncremental},
		{"full", pullFlags{full: true}, store.ModeFull},
		{"from", pullFlags{from: "2w"}, store.ModeAdHoc},
		{"to", pullFlags{to: "today"}, store.ModeAdHoc},
		{"changed-since", pullFlags{changedSince: "2h"}, store.ModeAdHoc},
		{"changed-until", pullFlags{changedUntil: "1d"}, store.ModeAdHoc},
		// An explicit window wins over --full: the caller asked a narrow
		// question, so the bookkeeping stays untouched.
		{"full with a window", pullFlags{full: true, from: "2w"}, store.ModeAdHoc},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts, err := tc.flags.options(pullNow)
			if err != nil {
				t.Fatal(err)
			}
			if opts.Mode != tc.want {
				t.Errorf("mode = %s, want %s", opts.Mode, tc.want)
			}
			if store.AdvancesCursor(opts.Mode) && tc.want == store.ModeAdHoc {
				t.Error("an ad-hoc run is allowed to advance the cursor")
			}
		})
	}
}

func TestPullFlagsResolveWindows(t *testing.T) {
	t.Parallel()
	flags := pullFlags{from: "2026-01-01", to: "2026-03-31", changedSince: "2h"}

	opts, err := flags.options(pullNow)
	if err != nil {
		t.Fatal(err)
	}

	wantFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !opts.Window.From.Equal(wantFrom) {
		t.Errorf("Window.From = %s, want %s", opts.Window.From, wantFrom)
	}
	if !opts.Window.ChangedSince.Equal(pullNow.Add(-2 * time.Hour)) {
		t.Errorf("Window.ChangedSince = %s, want two hours before now",
			opts.Window.ChangedSince)
	}
}

func TestPullFlagsRejectABadWindow(t *testing.T) {
	t.Parallel()
	for _, flags := range []pullFlags{
		{from: "last tuesday"},
		{from: "2026-06-01", to: "2026-01-01"},
		{changedSince: "2W"},
	} {
		if _, err := flags.options(pullNow); err == nil {
			t.Errorf("%+v was accepted", flags)
		}
	}
}

func TestPullFlagsSplitFamilies(t *testing.T) {
	t.Parallel()
	flags := pullFlags{families: "bills, invoices ,,contacts"}

	opts, err := flags.options(pullNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opts.Families, ","); got != "bills,invoices,contacts" {
		t.Errorf("families = %q, want the list trimmed and the blank dropped", got)
	}
}

func TestPullFlagsDeadlineFromMaxDuration(t *testing.T) {
	t.Parallel()
	flags := pullFlags{maxDuration: 30 * time.Minute}
	opts, err := flags.options(pullNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := pullNow.Add(30 * time.Minute); !opts.Deadline.Equal(want) {
		t.Errorf("Deadline = %s, want %s", opts.Deadline, want)
	}

	empty := pullFlags{}
	opts, err = empty.options(pullNow)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Deadline.IsZero() {
		t.Errorf("Deadline = %s with no --max-duration, want none", opts.Deadline)
	}
}

// Distinct exit codes are what let a cron job tell a broken run from one
// flaky family without parsing the output.
func TestExitFor(t *testing.T) {
	t.Parallel()
	tests := map[string]int{
		store.OutcomeOK:      exitOK,
		store.OutcomeBudget:  exitBudget,
		store.OutcomePartial: exitPartial,
		store.OutcomeFailed:  exitPartial,
	}
	for outcome, want := range tests {
		if got := exitFor(engine.Result{Outcome: outcome}); got != want {
			t.Errorf("exitFor(%s) = %d, want %d", outcome, got, want)
		}
	}
}

func TestFamiliesCommand(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	out := h.mustRun("families")
	for _, want := range []string{
		"bills", "collection", "bank_transactions", "bank-scoped",
		"payroll", "year-scoped", "trial_balance", "report",
		// Attachments are archived through their parents, so reporting them
		// as "not yet" would understate what the mirror holds.
		"attachments", "via parents",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("families output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not yet") {
		t.Errorf("every family is now read; nothing should say \"not yet\":\n%s", out)
	}
	// The Files-area gap is invisible from inside the archive, so it is
	// repeated wherever coverage is described.
	if !strings.Contains(out, "Smart Capture") {
		t.Error("families does not mention the unreachable Files area")
	}
}

// A command that talks to the API must stop before it does anything when the
// OAuth client is not configured.
func TestPullWithoutCredentialsFailsEarly(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("account", "add", "acme")

	code, _, stderr := h.run("pull")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "FREEAGENT_CLIENT_ID") {
		t.Errorf("stderr = %q, want it to name the missing variable", stderr)
	}
}

func TestPullWithNoAccountSaysHowToAddOne(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FREEAGENT_CLIENT_ID", "id")
	t.Setenv("FREEAGENT_CLIENT_SECRET", "secret")
	h.mustRun("init")

	code, _, stderr := h.run("pull")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "account add") {
		t.Errorf("stderr = %q, want it to say how to add an account", stderr)
	}
}

func TestPullRejectsABadProgressMode(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	code, _, stderr := h.run("pull", "--progress", "loud")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "auto, always or never") {
		t.Errorf("stderr = %q, want the valid modes listed", stderr)
	}
}

func TestAuthStatusWithNoAccounts(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FREEAGENT_CLIENT_ID", "id")
	t.Setenv("FREEAGENT_CLIENT_SECRET", "secret")
	h.mustRun("init")

	out := h.mustRun("auth", "status")
	if !strings.Contains(out, "no accounts configured") {
		t.Errorf("output = %q, want it to prompt for an account", out)
	}
}

func TestAuthStatusReportsAMissingToken(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FREEAGENT_CLIENT_ID", "id")
	t.Setenv("FREEAGENT_CLIENT_SECRET", "secret")
	h.mustRun("init")
	h.mustRun("account", "add", "acme")

	code, stdout, _ := h.run("auth", "status", "--token-file", h.dataDir+"/token.json")
	if code != exitConfig {
		t.Errorf("exited %d, want %d when a token is missing", code, exitConfig)
	}
	if !strings.Contains(stdout, "no token") {
		t.Errorf("output = %q, want it to report the missing token", stdout)
	}
	if !strings.Contains(stdout, "auth login") {
		t.Errorf("output = %q, want it to say how to fix it", stdout)
	}
}

func TestUnknownAuthSubcommand(t *testing.T) {
	h := newHarness(t)
	code, _, stderr := h.run("auth", "frobnicate")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "frobnicate") {
		t.Errorf("stderr = %q, want it to name the subcommand", stderr)
	}
}

// Overlapping cron ticks must not both run: they would corrupt cursors and
// the partial downloads in tmp/, neither of which a transaction covers. The
// lock is taken before anything else, so this also proves ordering.
func TestPullExitsWhenTheLockIsHeld(t *testing.T) {
	h := newHarness(t)
	t.Setenv("FREEAGENT_CLIENT_ID", "id")
	t.Setenv("FREEAGENT_CLIENT_SECRET", "secret")
	h.mustRun("init")
	h.mustRun("account", "add", "acme")

	held, err := lock.Acquire(filepath.Join(h.dataDir, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	code, _, stderr := h.run("pull")
	if code != exitLockHeld {
		t.Errorf("exited %d, want %d for a held lock", code, exitLockHeld)
	}
	if !strings.Contains(stderr, "another run") {
		t.Errorf("stderr = %q, want it to say another run holds the directory", stderr)
	}
}

// sql is read-only. The archive is the only remaining copy of records the far
// end may have deleted, so an ad-hoc query is not where a typo in a DELETE
// should be discovered.
func TestSQLRefusesWrites(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	for _, query := range []string{
		"delete from records",
		"DROP TABLE records",
		"  update records set body = '{}'",
		"select 1; delete from records",
		"vacuum",
		"pragma user_version = 99",
		"attach database '/tmp/x' as x",
	} {
		code, _, stderr := h.run("sql", query)
		if code != exitConfig {
			t.Errorf("%q exited %d, want %d", query, code, exitConfig)
		}
		if !strings.Contains(stderr, "read-only") {
			t.Errorf("%q stderr = %q, want it to say sql is read-only", query, stderr)
		}
	}
}

func TestSQLReads(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	out := h.mustRun("sql", "select 'bills' as family, 3 as n", "-format", "tsv")
	if !strings.Contains(out, "family\tn") || !strings.Contains(out, "bills\t3") {
		t.Errorf("output = %q, want a tab-separated header and row", out)
	}
}

func TestSQLNeedsAQuery(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	code, _, stderr := h.run("sql")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr = %q, want usage", stderr)
	}
}

func TestSQLRejectsAnUnknownFormat(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	code, _, stderr := h.run("sql", "select 1", "-format", "yaml")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "table, tsv or csv") {
		t.Errorf("stderr = %q, want the valid formats listed", stderr)
	}
}

// Nothing checked is not the same as nothing wrong, and reporting a pass there
// would be the single most misleading thing this command could say.
func TestVerifyOnAnEmptyArchiveDoesNotClaimSuccess(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("account", "add", "acme")

	out := h.mustRun("verify")
	if strings.Contains(out, "passed") {
		t.Errorf("output claims a pass on an empty archive:\n%s", out)
	}
	if !strings.Contains(out, "Nothing could be checked") {
		t.Errorf("output = %q, want it to say nothing could be checked", out)
	}
}

// verify reads only the archive, so it must work with no credentials at all.
func TestVerifyNeedsNoCredentials(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("account", "add", "acme")

	code, _, stderr := h.run("verify")
	if code != exitOK {
		t.Errorf("exited %d with no credentials, want %d; stderr: %s", code, exitOK, stderr)
	}
}

func TestExportNeedsAFamily(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	code, _, stderr := h.run("export")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr = %q, want usage", stderr)
	}
}

func TestExportRejectsAnUnknownFormat(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	code, _, stderr := h.run("export", "-family", "invoices", "-format", "xlsx")
	if code != exitConfig {
		t.Errorf("exited %d, want %d", code, exitConfig)
	}
	if !strings.Contains(stderr, "csv, json or jsonl") {
		t.Errorf("stderr = %q, want the valid formats listed", stderr)
	}
}

// Export data goes to stdout so it can be redirected; progress and warnings go
// to stderr so they never end up inside the file.
func TestExportKeepsProgressOffStdout(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("account", "add", "acme")

	code, stdout, stderr := h.run("export", "-family", "invoices")
	if code != exitOK {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "wrote") {
		t.Errorf("progress leaked into the exported data:\n%s", stdout)
	}
	if !strings.Contains(stderr, "wrote") {
		t.Errorf("stderr = %q, want the summary", stderr)
	}
}

// Export and reproject read only the archive, so neither needs credentials.
func TestOfflineCommandsNeedNoCredentials(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	h.mustRun("account", "add", "acme")

	for _, args := range [][]string{
		{"export", "-family", "invoices"},
		{"reproject"},
		{"verify"},
		{"files", "rebuild"},
	} {
		code, _, stderr := h.run(args...)
		if code != exitOK {
			t.Errorf("%v exited %d with no credentials: %s", args, code, stderr)
		}
	}
}

// A scoped family runs one job per bank account, parent or tax year. Printing
// each of those gave a dozen identically named rows with nothing to tell them
// apart, so they merge into one row carrying the fan-out count.
func TestRunTableMergesScopedFamilies(t *testing.T) {
	t.Parallel()
	results := []engine.FamilyResult{
		{Family: "bills", Pages: 1, FullScan: true},
		{Family: "bank_transactions", Scope: "acct/1", Label: "Current",
			Pages: 2, FullScan: true},
		{Family: "bank_transactions", Scope: "acct/2", Label: "Savings",
			Pages: 3, FullScan: true},
		{Family: "bank_transactions", Scope: "acct/3", Label: "Reserve",
			Pages: 1, FullScan: true},
	}
	results[1].Stats.Inserted = 170
	results[2].Stats.Inserted = 135
	results[3].Stats.Inserted = 54

	merged := mergeByFamily(results)
	if len(merged) != 2 {
		t.Fatalf("merged into %d rows, want 2", len(merged))
	}

	bank := merged[1]
	if bank.Family != "bank_transactions" {
		t.Fatalf("second row = %s", bank.Family)
	}
	if bank.Pages != 6 {
		t.Errorf("pages = %d, want the sum 6", bank.Pages)
	}
	if bank.Stats.Inserted != 359 {
		t.Errorf("inserted = %d, want the sum 359", bank.Stats.Inserted)
	}
	// The fan-out has to show, or a family that cost sixty requests looks
	// like one that cost one.
	if note := familyNote(bank); !strings.Contains(note, "3 scopes") {
		t.Errorf("note = %q, want it to report 3 scopes", note)
	}
	// An unscoped family says nothing about scopes.
	if note := familyNote(merged[0]); strings.Contains(note, "scope") {
		t.Errorf("bills note = %q, want no scope count", note)
	}
}

// The merged cursor is the latest of the family's jobs: reporting an earlier
// one would suggest less progress than was made.
func TestMergedRowKeepsTheLatestCursor(t *testing.T) {
	t.Parallel()
	early := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	late := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

	merged := mergeByFamily([]engine.FamilyResult{
		{Family: "bank_transactions", Scope: "a", Cursor: early, CursorAdvance: true},
		{Family: "bank_transactions", Scope: "b", Cursor: late, CursorAdvance: true},
	})
	if !merged[0].Cursor.Equal(late) {
		t.Errorf("cursor = %s, want the latest %s", merged[0].Cursor, late)
	}
}

// A family is only unavailable when every one of its jobs was. One bank account
// answering 403 does not mean the company has no bank transactions.
func TestMergedRowIsUnavailableOnlyIfEveryScopeWas(t *testing.T) {
	t.Parallel()
	partly := mergeByFamily([]engine.FamilyResult{
		{Family: "bank_transactions", Scope: "a", Unavailable: true},
		{Family: "bank_transactions", Scope: "b", FullScan: true},
	})
	if partly[0].Unavailable {
		t.Error("one unavailable scope marked the whole family unavailable")
	}

	fully := mergeByFamily([]engine.FamilyResult{
		{Family: "income_tax_returns", Scope: "a", Unavailable: true},
		{Family: "income_tax_returns", Scope: "b", Unavailable: true},
	})
	if !fully[0].Unavailable {
		t.Error("a family with no available scope was not marked unavailable")
	}
	if note := familyNote(fully[0]); note != "not available" {
		t.Errorf("note = %q, want \"not available\"", note)
	}
}

// --by-scope is how the per-account detail is still reachable.
func TestByScopeKeepsEveryRow(t *testing.T) {
	t.Parallel()
	var flags pullFlags
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	flags.register(fs)
	if err := fs.Parse([]string{"--by-scope"}); err != nil {
		t.Fatal(err)
	}
	if !flags.byScope {
		t.Error("--by-scope did not register")
	}
}
