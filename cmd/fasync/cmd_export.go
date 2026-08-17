package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/alekc/freeagent-sync/internal/export"
	"github.com/alekc/freeagent-sync/internal/timeframe"
)

// cmdExport writes one family out. Offline: it reads only the archive.
func cmdExport(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("export", e)
	e.g.register(fs)
	family := fs.String("family", "", "the record family to write (required)")
	format := fs.String("format", "csv", "output format: csv, json or jsonl")
	flat := fs.Bool("flat", false,
		"resolve references to names for a spreadsheet (lossy)")
	from := fs.String("from", "", "business date lower bound ("+timeframe.Syntax+")")
	to := fs.String("to", "", "business date upper bound")
	includeDeleted := fs.Bool("deleted", false,
		"include records the far end no longer has")
	outPath := fs.String("out", "", "write to a file instead of stdout")
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	if *family == "" {
		fprintln(e.err, "Usage: fasync export -family invoices [-format csv|json|jsonl] [-flat]")
		return exitConfig
	}
	encoding, err := export.ParseFormat(*format)
	if err != nil {
		return e.fail(err)
	}
	window, err := timeframe.ParseDateWindow(*from, *to, time.Now())
	if err != nil {
		return e.fail(err)
	}

	_, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	account, err := e.resolveAccount(ctx, db)
	if err != nil {
		return e.fail(err)
	}

	out, closeOut, err := e.exportTarget(*outPath)
	if err != nil {
		return e.fail(err)
	}

	result, err := export.Write(ctx, db, account.ID, out, export.Options{
		Family:         *family,
		Format:         encoding,
		Flat:           *flat,
		FromDate:       formatDate(window.From),
		ToDate:         formatDate(window.To),
		IncludeDeleted: *includeDeleted,
	})
	if closeErr := closeOut(); err == nil {
		err = closeErr
	}
	if err != nil {
		return e.fail(err)
	}

	// Progress goes to stderr so it never lands in a redirected export.
	fprintf(e.err, "wrote %d %s records", result.Records, *family)
	if result.Fields > 0 {
		fprintf(e.err, " across %d columns", result.Fields)
	}
	if *outPath != "" {
		fprintf(e.err, " to %s", *outPath)
	}
	fprintln(e.err)

	if *flat {
		fprintln(e.err,
			"note: --flat resolves references and drops the URLs, so it is not a backup")
	}
	return exitOK
}

// exportTarget opens the destination, buffered, and returns a closer that
// flushes it. Writing to a file is 0600 like everything else here: an export
// of the books is as sensitive as the archive it came from.
func (e *env) exportTarget(path string) (out *bufio.Writer, closeOut func() error, err error) {
	if path == "" {
		buffered := bufio.NewWriter(e.out)
		return buffered, buffered.Flush, nil
	}

	// The path is where the operator asked their own export to go.
	//nolint:gosec // G304: the destination is operator-supplied by design
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	buffered := bufio.NewWriter(file)
	return buffered, func() error {
		if err := buffered.Flush(); err != nil {
			_ = file.Close()
			return err
		}
		return file.Close()
	}, nil
}

// cmdReproject rebuilds everything derived from the archive: the exact numeric
// projection, the JSON record tree and the browsable file views. Offline, so it
// costs nothing and can be run whenever the derived state looks wrong.
func cmdReproject(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("reproject", e)
	e.g.register(fs)
	if _, err := e.parse(fs, args); err != nil {
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

	numbers, err := db.ProjectNumbers(ctx, account.ID)
	if err != nil {
		return e.fail(err)
	}
	fprintf(e.out, "projected %d numeric values from %d records\n",
		numbers.Values, numbers.Records)
	if numbers.Inexact > 0 {
		// Their text is still exact; only the integer column is NULL. Saying so
		// is better than letting someone assume every row can be summed.
		fprintf(e.out,
			"%d values need more than %d decimal places, so value_e6 is NULL for them\n",
			numbers.Inexact, 6)
	}

	e.rebuildTrees(ctx, db, account.ID, cfg)
	return exitOK
}
