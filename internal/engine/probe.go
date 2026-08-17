package engine

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// ProbeUpdatedSince is the capability key recorded in the archive.
const ProbeUpdatedSince = "updated_since"

// Probe results.
const (
	// ProbeHonoured means the filter was applied.
	ProbeHonoured = "honoured"
	// ProbeIgnored means records came back that the filter excluded, so an
	// incremental read of this family would silently be a full one.
	ProbeIgnored = "ignored"
	// ProbeNoData means the family is empty, so nothing can be concluded.
	ProbeNoData = "no-data"
	// ProbeFailed means the probe itself errored.
	ProbeFailed = "failed"
	// ProbeNotApplicable means the family is not a paged collection, so
	// updated_since has nothing to filter.
	ProbeNotApplicable = "not applicable"
	// ProbeUnavailable means the company does not have this family.
	ProbeUnavailable = "unavailable"
)

// probeHorizon is the updated_since value used to prove the filter works.
// Far enough ahead that no real record can match, near enough that a server
// validating the date will still accept it.
var probeHorizon = time.Date(2199, 1, 1, 0, 0, 0, 0, time.UTC)

// ProbeResult is what one family answered.
type ProbeResult struct {
	Family string
	Result string
	Detail string
	Err    error
}

// Probe establishes which families actually honour updated_since.
//
// This exists because a family that ignores the filter returns everything,
// which looks exactly like success. Two cheap requests settle it: one to see
// whether the family has any records at all, and one asking for records
// changed after a date in the far future. Anything that comes back proves the
// filter was ignored.
func (e *Engine) Probe(ctx context.Context, names []string) ([]ProbeResult, error) {
	families, err := SelectFamilies(names)
	if err != nil {
		return nil, err
	}

	runID, err := e.db.StartRun(ctx, e.account.ID, store.ModeProbe, store.RunWindow{})
	if err != nil {
		return nil, err
	}

	// Scopes are resolved once, because a bank-scoped family cannot be probed
	// without the filter the API insists on.
	scopes, err := e.scopesFor(ctx, families)
	if err != nil {
		return nil, err
	}

	results := make([]ProbeResult, len(families))
	for i, meta := range families {
		results[i] = e.probeFamily(ctx, meta, scopes)
	}

	summary := store.RunSummary{
		Families: namesOf(families),
		Requests: e.client.Requests(),
		Outcome:  store.OutcomeOK,
	}
	if err := e.db.FinishRun(ctx, runID, summary); err != nil {
		return results, err
	}
	return results, nil
}

func (e *Engine) probeFamily(
	ctx context.Context, meta freeagent.ResourceMeta, scopes map[Class][]scope,
) ProbeResult {
	out := ProbeResult{Family: meta.Name}

	// Asking a singleton or a report about updated_since produces an error
	// that says nothing about the API, so it is not asked.
	if !Probeable(meta) {
		out.Result = ProbeNotApplicable
		out.Detail = Classify(meta).String() + " families have nothing to filter"
		return out
	}

	tracker := e.report.Track("probe "+meta.Name, 2, ui.UnitsCount)
	defer tracker.Done()

	narrowing, ok := e.probeScope(ctx, meta, scopes)
	if !ok {
		out.Result = ProbeNoData
		out.Detail = "nothing to scope the request by, so the filter cannot be tested"
		e.saveProbe(ctx, out)
		return out
	}

	unfiltered, err := e.countFirstPage(ctx, meta, narrowing.path, narrowing.listOptions(1))
	tracker.Add(1)
	if err != nil {
		out.Result, out.Err = ProbeFailed, err
		out.Detail = err.Error()
		if isUnavailable(err) {
			out.Result, out.Err = ProbeUnavailable, nil
			out.Detail = "this company does not have " + meta.Name
		}
		e.saveProbe(ctx, out)
		return out
	}
	if unfiltered == 0 {
		out.Result = ProbeNoData
		out.Detail = "the family has no records, so the filter cannot be tested"
		e.saveProbe(ctx, out)
		return out
	}

	filtered, err := e.countFirstPage(ctx, meta, narrowing.path,
		narrowing.listOptions(1, freeagent.TimeOf(probeHorizon)))
	tracker.Add(1)
	if err != nil {
		out.Result, out.Err = ProbeFailed, err
		out.Detail = err.Error()
		e.saveProbe(ctx, out)
		return out
	}

	if filtered > 0 {
		out.Result = ProbeIgnored
		out.Detail = fmt.Sprintf(
			"records dated before %s came back, so updated_since was not applied",
			probeHorizon.Format(time.DateOnly))
	} else {
		out.Result = ProbeHonoured
	}
	e.saveProbe(ctx, out)
	return out
}

// probeScope picks one scope to probe a scoped family through, since the API
// refuses the request without it. One is enough: the question is whether the
// endpoint honours the filter, not what any particular scope contains.
func (e *Engine) probeScope(
	ctx context.Context, meta freeagent.ResourceMeta, scopes map[Class][]scope,
) (probeNarrowing, bool) {
	class := Classify(meta)
	switch class {
	case ClassBankScoped, ClassParentScoped:
		available := scopes[class]
		if len(available) == 0 {
			return probeNarrowing{}, false
		}
		return probeNarrowing{query: available[0].query}, true

	case ClassUserScoped:
		available, err := e.userScopes(ctx, meta)
		if err != nil || len(available) == 0 {
			return probeNarrowing{}, false
		}
		return probeNarrowing{path: available[0].path}, true

	default:
		return probeNarrowing{}, true
	}
}

// probeNarrowing is how one probe request is scoped.
type probeNarrowing struct {
	query url.Values
	path  string
}

func (n probeNarrowing) listOptions(
	perPage int, updatedSince ...freeagent.Time,
) *freeagent.ListOptions {
	opts := &freeagent.ListOptions{PerPage: perPage, Extra: n.query}
	if len(updatedSince) > 0 {
		opts.UpdatedSince = updatedSince[0]
	}
	return opts
}

// countFirstPage asks for a single record, which is the cheapest question
// that distinguishes empty from not.
func (e *Engine) countFirstPage(
	ctx context.Context, meta freeagent.ResourceMeta,
	path string, opts *freeagent.ListOptions,
) (int, error) {
	if opts == nil {
		opts = &freeagent.ListOptions{PerPage: 1}
	}
	if path == "" {
		path = meta.Path
	}
	for page, err := range e.client.PagesAt(ctx, meta, path, opts) {
		if err != nil {
			return 0, err
		}
		return len(page.Records), nil
	}
	return 0, nil
}

// saveProbe records the outcome so the next run reads it instead of guessing.
// Only a conclusive answer sets the capability; a failed or untestable probe
// leaves it unknown rather than asserting something wrong.
func (e *Engine) saveProbe(ctx context.Context, result ProbeResult) {
	_ = e.db.SaveCapability(ctx, e.account.ID, result.Family,
		ProbeUpdatedSince, result.Result, result.Detail)

	switch result.Result {
	case ProbeHonoured:
		_ = e.db.SaveUpdatedSinceSupport(ctx, e.account.ID, result.Family, "", true)
	case ProbeIgnored:
		_ = e.db.SaveUpdatedSinceSupport(ctx, e.account.ID, result.Family, "", false)
	}
}
