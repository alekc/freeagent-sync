package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/store"
)

// Families whose records are the scopes other families are read through.
const (
	contactsFamily = "contacts"
	projectsFamily = "projects"
	usersFamily    = "users"
)

// scope is one narrowing of a family: a URL to filter by and a label to show.
type scope struct {
	url   string
	label string
	// query is what makes the scope real, for the families filtered by a
	// parameter rather than by a nested path.
	query url.Values
	// path overrides the request path, for the families nested under a parent.
	path string
}

// parentScopes are the contacts and projects notes can hang off.
//
// The cost of this family is proportional to how many of those exist, because
// the API refuses to list notes without one. There is no cheaper way to read
// them, so the count is reported rather than hidden.
func (e *Engine) parentScopes(ctx context.Context) ([]scope, error) {
	var out []scope
	for _, family := range []string{contactsFamily, projectsFamily} {
		refs, err := e.recordRefs(ctx, family)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			out = append(out, scope{
				url:   ref.url,
				label: ref.label,
				query: url.Values{singularOf(family): {ref.url}},
			})
		}
	}
	return out, nil
}

// userScopes are the users an income tax return can belong to.
func (e *Engine) userScopes(ctx context.Context, meta freeagent.ResourceMeta) ([]scope, error) {
	refs, err := e.recordRefs(ctx, usersFamily)
	if err != nil {
		return nil, err
	}

	out := make([]scope, 0, len(refs))
	for _, ref := range refs {
		// The registry Path is a suffix here: only the nested form exists.
		out = append(out, scope{
			url:   ref.url,
			label: ref.label,
			path:  usersFamily + "/" + store.IDFromURL(ref.url) + "/" + meta.Path,
		})
	}
	return out, nil
}

// recordRef is the little of a record needed to scope by it.
type recordRef struct {
	url   string
	label string
}

// recordRefs reads a family's records from the archive, fetching them first if
// they are not there. The pull order puts the scope families first, so the
// fetch only fires for a run that named a scoped family on its own.
func (e *Engine) recordRefs(ctx context.Context, family string) ([]recordRef, error) {
	refs, err := e.archivedRefs(ctx, family)
	if err != nil {
		return nil, err
	}
	if len(refs) > 0 {
		return refs, nil
	}

	meta, ok := freeagent.Resources[family]
	if !ok {
		return nil, fmt.Errorf("engine: the SDK has no %s entry", family)
	}
	e.report.Logf("reading %s: needed as a scope and none archived yet", family)

	for page, err := range e.client.Pages(ctx, meta, nil) {
		if err != nil {
			return nil, fmt.Errorf("engine: listing %s: %w", family, err)
		}
		records := make([]store.Record, 0, len(page.Records))
		for _, raw := range page.Records {
			rec, err := store.NewRecord(family, raw)
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		}
		if _, err := e.db.UpsertRecords(ctx, e.account.ID, records); err != nil {
			return nil, err
		}
	}
	return e.archivedRefs(ctx, family)
}

func (e *Engine) archivedRefs(ctx context.Context, family string) ([]recordRef, error) {
	bodies, err := e.db.LiveRecordBodies(ctx, e.account.ID, family)
	if err != nil {
		return nil, err
	}

	out := make([]recordRef, 0, len(bodies))
	for _, body := range bodies {
		var parsed struct {
			URL              string `json:"url"`
			Name             string `json:"name"`
			OrganisationName string `json:"organisation_name"`
			FirstName        string `json:"first_name"`
			LastName         string `json:"last_name"`
			Email            string `json:"email"`
		}
		if json.Unmarshal(body, &parsed) != nil || parsed.URL == "" {
			continue
		}

		label := firstNonBlank(parsed.OrganisationName, parsed.Name,
			join(parsed.FirstName, parsed.LastName), parsed.Email)
		if label == "" {
			label = store.IDFromURL(parsed.URL)
		}
		out = append(out, recordRef{url: parsed.URL, label: label})
	}
	return out, nil
}

// singularOf is the parameter name a family is filtered by: notes takes
// contact= and project=, not contacts= and projects=.
func singularOf(family string) string {
	if meta, ok := freeagent.Resources[family]; ok && meta.Singular != "" {
		return meta.Singular
	}
	return family
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func join(parts ...string) string {
	var out string
	for _, part := range parts {
		if part == "" {
			continue
		}
		if out != "" {
			out += " "
		}
		out += part
	}
	return out
}
