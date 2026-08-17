package main

import (
	"context"
	"strings"
	"time"

	"github.com/alekc/freeagent-sync/internal/engine"
	"github.com/alekc/freeagent-sync/internal/store"
)

// cmdDocs renders the documents FreeAgent generates for sales records.
//
// Separate from `pull` and off by default there, because unlike attachments
// these come from the API and cost one request each. A company with a thousand
// invoices is a thousand requests, which is a deliberate act rather than
// something a routine sync should do behind your back.
func cmdDocs(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 || isHelp(args[0]) {
		fprintln(e.out, "Usage: fasync docs render [flags]")
		fprintf(e.out, "\nRenders the PDF for %s.\n", strings.Join(engine.PDFFamilies, ", "))
		fprintln(e.out, "One API request per document, so it is incremental:")
		fprintln(e.out, "only records that changed since their last render are fetched.")
		return exitOK
	}
	if args[0] != "render" {
		fprintf(e.err, "fasync: unknown docs subcommand %q\n", args[0])
		return exitConfig
	}
	return docsRender(ctx, e, args[1:])
}

func docsRender(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("docs render", e)
	e.g.register(fs)
	families := fs.String("family", "",
		"comma-separated families to render (default: "+
			strings.Join(engine.PDFFamilies, ", ")+")")
	limit := fs.Int("limit", 0, "stop after this many documents (0: all outstanding)")
	maxRequests := fs.Int64("max-requests", 0, "stop after this many API calls")
	maxDuration := fs.Duration("max-duration", 0, "stop after this long")
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	session, code := e.openSession(ctx)
	if session == nil {
		return code
	}
	defer session.Close()

	blobs, err := openBlobStore(session.cfg)
	if err != nil {
		return e.fail(err)
	}

	opts := engine.RenderOptions{
		Families:    splitList(*families),
		Limit:       *limit,
		MaxRequests: *maxRequests,
	}
	if *maxDuration > 0 {
		opts.Deadline = time.Now().Add(*maxDuration)
	}

	result, err := session.engine.RenderDocuments(ctx, blobs, opts)
	if err != nil {
		return e.fail(err)
	}

	fprintf(e.out, "rendered %d documents (%s), %d failed\n",
		result.Rendered, humanBytes(result.Bytes), result.Failed)
	for _, err := range result.Errs {
		fprintf(e.err, "  %v\n", err)
	}
	if result.Remaining > 0 {
		fprintf(e.out, "%d still outstanding; run again to continue\n", result.Remaining)
	}

	total, err := session.db.DocumentCount(ctx, session.account.ID, store.DocumentKindPDF)
	if err != nil {
		return e.fail(err)
	}
	fprintf(e.out, "\n%d documents stored in total\n", total)

	if result.Failed > 0 {
		return exitPartial
	}
	return exitOK
}
