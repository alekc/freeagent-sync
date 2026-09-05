// Command famcp serves the FreeAgent ledger to MCP clients over stdio.
//
// It reads the live API through the same read-only client the archive uses, so
// there is no path here that can write to the company. It does not take the
// archive lock either, because it does not write to the archive: a famcp
// session and a `fasync pull` can run at the same time.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alekc/freeagent"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alekc/freeagent-sync/internal/api"
	"github.com/alekc/freeagent-sync/internal/auth"
	"github.com/alekc/freeagent-sync/internal/config"
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
	)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(),
			"Usage: famcp [flags]\n\n"+
				"Serves the FreeAgent ledger to MCP clients over stdio. Read-only.\n\n"+
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

	client, acct, closeArchive, err := connect(ctx, config.Flags{
		DataDir: *dataDir, TokenFile: *tokenFile,
	}, *account, *perMinute, *perHour)
	if err != nil {
		return err
	}
	defer closeArchive()

	// A terminated client should end the process, not leave it holding an
	// open token source and a half-read response.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := newServer(client, acct)
	fmt.Fprintf(os.Stderr, "famcp: serving %s (%s) on stdio\n", acct.Slug, acct.Environment)
	return server.Run(ctx, &mcp.StdioTransport{})
}

// connect resolves configuration, finds the account and builds the read-only
// client. The archive is opened only to look the account up: famcp reads the
// API, not the records.
func connect(
	ctx context.Context, flags config.Flags, slug string, perMinute, perHour int,
) (*api.Client, store.Account, func(), error) {
	var none store.Account

	cfg, err := config.Load(flags)
	if err != nil {
		return nil, none, nil, err
	}
	if err := cfg.RequireCredentials(); err != nil {
		return nil, none, nil, err
	}

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, none, nil, err
	}
	closeArchive := func() { _ = db.Close() }

	account, err := resolveAccount(ctx, db, slug)
	if err != nil {
		closeArchive()
		return nil, none, nil, err
	}

	environment, err := freeagent.EnvironmentByName(account.Environment)
	if err != nil {
		closeArchive()
		return nil, none, nil, err
	}
	source, err := auth.Source(ctx, auth.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Environment:  environment,
		TokenFile:    cfg.TokenFile,
		Key:          account.Slug,
	})
	if err != nil {
		closeArchive()
		// The token store is written by a native `fasync auth login`, which is
		// the one thing a containerised wrapper cannot do for you.
		return nil, none, nil, fmt.Errorf(
			"%w; sign in first with: fasync auth login", err)
	}

	client, err := api.NewReadOnly(api.Options{
		Environment:       environment,
		TokenSource:       source,
		UserAgent:         userAgent,
		RequestsPerMinute: perMinute,
		RequestsPerHour:   perHour,
	})
	if err != nil {
		closeArchive()
		return nil, none, nil, err
	}
	return client, *account, closeArchive, nil
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
