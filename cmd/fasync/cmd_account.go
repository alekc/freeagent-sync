package main

import (
	"context"

	"github.com/alekc/freeagent"
	"github.com/jedib0t/go-pretty/v6/table"
)

func cmdAccount(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 || isHelp(args[0]) {
		fprintln(e.out, "Usage: fasync account <add|list|remove> [flags]")
		return exitOK
	}
	switch args[0] {
	case "add":
		return accountAdd(ctx, e, args[1:])
	case "list":
		return accountList(ctx, e, args[1:])
	case "remove":
		return accountRemove(ctx, e, args[1:])
	}
	fprintf(e.err, "fasync: unknown account subcommand %q\n", args[0])
	return exitConfig
}

func accountAdd(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("account add", e)
	e.g.register(fs)
	environment := fs.String("env", freeagent.Sandbox.Name,
		"FreeAgent deployment: sandbox or production")
	name := fs.String("name", "", "human-readable label for the company")
	positional, err := e.parse(fs, args)
	if err != nil {
		return e.fail(err)
	}
	if len(positional) != 1 {
		fprintln(e.err, "Usage: fasync account add <slug> [-env sandbox|production]")
		return exitConfig
	}
	slug := positional[0]

	// The SDK owns the list of deployments, so resolve rather than accepting
	// any string and failing later against a URL that does not exist.
	if _, err := freeagent.EnvironmentByName(*environment); err != nil {
		return e.fail(err)
	}

	_, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	account, err := db.AddAccount(ctx, slug, *name, *environment)
	if err != nil {
		return e.fail(err)
	}

	fprintf(e.out, "added %s (%s)\n", account.Slug, account.Environment)
	fprintf(e.out, "\nNext: fasync auth login -account %s\n", account.Slug)
	return exitOK
}

func accountList(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("account list", e)
	e.g.register(fs)
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	_, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	accounts, err := db.Accounts(ctx)
	if err != nil {
		return e.fail(err)
	}
	if len(accounts) == 0 {
		fprintln(e.out, "no accounts configured; add one with: fasync account add <slug>")
		return exitOK
	}

	t := newTable(e)
	t.AppendHeader(table.Row{"Slug", "Name", "Environment", "Company", "Added"})
	for _, a := range accounts {
		t.AppendRow(table.Row{
			a.Slug, or(a.Name, "-"), a.Environment,
			or(a.CompanyURL, "not yet seen"),
			a.CreatedAt.Local().Format("2006-01-02"),
		})
	}
	t.Render()
	return exitOK
}

func accountRemove(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("account remove", e)
	e.g.register(fs)
	positional, err := e.parse(fs, args)
	if err != nil {
		return e.fail(err)
	}
	if len(positional) != 1 {
		fprintln(e.err, "Usage: fasync account remove <slug>")
		return exitConfig
	}

	_, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	if err := db.RemoveAccount(ctx, positional[0]); err != nil {
		return e.fail(err)
	}
	fprintf(e.out, "removed %s\n", positional[0])
	return exitOK
}

func newTable(e *env) table.Writer {
	t := table.NewWriter()
	t.SetOutputMirror(e.out)
	t.SetStyle(table.StyleLight)
	return t
}

func or(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
