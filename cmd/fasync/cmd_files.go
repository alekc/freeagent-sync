package main

import (
	"context"
	"time"

	"github.com/alekc/freeagent-sync/internal/blob"
	"github.com/alekc/freeagent-sync/internal/config"
	"github.com/alekc/freeagent-sync/internal/engine"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/tree"
)

func cmdBlobs(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 || isHelp(args[0]) {
		fprintln(e.out, "Usage: fasync blobs <fetch|verify> [flags]")
		return exitOK
	}
	switch args[0] {
	case "fetch":
		return blobsFetch(ctx, e, args[1:])
	case "verify":
		return blobsVerify(ctx, e, args[1:])
	}
	fprintf(e.err, "fasync: unknown blobs subcommand %q\n", args[0])
	return exitConfig
}

func blobsFetch(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("blobs fetch", e)
	e.g.register(fs)
	concurrency := fs.Int("concurrency", engine.DefaultBlobConcurrency,
		"how many attachments to download at once")
	limit := fs.Int("limit", 0, "stop after this many attachments (0: all outstanding)")
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

	opts := engine.BlobOptions{Concurrency: *concurrency, Limit: *limit}
	if *maxDuration > 0 {
		opts.Deadline = time.Now().Add(*maxDuration)
	}

	result, err := session.engine.FetchBlobs(ctx, blobs, opts)
	if err != nil {
		return e.fail(err)
	}
	return e.reportBlobs(ctx, session, result)
}

func (e *env) reportBlobs(
	ctx context.Context, session *session, result engine.BlobResult,
) int {
	fprintf(e.out, "downloaded %d attachments (%s), %d failed, %d skipped\n",
		result.Stored, humanBytes(result.Bytes), result.Failed, result.Skipped)

	for _, err := range result.Errs {
		fprintf(e.err, "  %v\n", err)
	}

	counts, err := session.db.AttachmentCounts(ctx, session.account.ID)
	if err != nil {
		return e.fail(err)
	}
	fprintf(e.out, "\n%d of %d attachments stored, %s on disk\n",
		counts.Stored, counts.Total, humanBytes(counts.Bytes))
	if counts.Pending+counts.Failed > 0 {
		fprintf(e.out, "%d still outstanding; run again to retry\n",
			counts.Pending+counts.Failed)
	}

	if result.Failed > 0 {
		return exitPartial
	}
	return exitOK
}

// blobsVerify re-hashes stored blobs. Comparing them against anything else
// kept alongside them would only prove two copies agree, so this recomputes.
func blobsVerify(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("blobs verify", e)
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
	blobs, err := openBlobStore(cfg)
	if err != nil {
		return e.fail(err)
	}

	stored, err := db.StoredAttachments(ctx, account.ID)
	if err != nil {
		return e.fail(err)
	}

	seen := make(map[string]bool, len(stored))
	var checked, bad int
	for _, att := range stored {
		if seen[att.SHA256] {
			continue
		}
		seen[att.SHA256] = true
		checked++
		if err := blobs.Verify(att.SHA256); err != nil {
			bad++
			fprintf(e.err, "  %s (%s): %v\n", att.FileName, att.URL, err)
		}
	}

	fprintf(e.out, "verified %d distinct blobs, %d failed\n", checked, bad)
	if bad > 0 {
		return exitPartial
	}
	return exitOK
}

func cmdFiles(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 || isHelp(args[0]) {
		fprintln(e.out, "Usage: fasync files <rebuild|relink> [flags]")
		return exitOK
	}
	switch args[0] {
	case "rebuild":
		return filesRebuild(ctx, e, args[1:])
	case "relink":
		return filesRelink(ctx, e, args[1:])
	}
	fprintf(e.err, "fasync: unknown files subcommand %q\n", args[0])
	return exitConfig
}

// filesRebuild regenerates the JSON record tree. Offline: it reads only the
// archive, so it needs no credentials and costs no rate budget.
func filesRebuild(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("files rebuild", e)
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

	stats, err := tree.BuildRecords(ctx, db, account.ID, cfg.RecordsDir)
	if err != nil {
		return e.fail(err)
	}
	fprintf(e.out, "wrote %d records and %d versions to %s\n",
		stats.Records, stats.Versions, cfg.RecordsDir)
	return exitOK
}

func filesRelink(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("files relink", e)
	e.g.register(fs)
	mode := fs.String("link-mode", "auto",
		"how to point at blobs: hardlink, symlink, copy or auto")
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	linkMode, err := tree.ParseLinkMode(*mode)
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
	blobs, err := openBlobStore(cfg)
	if err != nil {
		return e.fail(err)
	}

	stats, err := tree.BuildFiles(ctx, db, blobs, account.ID, cfg.FilesDir, linkMode)
	if err != nil {
		return e.fail(err)
	}

	fprintf(e.out, "made %d %s entries in %s\n", stats.Links, stats.Mode, cfg.FilesDir)
	if stats.Missing > 0 {
		fprintf(e.out, "%d attachments are recorded as stored but their bytes are missing; "+
			"run: fasync blobs fetch\n", stats.Missing)
	}
	return exitOK
}

func openBlobStore(cfg *config.Config) (*blob.Store, error) {
	return blob.NewStore(cfg.BlobsDir, cfg.TmpDir)
}

// derivedAfterPull regenerates everything derived at the end of a run, so a
// pull leaves the projections and browsable copies matching the archive it just
// updated.
func (e *env) derivedAfterPull(ctx context.Context, s *session) {
	if _, err := s.db.ProjectNumbers(ctx, s.account.ID); err != nil {
		fprintf(e.err, "warning: rebuilding the numeric projection: %v\n", err)
	}
	e.rebuildTrees(ctx, s.db, s.account.ID, s.cfg)
}

// rebuildTrees regenerates the JSON record tree and the symlink views. Both are
// derived, so a failure is a warning: the archive itself is unaffected.
func (e *env) rebuildTrees(
	ctx context.Context, db *store.DB, accountID int64, cfg *config.Config,
) {
	records, err := tree.BuildRecords(ctx, db, accountID, cfg.RecordsDir)
	if err != nil {
		fprintf(e.err, "warning: rebuilding the record tree: %v\n", err)
		return
	}
	blobs, err := openBlobStore(cfg)
	if err != nil {
		fprintf(e.err, "warning: opening the blob store: %v\n", err)
		return
	}
	files, err := tree.BuildFiles(ctx, db, blobs, accountID, cfg.FilesDir, "")
	if err != nil {
		fprintf(e.err, "warning: relinking the file tree: %v\n", err)
		return
	}
	fprintf(e.out, "rebuilt %d record files and %d %s entries\n",
		records.Records, files.Links, files.Mode)
}
