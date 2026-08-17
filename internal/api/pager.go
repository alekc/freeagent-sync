package api

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/alekc/freeagent"
)

// maxPages bounds a single walk. Far above any real collection, so it only
// ever fires when a server keeps advertising a next page, which would
// otherwise spin until the rate limit or the run budget stopped it.
const maxPages = 10_000

// Page is one page of undecoded records, as they came off the wire.
type Page struct {
	Family  string
	Number  int
	Last    int
	Records []json.RawMessage
}

// Pages walks a collection, following the Link header. It yields raw objects
// rather than typed models: the archive stores bodies verbatim, so decoding
// here would only risk losing a field the SDK does not model yet.
//
// The caller stops the walk by returning false, which is how a budget or a
// cancelled context ends it early.
func (c *Client) Pages(
	ctx context.Context, meta freeagent.ResourceMeta, opts *freeagent.ListOptions,
) iter.Seq2[Page, error] {
	return c.PagesAt(ctx, meta, meta.Path, opts)
}

// PagesAt is Pages against an explicit path, for the families addressed by
// something other than their own registry path.
func (c *Client) PagesAt(
	ctx context.Context, meta freeagent.ResourceMeta, path string,
	opts *freeagent.ListOptions,
) iter.Seq2[Page, error] {
	return func(yield func(Page, error) bool) {
		if meta.NoList {
			yield(Page{}, fmt.Errorf(
				"api: %s has no collection endpoint and is reached through its parent",
				meta.Name))
			return
		}

		page := effectiveOptions(opts)
		for n := 1; n <= maxPages; n++ {
			body, resp, err := c.Get(ctx, path, page)
			if err != nil {
				yield(Page{Family: meta.Name, Number: n}, err)
				return
			}

			records, err := decodeEnvelope(body, meta)
			if err != nil {
				yield(Page{Family: meta.Name, Number: n}, err)
				return
			}

			out := Page{Family: meta.Name, Number: n, Records: records}
			if resp != nil {
				out.Last = resp.LastPage
			}
			if !yield(out, nil) {
				return
			}

			// A next link with nothing on the page would loop forever, so an
			// empty page ends the walk regardless of what the header says.
			if resp == nil || resp.NextPage == 0 || len(records) == 0 {
				return
			}
			next := *page
			next.Page = resp.NextPage
			page = &next
		}
		yield(Page{Family: meta.Name}, fmt.Errorf(
			"api: %s did not stop paginating after %d pages", meta.Name, maxPages))
	}
}

// effectiveOptions copies the caller's options and asks for the largest page
// the API allows, since every request costs rate budget.
func effectiveOptions(opts *freeagent.ListOptions) *freeagent.ListOptions {
	out := freeagent.ListOptions{}
	if opts != nil {
		out = *opts
	}
	if out.PerPage == 0 {
		out.PerPage = freeagent.MaxPerPage
	}
	return &out
}

// decodeEnvelope pulls the records out of a list response. A missing plural
// key is an error naming what was actually returned, because silently
// yielding nothing is indistinguishable from an empty collection and would
// let a whole family go unarchived without a word.
func decodeEnvelope(body []byte, meta freeagent.ResourceMeta) ([]json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("api: %s did not return a JSON object: %w", meta.Name, err)
	}

	if meta.Grouped {
		return decodeGrouped(envelope, meta)
	}

	raw, ok := envelope[meta.Plural]
	if !ok {
		if len(envelope) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"api: %s response has no %q key; it returned %s",
			meta.Name, meta.Plural, strings.Join(sortedKeys(envelope), ", "))
	}

	var records []json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("api: %s %q is not an array: %w", meta.Name, meta.Plural, err)
	}
	return records, nil
}

// decodeGrouped handles the families whose results are split across several
// envelope keys instead of one plural key. Categories is the only one.
func decodeGrouped(
	envelope map[string]json.RawMessage, meta freeagent.ResourceMeta,
) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, key := range sortedKeys(envelope) {
		var group []json.RawMessage
		if err := json.Unmarshal(envelope[key], &group); err != nil {
			// A grouped envelope can carry scalars alongside the arrays, so a
			// non-array value is skipped rather than failing the page.
			continue
		}
		out = append(out, group...)
	}
	if out == nil {
		return nil, fmt.Errorf("api: %s returned no groups", meta.Name)
	}
	return out, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	return slices.Sorted(maps.Keys(m))
}
