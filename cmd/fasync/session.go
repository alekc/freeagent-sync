package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/api"
	"github.com/alekc/freeagent-sync/internal/auth"
	"github.com/alekc/freeagent-sync/internal/config"
	"github.com/alekc/freeagent-sync/internal/engine"
	"github.com/alekc/freeagent-sync/internal/lock"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// userAgent identifies this tool to FreeAgent. Built from version(), not from
// Version, so a `go install` build does not announce itself as "dev".
var userAgent = "fasync/" + version() + " (+https://github.com/alekc/freeagent-sync)"

// session is everything a command that talks to the API needs, assembled in
// one place so pull, reconcile and probe cannot drift apart.
type session struct {
	cfg     *config.Config
	db      *store.DB
	account store.Account
	client  *api.Client
	report  ui.Reporter
	engine  *engine.Engine
	held    *lock.Lock
}

// Close releases everything in the reverse of the order it was taken.
func (s *session) Close() {
	if s.report != nil {
		s.report.Close()
	}
	if s.held != nil {
		_ = s.held.Release()
	}
	if s.db != nil {
		_ = s.db.Close()
	}
}

// openSession resolves configuration, takes the run lock, opens the archive
// and builds a read-only client. It returns nil and an exit code on failure,
// so callers stay a single line.
func (e *env) openSession(ctx context.Context) (*session, int) {
	cfg, err := e.g.config()
	if err != nil {
		return nil, e.fail(err)
	}
	if err := cfg.RequireCredentials(); err != nil {
		return nil, e.fail(err)
	}

	// Taken before the archive is opened. Two overlapping runs would corrupt
	// cursors and the partial downloads in tmp/, neither of which any
	// transaction covers.
	held, err := lock.Acquire(cfg.LockPath)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			fprintf(e.err, "fasync: %v\n", err)
			return nil, exitLockHeld
		}
		return nil, e.fail(err)
	}

	s := &session{cfg: cfg, held: held}
	if s.db, err = store.Open(ctx, cfg.DBPath); err != nil {
		s.Close()
		return nil, e.fail(err)
	}

	account, err := e.resolveAccount(ctx, s.db)
	if err != nil {
		s.Close()
		return nil, e.fail(err)
	}
	s.account = *account

	if s.client, err = e.buildClient(ctx, cfg, *account); err != nil {
		s.Close()
		return nil, e.fail(err)
	}
	if s.report, err = e.g.reporter(e.err); err != nil {
		s.Close()
		return nil, e.fail(err)
	}

	s.engine = engine.New(s.db, s.client, s.report, *account)
	return s, exitOK
}

// buildClient returns a client that cannot write. There is no variant of this
// function that returns a writable one, which is the whole guarantee.
func (e *env) buildClient(
	ctx context.Context, cfg *config.Config, account store.Account,
) (*api.Client, error) {
	environment, err := freeagent.EnvironmentByName(account.Environment)
	if err != nil {
		return nil, err
	}
	source, err := auth.Source(ctx, e.authConfig(cfg, account, environment))
	if err != nil {
		return nil, err
	}
	return api.NewReadOnly(api.Options{
		Environment: environment,
		TokenSource: source,
		UserAgent:   userAgent,
	})
}

func (e *env) authConfig(
	cfg *config.Config, account store.Account, environment freeagent.Environment,
) auth.Config {
	return auth.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Environment:  environment,
		TokenFile:    cfg.TokenFile,
		Key:          account.Slug,
	}
}

// resolveAccount picks the account a command acts on: the one named by
// --account, or the only one configured.
func (e *env) resolveAccount(ctx context.Context, db *store.DB) (*store.Account, error) {
	if e.g.account != "" {
		return db.AccountBySlug(ctx, e.g.account)
	}
	account, err := db.OnlyAccount(ctx)
	if errors.Is(err, store.ErrNoSuchAccount) {
		return nil, fmt.Errorf("%w; add one with: fasync account add <slug>", err)
	}
	return account, err
}
