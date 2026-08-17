package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"

	"github.com/alekc/freeagent-sync/internal/store"
)

// filesAreaNote is printed on every status. FreeAgent's Files and Smart
// Capture areas have no API, and a file leaves that area when it is attached,
// so unattached uploads are structurally unreachable from here.
const filesAreaNote = "Files and Smart Capture uploads that are not attached to a bill, " +
	"expense or bank transaction have no API and are not mirrored."

func cmdStatus(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("status", e)
	e.g.register(fs)
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	cfg, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	fprintf(e.out, "archive        %s (%s)\n", db.Path(), archiveSize(db.Path()))
	fprintf(e.out, "schema         %d\n", store.SchemaVersion)
	fprintf(e.out, "blobs          %s\n", cfg.BlobsDir)
	fprintf(e.out, "tokens         %s\n", cfg.TokenFile)

	accounts, err := db.Accounts(ctx)
	if err != nil {
		return e.fail(err)
	}
	fprintf(e.out, "accounts       %d\n", len(accounts))
	if len(accounts) == 0 {
		fprintln(e.out, "\nNothing configured yet: fasync account add <slug>")
		return exitOK
	}

	if err := printPerAccount(ctx, e, db, accounts); err != nil {
		return e.fail(err)
	}
	fprintf(e.out, "\nNote: %s\n", filesAreaNote)
	return exitOK
}

func printPerAccount(ctx context.Context, e *env, db *store.DB, accounts []store.Account) error {
	t := newTable(e)
	t.AppendHeader(table.Row{
		"Account", "Environment", "Records", "Deleted", "Attachments", "Docs",
		"Size", "Last run",
	})

	for _, a := range accounts {
		counts, err := recordCounts(ctx, db, a.ID)
		if err != nil {
			return err
		}
		attachments, err := db.AttachmentCounts(ctx, a.ID)
		if err != nil {
			return err
		}
		docs, err := db.DocumentCount(ctx, a.ID, store.DocumentKindPDF)
		if err != nil {
			return err
		}
		last, err := lastRun(ctx, db, a.ID)
		if err != nil {
			return err
		}
		t.AppendRow(table.Row{
			a.Slug, a.Environment, counts.live, counts.deleted,
			attachmentSummary(attachments), docs,
			humanBytes(attachments.Bytes), last,
		})
	}
	fprintln(e.out)
	t.Render()
	return nil
}

type counts struct{ live, deleted int64 }

func recordCounts(ctx context.Context, db *store.DB, accountID int64) (counts, error) {
	var c counts
	err := db.QueryRowContext(ctx,
		`SELECT
		    count(*) FILTER (WHERE deleted_at IS NULL),
		    count(*) FILTER (WHERE deleted_at IS NOT NULL)
		 FROM records WHERE account_id = ?`, accountID).Scan(&c.live, &c.deleted)
	if err != nil {
		return c, fmt.Errorf("counting records: %w", err)
	}
	return c, nil
}

// attachmentSummary shows outstanding work rather than a bare total, because
// "12 of 14" is the number anyone reading status actually wants.
func attachmentSummary(c store.AttachmentCounts) string {
	if c.Total == 0 {
		return "0"
	}
	if c.Stored == c.Total {
		return fmt.Sprintf("%d", c.Total)
	}
	return fmt.Sprintf("%d of %d", c.Stored, c.Total)
}

// lastRun reports when this account last completed a run, which is the
// question anyone debugging a cron schedule is actually asking.
func lastRun(ctx context.Context, db *store.DB, accountID int64) (string, error) {
	var started, outcome string
	err := db.QueryRowContext(ctx,
		`SELECT started_at, coalesce(outcome, 'in progress')
		 FROM sync_runs WHERE account_id = ?
		 ORDER BY started_at DESC LIMIT 1`, accountID).Scan(&started, &outcome)
	if err != nil {
		return "never", nil //nolint:nilerr // no rows is a legitimate "never"
	}

	when, err := store.ParseTime(started)
	if err != nil {
		return "", err
	}
	return when.Local().Format("2006-01-02 15:04") + " (" + outcome + ")", nil
}

func archiveSize(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "unknown size"
	}
	return humanBytes(info.Size())
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
