package main

import (
	"context"
	"fmt"
	"os"
)

// dirPerm keeps the archive readable only by its owner. It holds bank
// transactions, invoices and scanned receipts.
const dirPerm os.FileMode = 0o700

func cmdInit(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("init", e)
	e.g.register(fs)
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	cfg, err := e.g.config()
	if err != nil {
		return e.fail(err)
	}

	for _, dir := range []string{
		cfg.DataDir, cfg.BlobsDir, cfg.TmpDir, cfg.RecordsDir, cfg.FilesDir,
	} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return e.fail(fmt.Errorf("creating %s: %w", dir, err))
		}
	}

	// Opening runs the migrations, so the archive exists and is at the
	// current schema by the time this returns.
	db, err := e.openArchiveAt(ctx, cfg)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	fprintf(e.out, "archive   %s\n", db.Path())
	fprintf(e.out, "blobs     %s\n", cfg.BlobsDir)
	fprintf(e.out, "records   %s\n", cfg.RecordsDir)
	fprintf(e.out, "files     %s\n", cfg.FilesDir)
	fprintf(e.out, "tokens    %s\n", cfg.TokenFile)
	fprintln(e.out, "\nNext: fasync account add <slug> -env sandbox")
	return exitOK
}
