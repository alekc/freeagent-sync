// Command famcp serves the FreeAgent ledger to MCP clients over stdio.
//
// It reads the live API through the same read-only client the archive uses. It
// does not take the archive lock, because it does not write to the archive: a
// famcp session and a `fasync pull` can run at the same time.
//
// By default nothing here can write to the company. The -allow-writes flag
// adds a second client, from internal/explain, which can do exactly two
// things: explain a bank transaction that still has an unexplained balance,
// and add a file to an explanation. Neither can change a value already
// recorded, explaining because it re-reads the transaction first and refuses
// unless a balance is still open, attaching because it appends to a
// sub-resource whose replace and delete operations no code here reaches. Every
// attempt is appended to an audit file. Without the flag neither tool is
// registered, so the model is not offered a capability that would then be
// refused.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/alekc/freeagent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alekc/freeagent-sync/internal/api"
	"github.com/alekc/freeagent-sync/internal/auth"
	"github.com/alekc/freeagent-sync/internal/config"
	"github.com/alekc/freeagent-sync/internal/explain"
	"github.com/alekc/freeagent-sync/internal/store"
)

// Version is overridden at build time.
var Version = "dev"

// userAgent identifies this server to FreeAgent. Distinct from fasync's, so a
// rate-limit conversation with support can tell the two apart.
var userAgent = "famcp/" + Version + " (+https://github.com/alekc/freeagent-sync)"

// Half the SDK's own budget. An MCP session is interactive and bursty, and it
// shares the company's rate allowance with any scheduled pull, so famcp takes
// the smaller half rather than racing it.
const (
	defaultPerMinute = freeagent.DefaultRequestsPerMinute / 2
	defaultPerHour   = freeagent.DefaultRequestsPerHour / 2
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		// stdout belongs to the transport, so every diagnostic goes to stderr.
		fmt.Fprintf(os.Stderr, "famcp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("famcp", flag.ContinueOnError)
	var (
		account   = fs.String("account", "", "account slug (default: the only one configured)")
		dataDir   = fs.String("data-dir", "", "archive directory")
		tokenFile = fs.String("token-file", "", "OAuth token store")
		perMinute = fs.Int("requests-per-minute", defaultPerMinute, "client-side rate budget")
		perHour   = fs.Int("requests-per-hour", defaultPerHour, "client-side rate budget")
		showVer   = fs.Bool("version", false, "print the version and exit")

		allowWrites = fs.Bool("allow-writes", false,
			"allow explaining unexplained bank transactions and attaching "+
				"missing receipts; nothing already explained or attached can "+
				"be changed, and every attempt is recorded to writes.jsonl")
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(),
			"Usage: famcp [flags]\n\n"+
				"Serves the FreeAgent ledger to MCP clients over stdio. Read-only\n"+
				"unless -allow-writes is given.\n\n"+
				"Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVer {
		fmt.Fprintln(os.Stderr, userAgent)
		return nil
	}

	s, err := connect(ctx, config.Flags{
		DataDir: *dataDir, TokenFile: *tokenFile,
	}, *account, *perMinute, *perHour, *allowWrites)
	if err != nil {
		return err
	}
	defer s.close()

	// A terminated client should end the process, not leave it holding an
	// open token source and a half-read response.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := newServer(s.client, s.writer, s.account)
	fmt.Fprintf(os.Stderr, "famcp: serving %s (%s) on stdio\n",
		s.account.Slug, s.account.Environment)
	if s.writer != nil {
		// Said plainly and at every start, because the difference between this
		// mode and the default is the difference between reading a company's
		// ledger and changing it.
		fmt.Fprintf(os.Stderr,
			"famcp: writes are ENABLED for %s (%s). Only unexplained "+
				"transactions can be explained and only missing receipts "+
				"attached; nothing already recorded can be changed. Every "+
				"attempt is appended to %s\n",
			s.account.Slug, s.account.Environment, s.writer.AuditPath())
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

// session is what connect resolves: the read client, the write client when it
// was asked for, the account both are pinned to, and the archive handle to
// release.
type session struct {
	client  *api.Client
	writer  *explain.Client
	account store.Account
	close   func()
}

// connect resolves configuration, finds the account and builds the read client,
// plus the write one when allowWrites asked for it. The archive is opened only
// to look the account up: famcp reads the API, not the records.
func connect(
	ctx context.Context, flags config.Flags, slug string,
	perMinute, perHour int, allowWrites bool,
) (*session, error) {
	cfg, err := config.Load(flags)
	if err != nil {
		return nil, err
	}
	if err := cfg.RequireCredentials(); err != nil {
		return nil, err
	}

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}
	closeArchive := func() { _ = db.Close() }
	fail := func(err error) (*session, error) {
		closeArchive()
		return nil, err
	}

	account, err := resolveAccount(ctx, db, slug)
	if err != nil {
		return fail(err)
	}

	environment, err := freeagent.EnvironmentByName(account.Environment)
	if err != nil {
		return fail(err)
	}
	source, err := auth.Source(ctx, auth.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Environment:  environment,
		TokenFile:    cfg.TokenFile,
		Key:          account.Slug,
	})
	if err != nil {
		// The token store is written by a native `fasync auth login`, which is
		// the one thing a containerised wrapper cannot do for you.
		return fail(fmt.Errorf("%w; sign in first with: fasync auth login", err))
	}

	client, err := api.NewReadOnly(api.Options{
		Environment:       environment,
		TokenSource:       source,
		UserAgent:         userAgent,
		RequestsPerMinute: perMinute,
		RequestsPerHour:   perHour,
	})
	if err != nil {
		return fail(err)
	}

	s := &session{client: client, account: *account, close: closeArchive}
	if !allowWrites {
		return s, nil
	}

	// The writable client is built only on request, and it is a different
	// type from the read one, so nothing downstream can mistake one for the
	// other. The audit file sits beside the archive it describes.
	s.writer, err = explain.New(explain.Options{
		Environment:       environment,
		TokenSource:       source,
		UserAgent:         userAgent,
		RequestsPerMinute: perMinute,
		RequestsPerHour:   perHour,
		Account:           account.Slug,
		AuditPath:         filepath.Join(cfg.DataDir, "writes.jsonl"),
	})
	if err != nil {
		return fail(err)
	}
	return s, nil
}

func resolveAccount(
	ctx context.Context, db *store.DB, slug string,
) (*store.Account, error) {
	if slug != "" {
		return db.AccountBySlug(ctx, slug)
	}
	account, err := db.OnlyAccount(ctx)
	if errors.Is(err, store.ErrNoSuchAccount) {
		return nil, fmt.Errorf("%w; add one with: fasync account add <slug>", err)
	}
	return account, err
}
