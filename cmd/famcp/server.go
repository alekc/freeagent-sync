package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/alekc/freeagent"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alekc/freeagent-sync/internal/api"
	"github.com/alekc/freeagent-sync/internal/explain"
	"github.com/alekc/freeagent-sync/internal/family"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/timeframe"
)

// How many records one call may return. The default is large enough to answer
// most questions in a single call and small enough that an unbounded family
// does not fill the caller's context; the maximum bounds a caller who asks for
// everything.
const (
	defaultLimit = 100
	maxLimit     = 1000
)

// newServer builds the server and registers every tool this build serves.
//
// w is nil unless -allow-writes was given. When it is nil no write tool is
// registered at all, so the model is not told about a capability it would
// then be refused; the flag changes the tool list, not just the outcome.
func newServer(c *api.Client, w *explain.Client, account store.Account) *mcp.Server {
	access := "Read-only access"
	if w != nil {
		access = "Read access, plus explaining unexplained bank transactions " +
			"and attaching receipts that are not there yet"
	}
	s := mcp.NewServer(&mcp.Implementation{
		Name:        "famcp",
		Title:       "FreeAgent: " + account.Name,
		Description: access + " to the FreeAgent ledger for " + account.Slug + ".",
		Version:     Version,
		WebsiteURL:  "https://github.com/alekc/freeagent-sync",
	}, nil)

	for _, meta := range servableCollections() {
		mcp.AddTool(s, &mcp.Tool{
			Name:         "list_" + meta.Name,
			Description:  describeCollection(meta),
			OutputSchema: listOutputSchema,
			Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, listCollection(c, meta))
	}
	if meta, ok := freeagent.Resources[companyFamily]; ok {
		mcp.AddTool(s, &mcp.Tool{
			Name: "get_company",
			Description: "The company record: name, registration details and " +
				"accounting dates, including the current financial year. Docs: " + meta.Doc,
			OutputSchema: companyOutputSchema,
			Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, getCompany(c, meta))
	}
	if meta, ok := freeagent.Resources[bankTransactionFamily]; ok {
		mcp.AddTool(s, &mcp.Tool{
			Name: "list_bank_transactions",
			Description: "Bank statement lines for one bank account, which the " +
				"API requires as a parameter: take it from list_bank_accounts. " +
				"Set view to unexplained to find the lines still needing an " +
				"explanation, or marked_for_review for ones FreeAgent guessed " +
				"and a person has not confirmed. Docs: " + meta.Doc,
			OutputSchema: listOutputSchema,
			Annotations:  &mcp.ToolAnnotations{ReadOnlyHint: true},
		}, listBankTransactions(c, meta))
	}
	if w != nil {
		registerWriteTools(s, w)
	}
	return s
}

// bankTransactionFamily needs a bank_account parameter, so it gets a tool of
// its own rather than joining the generic collections.
const bankTransactionFamily = "bank_transactions"

// companyFamily is the one singleton this build serves. Every other singleton
// is a report, which needs its own window handling.
const companyFamily = "company"

// servableCollections are the families this build exposes: plain paged
// collections with a plural envelope key. The set is derived from the SDK
// registry, so a family added upstream appears without being listed here, and
// sorted so the tool list is stable between runs.
func servableCollections() []freeagent.ResourceMeta {
	var out []freeagent.ResourceMeta
	for _, meta := range freeagent.Resources {
		if family.Classify(meta) == family.ClassCollection && meta.Plural != "" {
			out = append(out, meta)
		}
	}
	slices.SortFunc(out, func(a, b freeagent.ResourceMeta) int {
		return cmpString(a.Name, b.Name)
	})
	return out
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// describeCollection tells the caller the two things it cannot see from the
// schema: that pagination is handled here, and that an answer can be short of
// what matched. meta.Doc is a URL, so it is labelled rather than run into the
// prose as if it were a sentence.
func describeCollection(meta freeagent.ResourceMeta) string {
	return fmt.Sprintf(
		"List %s from FreeAgent. Pagination is followed internally up to the "+
			"limit; when more matched, truncated is true and no total computed "+
			"from the result is complete. Records are returned verbatim. Docs: %s",
		meta.Name, meta.Doc)
}

// listInput is shared by every collection tool. The date fields take the same
// grammar the fasync flags take, because a caller says "6mo" far more often
// than it computes a date.
type listInput struct {
	From string `json:"from,omitempty" jsonschema:"earliest business date to include: 2026-03-01, today, or a relative offset such as 3d, 6mo or 1y"`
	To   string `json:"to,omitempty" jsonschema:"latest business date to include, same grammar as from"`

	UpdatedSince string `json:"updated_since,omitempty" jsonschema:"only records changed at or after this instant, same grammar as from"`
	View         string `json:"view,omitempty" jsonschema:"server-side named filter, for example open_or_overdue on invoices"`
	Sort         string `json:"sort,omitempty" jsonschema:"field to sort by, prefixed with - for descending, for example -updated_at"`
	Limit        int    `json:"limit,omitempty" jsonschema:"maximum records to return, default 100, maximum 1000"`
}

// listOutput reports what was returned and, as importantly, whether anything
// was left behind.
//
// Its schema is declared in listOutputSchema rather than inferred. Records are
// carried as raw bytes so that amounts keep the exact text FreeAgent sent, and
// Go infers json.RawMessage from its underlying []byte, as an array of
// numbers, which is not what goes over the wire.
type listOutput struct {
	Family    string `json:"family"`
	Count     int    `json:"count"`
	Pages     int    `json:"pages_read"`
	Truncated bool   `json:"truncated"`

	Records []json.RawMessage `json:"records"`
}

// A nil slice marshals as null, so the record array admits null rather than
// forcing an empty result to invent an empty array.
var listOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"family":     {Type: "string", Description: "the family that was read"},
		"count":      {Type: "integer", Description: "records returned"},
		"pages_read": {Type: "integer", Description: "API pages walked to answer"},
		"truncated": {Type: "boolean", Description: "true when more records " +
			"matched than the limit allowed, so any total computed from this " +
			"answer is incomplete"},
		"records": {
			Types: []string{"array", "null"},
			Items: &jsonschema.Schema{Type: "object"},
			Description: "records exactly as FreeAgent returned them; amounts " +
				"arrive as JSON strings and must stay strings to keep their precision",
		},
	},
}

// listCollection walks a family and returns at most the caller's limit.
//
// Pagination is deliberately invisible: handing back one page and a next-page
// number is how a caller silently analyses the first 25 of several thousand
// records. The walk stops at the limit and says so instead.
func listCollection(
	c *api.Client, meta freeagent.ResourceMeta,
) mcp.ToolHandlerFor[listInput, listOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in listInput,
	) (*mcp.CallToolResult, listOutput, error) {
		opts, err := in.options(time.Now())
		if err != nil {
			return nil, listOutput{}, err
		}
		out, err := walk(ctx, c, meta, opts, in.effectiveLimit())
		return nil, out, err
	}
}

// listBankTransactions is the one bank-scoped read this build serves, because
// finding the unexplained lines is the question the write tools exist to
// answer and it cannot be asked without a bank account.
func listBankTransactions(
	c *api.Client, meta freeagent.ResourceMeta,
) mcp.ToolHandlerFor[bankListInput, listOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in bankListInput,
	) (*mcp.CallToolResult, listOutput, error) {
		opts, err := in.options(time.Now())
		if err != nil {
			return nil, listOutput{}, err
		}
		out, err := walk(ctx, c, meta, opts, in.effectiveLimit())
		return nil, out, err
	}
}

// walk pages a family up to limit, reporting where it stopped.
func walk(
	ctx context.Context, c *api.Client, meta freeagent.ResourceMeta,
	opts *freeagent.ListOptions, limit int,
) (listOutput, error) {
	out := listOutput{Family: meta.Name}
	for page, err := range c.Pages(ctx, meta, opts) {
		if err != nil {
			return listOutput{}, fmt.Errorf("%s: %w", meta.Name, err)
		}
		out.Pages = page.Number
		for _, record := range page.Records {
			if len(out.Records) >= limit {
				out.Truncated = true
				break
			}
			out.Records = append(out.Records, record)
		}
		if out.Truncated {
			break
		}
	}
	out.Count = len(out.Records)
	return out, nil
}

// bankListInput is listInput plus the bank account the API insists on. The
// fields are spelled out rather than embedded so the generated schema stays
// something this file states rather than something struct embedding implies.
type bankListInput struct {
	BankAccount string `json:"bank_account" jsonschema:"the bank account URL to read, taken from list_bank_accounts"`
	View        string `json:"view,omitempty" jsonschema:"unexplained, marked_for_review, explained, manual, imported, or all (the default)"`

	From         string `json:"from,omitempty" jsonschema:"earliest business date to include: 2026-03-01, today, or a relative offset such as 3d, 6mo or 1y"`
	To           string `json:"to,omitempty" jsonschema:"latest business date to include, same grammar as from"`
	UpdatedSince string `json:"updated_since,omitempty" jsonschema:"only records changed at or after this instant, same grammar as from"`
	Sort         string `json:"sort,omitempty" jsonschema:"field to sort by, prefixed with - for descending"`
	Limit        int    `json:"limit,omitempty" jsonschema:"maximum records to return, default 100, maximum 1000"`
}

func (in bankListInput) options(now time.Time) (*freeagent.ListOptions, error) {
	if strings.TrimSpace(in.BankAccount) == "" {
		return nil, errors.New(
			"bank_account is required; list_bank_accounts returns the URLs")
	}
	opts, err := listInput{
		From: in.From, To: in.To, UpdatedSince: in.UpdatedSince,
		View: in.View, Sort: in.Sort,
	}.options(now)
	if err != nil {
		return nil, err
	}
	opts.Extra = url.Values{"bank_account": {in.BankAccount}}
	return opts, nil
}

func (in bankListInput) effectiveLimit() int {
	return listInput{Limit: in.Limit}.effectiveLimit()
}

type companyInput struct{}

// Declared rather than inferred, for the same reason as listOutput.
type companyOutput struct {
	Company json.RawMessage `json:"company"`
}

var companyOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"company": {
			Type:        "object",
			Description: "the company record exactly as FreeAgent returned it",
		},
	},
}

func getCompany(
	c *api.Client, meta freeagent.ResourceMeta,
) mcp.ToolHandlerFor[companyInput, companyOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, _ companyInput,
	) (*mcp.CallToolResult, companyOutput, error) {
		body, _, err := c.Get(ctx, meta.Path, nil)
		if err != nil {
			return nil, companyOutput{}, err
		}
		record, err := unwrap(body, meta.Singular)
		if err != nil {
			return nil, companyOutput{}, fmt.Errorf("%s: %w", meta.Name, err)
		}
		return nil, companyOutput{Company: record}, nil
	}
}

// unwrap strips the envelope key a singleton endpoint wraps its record in.
// Handing the envelope straight back gives the caller {"company":{"company":
// {...}}}, where every field it was told to expect is one level deeper than
// the schema says. The pager does the same for the plural key.
func unwrap(body []byte, key string) (json.RawMessage, error) {
	if key == "" {
		return body, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decoding the response: %w", err)
	}
	record, ok := envelope[key]
	if !ok {
		return nil, fmt.Errorf("the response has no %q key, only: %s",
			key, strings.Join(sortedKeys(envelope), ", "))
	}
	return record, nil
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// options maps the caller's window onto the SDK's list options, which validate
// themselves before a request is built, so a bad value fails here rather than
// as a 400 from FreeAgent.
func (in listInput) options(now time.Time) (*freeagent.ListOptions, error) {
	opts := &freeagent.ListOptions{View: in.View, Sort: in.Sort}

	from, err := timeframe.ParseDate(in.From, now)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	if !from.IsZero() {
		opts.FromDate = freeagent.Date{Time: from}
	}

	to, err := timeframe.ParseDate(in.To, now)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	if !to.IsZero() {
		opts.ToDate = freeagent.Date{Time: to}
	}

	since, err := timeframe.Parse(in.UpdatedSince, now)
	if err != nil {
		return nil, fmt.Errorf("updated_since: %w", err)
	}
	if !since.IsZero() {
		opts.UpdatedSince = freeagent.TimeOf(since)
	}
	return opts, nil
}

func (in listInput) effectiveLimit() int {
	switch {
	case in.Limit <= 0:
		return defaultLimit
	case in.Limit > maxLimit:
		return maxLimit
	default:
		return in.Limit
	}
}
