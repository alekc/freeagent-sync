package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alekc/freeagent"
	"golang.org/x/oauth2"
)

// newTestClient points a read-only client at a stub server. Rate limits are
// raised because the stub answers instantly and the tests would otherwise
// spend their time waiting on a budget sized for the real API.
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := NewReadOnly(Options{
		Environment:       freeagent.Environment{Name: "test", BaseURL: srv.URL},
		TokenSource:       oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"}),
		UserAgent:         "fasync-test",
		RequestsPerMinute: 100000,
		RequestsPerHour:   100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, srv
}

var billsMeta = freeagent.ResourceMeta{
	Name: "bills", Path: "bills", Singular: "bill", Plural: "bills",
}

// pagedHandler serves `pages` pages of `perPage` bills with a Link header, so
// the walk exercises the same header parsing the real API drives.
func pagedHandler(t *testing.T, pages, perPage int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			var err error
			if page, err = strconv.Atoi(raw); err != nil {
				t.Errorf("page parameter %q is not a number", raw)
			}
		}

		items := make([]string, 0, perPage)
		for i := range perPage {
			id := (page-1)*perPage + i + 1
			items = append(items, fmt.Sprintf(
				`{"url":"https://api.test/v2/bills/%d","reference":"B%d"}`, id, id))
		}

		var links []string
		if page < pages {
			links = append(links, fmt.Sprintf(`<%s?page=%d>; rel="next"`, r.URL.Path, page+1))
		}
		links = append(links, fmt.Sprintf(`<%s?page=%d>; rel="last"`, r.URL.Path, pages))
		w.Header().Set("Link", strings.Join(links, ", "))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"bills":[%s]}`, strings.Join(items, ","))
	}
}

func TestPagesWalksEveryPage(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, pagedHandler(t, 3, 2))

	var seen []string
	var pages int
	for page, err := range client.Pages(t.Context(), billsMeta, nil) {
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if page.Last != 3 {
			t.Errorf("page %d reports Last = %d, want 3", page.Number, page.Last)
		}
		for _, rec := range page.Records {
			seen = append(seen, urlOf(t, rec))
		}
	}

	if pages != 3 {
		t.Errorf("walked %d pages, want 3", pages)
	}
	if len(seen) != 6 {
		t.Fatalf("collected %d records, want 6", len(seen))
	}
	if seen[0] != "https://api.test/v2/bills/1" || seen[5] != "https://api.test/v2/bills/6" {
		t.Errorf("records are out of order: %v", seen)
	}
	if got := client.Requests(); got != 3 {
		t.Errorf("made %d requests, want 3", got)
	}
}

// The first page's Link header carries the last page number, which is what
// turns an otherwise indeterminate progress bar into a real one.
func TestPagesReportsTheLastPage(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, pagedHandler(t, 7, 1))

	for page, err := range client.Pages(t.Context(), billsMeta, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if page.Last != 7 {
			t.Fatalf("Last = %d on page %d, want 7", page.Last, page.Number)
		}
		break
	}
}

func TestPagesStopsWhenTheCallerBreaks(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, pagedHandler(t, 10, 1))

	var pages int
	for _, err := range client.Pages(t.Context(), billsMeta, nil) {
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if pages == 2 {
			break
		}
	}

	if pages != 2 {
		t.Errorf("walked %d pages after breaking at 2", pages)
	}
	if got := client.Requests(); got != 2 {
		t.Errorf("made %d requests, want 2; breaking should not prefetch", got)
	}
}

// A next link pointing at an empty page would loop forever, so the walk ends
// on an empty page whatever the header claims.
func TestPagesStopsOnAnEmptyPage(t *testing.T) {
	t.Parallel()
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+r.URL.Path+`?page=99>; rel="next"`)
		fmt.Fprint(w, `{"bills":[]}`)
	}
	client, _ := newTestClient(t, http.HandlerFunc(handler))

	var pages int
	for page, err := range client.Pages(t.Context(), billsMeta, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 0 {
			t.Errorf("expected an empty page, got %d records", len(page.Records))
		}
		pages++
	}
	if pages != 1 {
		t.Errorf("walked %d pages, want 1", pages)
	}
}

func TestPagesRequestsTheLargestPageSize(t *testing.T) {
	t.Parallel()
	var got string
	handler := func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("per_page")
		fmt.Fprint(w, `{"bills":[]}`)
	}
	client, _ := newTestClient(t, http.HandlerFunc(handler))

	for range client.Pages(t.Context(), billsMeta, nil) {
		break
	}
	if want := strconv.Itoa(freeagent.MaxPerPage); got != want {
		t.Errorf("per_page = %q, want %q; every request costs rate budget", got, want)
	}
}

func TestPagesPassesTheCallersFilters(t *testing.T) {
	t.Parallel()
	var query url.Values
	handler := func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		fmt.Fprint(w, `{"bills":[]}`)
	}
	client, _ := newTestClient(t, http.HandlerFunc(handler))

	opts := &freeagent.ListOptions{
		UpdatedSince: freeagent.TimeOf(mustTime(t, "2026-03-01T00:00:00Z")),
		Sort:         "updated_at",
	}
	for range client.Pages(t.Context(), billsMeta, opts) {
		break
	}

	if len(query["updated_since"]) == 0 {
		t.Error("updated_since was not sent")
	}
	if got := query.Get("sort"); got != "updated_at" {
		t.Errorf("sort = %q, want updated_at", got)
	}
}

// Yielding nothing when the plural key is absent is indistinguishable from an
// empty collection, and would let a whole family go unarchived silently.
func TestPagesRejectsAnUnexpectedEnvelope(t *testing.T) {
	t.Parallel()
	handler := func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"invoices":[{"url":"https://api.test/v2/invoices/1"}]}`)
	}
	client, _ := newTestClient(t, http.HandlerFunc(handler))

	var gotErr error
	for _, err := range client.Pages(t.Context(), billsMeta, nil) {
		gotErr = err
		break
	}
	if gotErr == nil {
		t.Fatal("a mismatched envelope was accepted")
	}
	for _, want := range []string{"bills", "invoices"} {
		if !strings.Contains(gotErr.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", gotErr, want)
		}
	}
}

func TestPagesAcceptsAnEmptyObject(t *testing.T) {
	t.Parallel()
	handler := func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	}
	client, _ := newTestClient(t, http.HandlerFunc(handler))

	for page, err := range client.Pages(t.Context(), billsMeta, nil) {
		if err != nil {
			t.Fatalf("an empty object should be an empty collection: %v", err)
		}
		if len(page.Records) != 0 {
			t.Errorf("got %d records from {}", len(page.Records))
		}
	}
}

// Categories splits its results across several keys instead of one plural
// key, and carries scalars alongside them.
func TestPagesDecodesAGroupedEnvelope(t *testing.T) {
	t.Parallel()
	handler := func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
			"admin_expenses_categories":[{"url":"https://api.test/v2/categories/285"}],
			"income_categories":[{"url":"https://api.test/v2/categories/001"}],
			"some_scalar": 3
		}`)
	}
	client, _ := newTestClient(t, http.HandlerFunc(handler))

	meta := freeagent.ResourceMeta{Name: "categories", Path: "categories", Grouped: true}
	var total int
	for page, err := range client.Pages(t.Context(), meta, nil) {
		if err != nil {
			t.Fatal(err)
		}
		total += len(page.Records)
	}
	if total != 2 {
		t.Errorf("collected %d grouped records, want 2", total)
	}
}

// Attachments are reached through their parent record, so asking the pager
// for them is a programming error that should say so.
func TestPagesRefusesAFamilyWithNoCollection(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	meta := freeagent.ResourceMeta{Name: "attachments", Path: "attachments", NoList: true}
	var gotErr error
	for _, err := range client.Pages(t.Context(), meta, nil) {
		gotErr = err
	}
	if gotErr == nil {
		t.Fatal("the pager accepted a family with no collection endpoint")
	}
	if !strings.Contains(gotErr.Error(), "parent") {
		t.Errorf("error = %q, want it to explain how attachments are reached", gotErr)
	}
}

func TestPagesSurfacesAnAPIError(t *testing.T) {
	t.Parallel()
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"errors":{"error":{"message":"nope"}}}`)
	}
	client, _ := newTestClient(t, http.HandlerFunc(handler))

	var gotErr error
	for _, err := range client.Pages(t.Context(), billsMeta, nil) {
		gotErr = err
	}
	if gotErr == nil {
		t.Fatal("a 401 was not surfaced")
	}
}

// The read-only guarantee is the reason this package has no writable
// constructor, so it is asserted rather than assumed.
func TestClientRefusesToWrite(t *testing.T) {
	t.Parallel()
	client, _ := newTestClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a %s request reached the server", r.Method)
	}))

	_, _, err := client.SDK().Raw(t.Context(), http.MethodPost, "bills", nil, nil)
	if err == nil {
		t.Fatal("a POST was accepted through the read-only client")
	}
}

func TestNewReadOnlyValidatesItsOptions(t *testing.T) {
	t.Parallel()
	if _, err := NewReadOnly(Options{
		Environment: freeagent.Environment{BaseURL: "https://example.test"},
	}); err == nil {
		t.Error("a client with no token source was accepted")
	}
	if _, err := NewReadOnly(Options{
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"}),
	}); err == nil {
		t.Error("a client with no environment was accepted")
	}
}

func urlOf(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var rec struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec.URL
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
