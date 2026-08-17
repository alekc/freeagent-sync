package main

import (
	"context"

	"github.com/jedib0t/go-pretty/v6/table"

	"github.com/alekc/freeagent-sync/internal/engine"
)

func cmdProbe(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("probe", e)
	e.g.register(fs)
	families := fs.String("family", "", "comma-separated families to probe (default: all)")
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	session, code := e.openSession(ctx)
	if session == nil {
		return code
	}
	defer session.Close()

	results, err := session.engine.Probe(ctx, splitList(*families))
	if err != nil {
		return e.fail(err)
	}

	t := newTable(e)
	t.AppendHeader(table.Row{"Family", "updated_since", "Detail"})

	counts := map[string]int{}
	for _, r := range results {
		// The families with nothing to filter are counted rather than listed:
		// a row per singleton saying "not applicable" is noise.
		if r.Result != engine.ProbeNotApplicable {
			t.AppendRow(table.Row{r.Family, r.Result, r.Detail})
		}
		counts[r.Result]++
	}
	t.Render()

	fprintf(e.out, "\n%d honour updated_since, %d ignore it.\n",
		counts[engine.ProbeHonoured], counts[engine.ProbeIgnored])
	if counts[engine.ProbeIgnored] > 0 {
		fprintf(e.out, "The ones that ignore it are read in full from now on.\n")
	}
	if n := counts[engine.ProbeNoData]; n > 0 {
		fprintf(e.out, "%d have no records yet, so they stay unknown and are read in full.\n", n)
	}
	if n := counts[engine.ProbeNotApplicable]; n > 0 {
		fprintf(e.out, "%d are singletons, reports or year-addressed, with nothing to filter.\n", n)
	}
	if n := counts[engine.ProbeUnavailable]; n > 0 {
		fprintf(e.out, "%d are not available to this company.\n", n)
	}
	if counts[engine.ProbeFailed] > 0 {
		fprintf(e.out, "%d probes failed unexpectedly.\n", counts[engine.ProbeFailed])
		return exitPartial
	}
	return exitOK
}

// coverageOf describes what happens to a family this build does not read
// directly. Reporting "not yet" for attachments would be wrong: they are
// archived, just through the records that carry them.
func coverageOf(class engine.Class) string {
	if class == engine.ClassChildOnly {
		return "via parents"
	}
	return "not yet"
}

func cmdFamilies(_ context.Context, e *env, args []string) int {
	fs := newFlagSet("families", e)
	e.g.register(fs)
	if _, err := e.parse(fs, args); err != nil {
		return e.fail(err)
	}

	archivable, err := engine.SelectFamilies(nil)
	if err != nil {
		return e.fail(err)
	}

	t := newTable(e)
	t.AppendHeader(table.Row{"#", "Family", "Class", "Archived"})
	for i, meta := range archivable {
		t.AppendRow(table.Row{i + 1, meta.Name, engine.Classify(meta), "yes"})
	}

	deferred := engine.Deferred()
	for _, name := range sortedKeys(deferred) {
		t.AppendRow(table.Row{"", name, deferred[name], coverageOf(deferred[name])})
	}
	t.Render()

	fprintf(e.out, "\n%d of %d families are read directly.\n",
		len(archivable), len(archivable)+len(deferred))
	fprintf(e.out, "\nNote: %s\n", filesAreaNote)
	return exitOK
}
