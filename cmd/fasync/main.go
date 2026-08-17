// Command fasync mirrors a FreeAgent company into a local SQLite archive with
// its attachments, and keeps that copy current.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/alekc/freeagent-sync/internal/config"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// Version is stamped at build time; see the Makefile.
var Version = "dev"

// Exit codes. Distinct so a cron job can tell a broken run from one flaky
// family, rather than reading every line of the mail it just sent.
const (
	exitOK       = 0
	exitPartial  = 1
	exitConfig   = 2
	exitLockHeld = 3
	exitBudget   = 4
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	// Not deferred: os.Exit does not run deferred functions.
	stop()
	os.Exit(code)
}

// env bundles what a command needs, so each one stays testable without
// reaching for globals.
type env struct {
	out io.Writer
	err io.Writer
	g   globals
}

type commandFunc func(context.Context, *env, []string) int

var commands = map[string]struct {
	summary string
	run     commandFunc
}{
	"init":      {"create the archive and its directories", cmdInit},
	"account":   {"add, list and remove accounts", cmdAccount},
	"auth":      {"authorise an account and check its token", cmdAuth},
	"probe":     {"establish which families honour updated_since", cmdProbe},
	"families":  {"list what is archived and what is not", cmdFamilies},
	"pull":      {"archive changes since the last run", cmdPull},
	"reconcile": {"read everything and mark what the far end no longer has", cmdReconcile},
	"blobs":     {"download and verify attachments", cmdBlobs},
	"docs":      {"render the PDFs FreeAgent generates", cmdDocs},
	"export":    {"write a family out as CSV or JSON", cmdExport},
	"reproject": {"rebuild everything derived from the archive", cmdReproject},
	"files":     {"regenerate the record and file trees", cmdFiles},
	"verify":    {"check the archive against itself and the accounts", cmdVerify},
	"sql":       {"run a read-only query against the archive", cmdSQL},
	"status":    {"what is configured and what has run", cmdStatus},
	"version":   {"print the version", cmdVersion},
}

func run(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || isHelp(args[0]) {
		usage(out)
		return exitOK
	}

	cmd, ok := commands[args[0]]
	if !ok {
		fprintf(errOut, "fasync: unknown command %q\n\n", args[0])
		usage(errOut)
		return exitConfig
	}

	e := &env{out: out, err: errOut}
	return cmd.run(ctx, e, args[1:])
}

func isHelp(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "help"
}

func usage(w io.Writer) {
	fprintf(w, "fasync %s: mirror a FreeAgent company locally\n\n", Version)
	fprintln(w, "Usage: fasync <command> [flags]")
	fprintln(w, "\nCommands:")
	for _, name := range sortedCommands() {
		fprintf(w, "  %-10s %s\n", name, commands[name].summary)
	}
	fprintln(w, "\nRun 'fasync <command> -h' for a command's flags.")
}

func sortedCommands() []string { return sortedKeys(commands) }

// sortedKeys gives map-backed output a stable order, so successive runs are
// diffable instead of shuffling.
func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// globals are the flags every command accepts. Registered per subcommand so
// they can be written after the verb, which is where people put them.
type globals struct {
	dataDir   string
	dbPath    string
	blobsDir  string
	tokenFile string
	account   string
	progress  string
	logFormat string
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.dataDir, "data-dir", "",
		"where the archive lives (default $XDG_DATA_HOME/freeagent-sync)")
	fs.StringVar(&g.dbPath, "db", "", "path to the SQLite archive")
	fs.StringVar(&g.blobsDir, "blobs-dir", "", "path to the content-addressed blob store")
	fs.StringVar(&g.tokenFile, "token-file", "", "path to the OAuth token store")
	fs.StringVar(&g.account, "account", "", "account slug, unless only one is configured")
	fs.StringVar(&g.progress, "progress", "auto", "progress display: auto, always or never")
	fs.StringVar(&g.logFormat, "log-format", "text", "log format: text or json")
}

// validate checks the flags whose values are a fixed set. Done at parse time
// so a typo is reported as a typo, rather than surfacing later behind
// whatever environment problem happens to be checked first.
func (g *globals) validate() error {
	if _, err := ui.ParseMode(g.progress); err != nil {
		return err
	}
	if g.logFormat != "text" && g.logFormat != "json" {
		return fmt.Errorf("fasync: unknown log format %q, want text or json", g.logFormat)
	}
	return nil
}

func (g *globals) logger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if g.logFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// reporter builds the progress display. Errors here are configuration errors,
// so they surface before any work starts.
func (g *globals) reporter(w io.Writer) (ui.Reporter, error) {
	mode, err := ui.ParseMode(g.progress)
	if err != nil {
		return nil, err
	}
	return ui.New(mode, w, ui.SlogLogger(g.logger(w)))
}

func (g *globals) config() (*config.Config, error) {
	return config.Load(config.Flags{
		DataDir:   g.dataDir,
		DBPath:    g.dbPath,
		BlobsDir:  g.blobsDir,
		TokenFile: g.tokenFile,
	})
}

// newFlagSet returns a flag set that reports its errors through the command's
// own stderr rather than the process-wide default.
func newFlagSet(name string, e *env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.err)
	return fs
}

// parse handles a command's arguments and validates the global flags, which
// is the only entry point commands should use.
func (e *env) parse(fs *flag.FlagSet, args []string) ([]string, error) {
	positional, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	return positional, e.g.validate()
}

// parseArgs parses flags that appear before, between, or after positional
// arguments. The stdlib flag package stops at the first non-flag argument,
// which would silently ignore "fasync account add acme -env production".
// Same helper, same behaviour, as facli.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// fail prints an error and maps it to an exit code, so every command reports
// the same way.
func (e *env) fail(err error) int {
	fprintf(e.err, "fasync: %v\n", err)
	switch {
	case errors.Is(err, store.ErrNoSuchAccount):
		return exitConfig
	default:
		return exitConfig
	}
}

// openArchive resolves configuration and opens the archive, the two steps
// every stateful command starts with.
func (e *env) openArchive(ctx context.Context) (*config.Config, *store.DB, error) {
	cfg, err := e.g.config()
	if err != nil {
		return nil, nil, err
	}
	db, err := e.openArchiveAt(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return cfg, db, nil
}

// openArchiveAt opens the archive for an already-resolved configuration.
func (e *env) openArchiveAt(ctx context.Context, cfg *config.Config) (*store.DB, error) {
	return store.Open(ctx, cfg.DBPath)
}

// fprintf and fprintln drop the write error deliberately. Output going to a
// terminal or a pipe that has gone away is not something a command can act
// on, and checking at every call site buries the logic that matters.
func fprintf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func fprintln(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

func cmdVersion(_ context.Context, e *env, _ []string) int {
	fprintln(e.out, Version)
	return exitOK
}
