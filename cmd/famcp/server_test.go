package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/alekc/freeagent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/alekc/freeagent-sync/internal/api"
	"github.com/alekc/freeagent-sync/internal/store"
)

// fakeAPI pages one family, so the tests can prove the walk follows the Link
// header rather than stopping at the first page.
type fakeAPI struct {
	mu      sync.Mutex
	plural  string
	records []string
	perPage int

	// singular and record serve the one singleton this build exposes. Set
	// them instead of plural/records to answer a get rather than a list.
	singular string
	record   string

	requests int
	queries  []string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	f.queries = append(f.queries, r.URL.RawQuery)
	records, perPage, plural := f.records, f.perPage, f.plural
	singular, record := f.singular, f.record
	f.mu.Unlock()

	if singular != "" {
		fmt.Fprintf(w, `{%q:%s}`, singular, record)
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pages := (len(records) + perPage - 1) / perPage
	if pages == 0 {
		pages = 1
	}

	start := (page - 1) * perPage
	end := min(start+perPage, len(records))
	if start > len(records) {
		start = len(records)
	}

	var links []string
	if page < pages {
		links = append(links,
			fmt.Sprintf(`<%s?page=%d>; rel="next"`, r.URL.Path, page+1))
	}
	links = append(links, fmt.Sprintf(`<%s?page=%d>; rel="last"`, r.URL.Path, pages))
	w.Header().Set("Link", strings.Join(links, ", "))

	fmt.Fprintf(w, `{%q:[%s]}`, plural, strings.Join(records[start:end], ","))
}

// invoice is deliberately shaped like the real thing in the one way that
// matters here: total_value is a JSON string, not a number.
func invoice(n int) string {
	return fmt.Sprintf(
		`{"url":"https://api.test/v2/invoices/%d","reference":"INV-%03d",`+
			`"total_value":"1234.56","dated_on":"2026-04-0%d"}`,
		n, n, (n%9)+1)
}

// connectServer wires a real server to an in-memory client, which exercises
// schema inference, argument validation and marshalling exactly as a real
// client would, without a process boundary.
func connectServer(t *testing.T, fake *fakeAPI) *mcp.ClientSession {
	t.Helper()

	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	client, err := api.NewReadOnly(api.Options{
		Environment:       freeagent.Environment{Name: "test", BaseURL: srv.URL},
		TokenSource:       oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"}),
		UserAgent:         "famcp-test",
		RequestsPerMinute: 100000,
		RequestsPerHour:   100000,
	})
	if err != nil {
		t.Fatal(err)
	}

	server := newServer(client, store.Account{Slug: "test", Name: "Test Co"})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(t.Context(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(
		&mcp.Implementation{Name: "test", Version: "v0"}, nil,
	).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func callList(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) listOutput {
	t.Helper()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %v", tool, res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out listOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s output did not decode: %v", tool, err)
	}
	return out
}

// A caller that asks for more than one page gets them all, because a tool that
// answered with page one and a page number would have the model quietly
// analyse a fraction of the ledger and report it as a total.
func TestListFollowsPaginationToTheLimit(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{plural: "invoices", perPage: 10}
	for i := 1; i <= 25; i++ {
		fake.records = append(fake.records, invoice(i))
	}
	session := connectServer(t, fake)

	out := callList(t, session, "list_invoices", nil)
	if out.Count != 25 {
		t.Errorf("count = %d, want 25", out.Count)
	}
	if out.Truncated {
		t.Error("a complete answer was reported as truncated")
	}
	if out.Pages != 3 {
		t.Errorf("pages read = %d, want 3", out.Pages)
	}
}

// The limit has to stop the walk and say so. Silently returning fewer records
// than matched is the failure this flag exists to prevent.
func TestListReportsTruncation(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{plural: "invoices", perPage: 10}
	for i := 1; i <= 40; i++ {
		fake.records = append(fake.records, invoice(i))
	}
	session := connectServer(t, fake)

	out := callList(t, session, "list_invoices", map[string]any{"limit": 15})
	if out.Count != 15 {
		t.Errorf("count = %d, want 15", out.Count)
	}
	if !out.Truncated {
		t.Error("a truncated answer was not flagged, so a total from it would be wrong")
	}
	// Two pages of ten cover the limit; a third would be wasted rate budget.
	if out.Pages != 2 {
		t.Errorf("pages read = %d, want 2", out.Pages)
	}
}

// Amounts must survive as strings. Decoding them into a float is the easiest
// way to hand back a subtly wrong number, and the archive keeps bodies
// verbatim for the same reason.
func TestAmountsStayStrings(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{plural: "invoices", perPage: 10, records: []string{invoice(1)}}
	session := connectServer(t, fake)

	out := callList(t, session, "list_invoices", nil)
	if len(out.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(out.Records))
	}
	var record map[string]any
	if err := json.Unmarshal(out.Records[0], &record); err != nil {
		t.Fatal(err)
	}
	if _, ok := record["total_value"].(string); !ok {
		t.Errorf("total_value = %#v, want a string", record["total_value"])
	}
}

// The relative grammar is what a caller actually uses, so it has to reach the
// wire as a date rather than being passed through or dropped.
func TestRelativeDatesReachTheQuery(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{plural: "invoices", perPage: 10, records: []string{invoice(1)}}
	session := connectServer(t, fake)

	callList(t, session, "list_invoices", map[string]any{"from": "6mo", "to": "today"})

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.queries) == 0 {
		t.Fatal("no request reached the API")
	}
	query := fake.queries[0]
	for _, want := range []string{"from_date=", "to_date="} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %s", query, want)
		}
	}
}

// An unparseable window is the caller's mistake, and it has to come back as a
// tool error naming the field rather than as a 400 from FreeAgent.
func TestABadDateFailsBeforeTheRequest(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{plural: "invoices", perPage: 10, records: []string{invoice(1)}}
	session := connectServer(t, fake)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "list_invoices",
		Arguments: map[string]any{"from": "last thursday"},
	})
	if err != nil {
		t.Fatalf("call failed as a protocol error, want a tool error: %v", err)
	}
	if !res.IsError {
		t.Fatal("an unparseable from was accepted")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.requests != 0 {
		t.Errorf("%d requests reached the API, want none", fake.requests)
	}
}

// The singleton envelope has to come off. Returning it intact type-checks,
// validates against the schema and passes every collection test, while putting
// every field the caller was promised one level deeper than the schema says.
func TestGetCompanyUnwrapsTheEnvelope(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{singular: "company",
		record: `{"name":"Test Co","currency":"GBP"}`}
	session := connectServer(t, fake)

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_company"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("get_company returned a tool error: %v", res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Company map[string]any `json:"company"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Company["name"] != "Test Co" {
		t.Errorf("company.name = %#v, want the record's own name; "+
			"the envelope is probably still wrapped", out.Company["name"])
	}
	if _, wrapped := out.Company["company"]; wrapped {
		t.Error("the company envelope was handed back inside itself")
	}
}

// A response missing the key has to name what did arrive, because the caller
// cannot see the body and a bare decode error says nothing about which field
// went missing.
func TestUnwrapNamesTheKeysItFound(t *testing.T) {
	t.Parallel()
	_, err := unwrap([]byte(`{"errors":{"error":"nope"}}`), "company")
	if err == nil {
		t.Fatal("a response with no company key was accepted")
	}
	for _, want := range []string{`"company"`, "errors"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// The tool list is the model's whole view of the server, so it has to carry
// the families this build can actually serve and none of the ones whose
// required parameters it does not yet ask for.
func TestToolListCoversCollectionsAndExcludesScopedFamilies(t *testing.T) {
	t.Parallel()
	session := connectServer(t, &fakeAPI{plural: "invoices", perPage: 10})

	names := map[string]bool{}
	for tool, err := range session.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names[tool.Name] = true
	}

	for _, want := range []string{
		"list_invoices", "list_bills", "list_expenses", "list_contacts", "get_company",
	} {
		if !names[want] {
			t.Errorf("%s is missing from the tool list", want)
		}
	}
	// bank_transactions needs a bank_account, notes needs a contact or project,
	// and payroll is addressed by year. Offering them without those parameters
	// would produce a 400 the model cannot act on.
	for _, unwanted := range []string{
		"list_bank_transactions", "list_notes", "list_payroll", "list_attachments",
		"list_trial_balance",
	} {
		if names[unwanted] {
			t.Errorf("%s is offered but its required parameters are not modelled yet", unwanted)
		}
	}
}

// Every served family must be reachable by the name the tool advertises.
func TestServableCollectionsAreSortedAndPlural(t *testing.T) {
	t.Parallel()
	metas := servableCollections()
	if len(metas) == 0 {
		t.Fatal("no collections are servable")
	}
	for i, meta := range metas {
		if meta.Plural == "" {
			t.Errorf("%s has no plural envelope key", meta.Name)
		}
		if i > 0 && metas[i-1].Name >= meta.Name {
			t.Errorf("tool order is unstable at %s", meta.Name)
		}
	}
}
