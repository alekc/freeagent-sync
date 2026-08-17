package main

import (
	"context"
	"os"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/auth"
)

func cmdAuth(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 || isHelp(args[0]) {
		fprintln(e.out, "Usage: fasync auth <login|status> [flags]")
		return exitOK
	}
	switch args[0] {
	case "login":
		return authLogin(ctx, e, args[1:])
	case "status":
		return authStatus(ctx, e, args[1:])
	}
	fprintf(e.err, "fasync: unknown auth subcommand %q\n", args[0])
	return exitConfig
}

func authLogin(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("auth login", e)
	e.g.register(fs)
	manual := fs.Bool("manual", false,
		"paste the authorisation code instead of listening for the callback")
	redirect := fs.String("redirect-uri", "",
		"OAuth redirect URI (default: $"+auth.EnvRedirectURI+" or "+auth.DefaultRedirectURI+")")
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	cfg, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	if err := cfg.RequireCredentials(); err != nil {
		return e.fail(err)
	}
	account, err := e.resolveAccount(ctx, db)
	if err != nil {
		return e.fail(err)
	}
	environment, err := freeagent.EnvironmentByName(account.Environment)
	if err != nil {
		return e.fail(err)
	}

	authCfg := e.authConfig(cfg, *account, environment)
	authCfg.RedirectURI = *redirect

	result, err := auth.Login(ctx, authCfg, auth.LoginOptions{
		Manual: *manual,
		In:     os.Stdin,
		Out:    e.out,
	})
	if err != nil {
		return e.fail(err)
	}

	fprintf(e.out, "\nStored in %s under %q.\nAccess token expires in %s.\n",
		result.TokenPath, account.Slug, result.ExpiresIn)
	fprintf(e.out, "\nNext: fasync probe, then fasync pull\n")
	return exitOK
}

func authStatus(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("auth status", e)
	e.g.register(fs)
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	cfg, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	if err := cfg.RequireCredentials(); err != nil {
		return e.fail(err)
	}
	accounts, err := db.Accounts(ctx)
	if err != nil {
		return e.fail(err)
	}
	if len(accounts) == 0 {
		fprintln(e.out, "no accounts configured; add one with: fasync account add <slug>")
		return exitOK
	}

	missing := 0
	for _, account := range accounts {
		environment, err := freeagent.EnvironmentByName(account.Environment)
		if err != nil {
			return e.fail(err)
		}
		info, err := auth.Status(ctx, e.authConfig(cfg, account, environment))
		if err != nil {
			return e.fail(err)
		}

		fprintf(e.out, "%-16s %-11s ", info.Key, info.Environment)
		switch {
		case !info.HasToken:
			fprintf(e.out, "no token; run: fasync auth login -account %s\n", info.Key)
			missing++
		case !info.HasRefresh:
			// Without a refresh token the credential dies at the first
			// expiry, which for a cron job means silently at 3am.
			fprintf(e.out, "expires in %s, but has no refresh token\n", info.ExpiresIn)
			missing++
		default:
			fprintf(e.out, "ok, expires in %s\n", info.ExpiresIn)
		}
	}

	fprintf(e.out, "\nTokens: %s\n", cfg.TokenFile)
	if missing > 0 {
		return exitConfig
	}
	return exitOK
}
