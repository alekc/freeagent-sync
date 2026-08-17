package engine

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekc/freeagent"
	"golang.org/x/oauth2"

	"github.com/alekc/freeagent-sync/internal/api"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// fakeRecord is one row in the stub API.
type fakeRecord struct {
	ID        int
	UpdatedAt time.Time
	Extra     string
}

func (r fakeRecord) json(family string) string {
	body := fmt.Sprintf(`{"url":"https://api.test/v2/%s/%d","updated_at":%q`,
		family, r.ID, r.UpdatedAt.UTC().Format(time.RFC3339))
	if r.Extra != "" {
		// Both keys: different families carry their human label under
		// different names, and the archive stores whatever arrives.
		body += `,"reference":"` + r.Extra + `","name":"` + r.Extra + `"`
	}
	return body + "}"
}

// fakeAPI is a FreeAgent stand-in whose contents the test can change between
// runs, which is what makes deletion and incremental behaviour testable.
type fakeAPI struct {
	mu       sync.Mutex
	families map[string][]fakeRecord
	// raw holds hand-written payloads for the families whose shape matters
	// beyond an id and a timestamp, such as anything carrying an attachment.
	raw map[string][]string
	// singles holds one-off responses addressed by path, for the record
	// lookups the blob pass makes when a download link has expired.
	singles map[string]string
	// scoped holds the bank-scoped families, keyed family+bank account URL.
	scoped      map[string][]fakeRecord
	status      map[string]int
	scopeStatus map[string]int
	ignoring    map[string]bool
	perPage     int

	// required lists the query parameters a family refuses to answer without,
	// which is how notes behaves.
	required map[string][]string
	// paramScoped holds bodies keyed family|param|value, for the families
	// filtered by a parameter naming a parent.
	paramScoped map[string][]string

	requests   int
	lastQuery  map[string]url.Values
	seenScopes map[string]map[string]bool
	seenPaths  map[string]bool
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		families:    map[string][]fakeRecord{},
		raw:         map[string][]string{},
		singles:     map[string]string{},
		scoped:      map[string][]fakeRecord{},
		status:      map[string]int{},
		scopeStatus: map[string]int{},
		ignoring:    map[string]bool{},
		perPage:     100,
		required:    map[string][]string{},
		paramScoped: map[string][]string{},
		lastQuery:   map[string]url.Values{},
		seenScopes:  map[string]map[string]bool{},
		seenPaths:   map[string]bool{},
	}
}

func (f *fakeAPI) set(family string, records ...fakeRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.families[family] = records
}

// requireParam makes a family answer 400 unless one of the named parameters is
// present, which is what notes does.
func (f *fakeAPI) requireParam(family string, params ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.required[family] = params
}

// setScopedParam populates a family for one value of a scoping parameter.
func (f *fakeAPI) setScopedParam(family, param, value string, bodies ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paramScoped[family+"|"+param+"|"+value] = bodies
}

// sawPath reports whether a path was ever requested, so a test can prove a
// path that does not exist was never tried.
func (f *fakeAPI) sawPath(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seenPaths[path]
}

// setRaw populates a family with verbatim payloads.
func (f *fakeAPI) setRaw(family string, bodies ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw[family] = bodies
}

// setDocument answers a singleton or report path with one verbatim body.
func (f *fakeAPI) setDocument(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.singles[path] = body
}

// setSingle answers one exact path with one body, unenveloped by the pager.
func (f *fakeAPI) setSingle(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.singles[path] = body
}

// setScoped populates a bank-scoped family for one bank account.
func (f *fakeAPI) setScoped(family, bankAccount string, records ...fakeRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scoped[family+"|"+bankAccount] = records
}

// failScope makes one account of a bank-scoped family error while the others
// keep working, which is the partial fan-out the sweep has to survive.
func (f *fakeAPI) failScope(family, bankAccount string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopeStatus[family+"|"+bankAccount] = status
}

// scopesSeen reports the distinct bank_account filters a family was asked
// for, so a test can prove the fan-out actually happened.
func (f *fakeAPI) scopesSeen(family string) map[string]bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]bool{}
	for k, v := range f.seenScopes[family] {
		out[k] = v
	}
	return out
}

// ignoreUpdatedSince makes a family behave like one that accepts the filter
// and silently returns everything anyway, which is the case the probe exists
// to catch.
func (f *fakeAPI) ignoreUpdatedSince(family string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ignoring[family] = true
}

func (f *fakeAPI) failWith(family string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[family] = status
}

func (f *fakeAPI) queryFor(family string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastQuery[family]
}

func (f *fakeAPI) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// familyForPath resolves a request path back to a family name, because five
// families live at a path that is not their name: transactions is under
// accounting/, and income_tax_returns is served as self_assessment_returns.
// Tests speak in family names; the stub does the translation.
var familyForPath = func() map[string]string {
	out := map[string]string{}
	for name, meta := range freeagent.Resources {
		out[meta.Path] = name
	}
	return out
}()

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2/"), "/")
	family := path
	if resolved, ok := familyForPath[path]; ok {
		family = resolved
	}

	bankAccount := r.URL.Query().Get("bank_account")

	f.mu.Lock()
	f.seenPaths[path] = true
	single, isDocument := f.singles[family]
	rawBodies := append([]string(nil), f.raw[family]...)
	required := append([]string(nil), f.required[family]...)
	for _, param := range required {
		if value := r.URL.Query().Get(param); value != "" {
			rawBodies = append([]string(nil),
				f.paramScoped[family+"|"+param+"|"+value]...)
		}
	}
	f.requests++
	f.lastQuery[family] = r.URL.Query()
	status := f.status[family]
	records := append([]fakeRecord(nil), f.families[family]...)
	if bankAccount != "" {
		if f.seenScopes[family] == nil {
			f.seenScopes[family] = map[string]bool{}
		}
		f.seenScopes[family][bankAccount] = true
		records = append([]fakeRecord(nil), f.scoped[family+"|"+bankAccount]...)
		if scoped := f.scopeStatus[family+"|"+bankAccount]; scoped != 0 {
			status = scoped
		}
	}
	perPage := f.perPage
	ignoring := f.ignoring[family]
	f.mu.Unlock()

	// A family that insists on a parameter answers 400 without it, exactly as
	// the real one does, so a planner that forgets is caught here.
	if missingRequired(required, r) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w,
			`{"errors":{"error":{"message":"Please provide a project or contact"}}}`)
		return
	}

	// The real API rejects these families without the filter, so the stub
	// does too rather than quietly returning the wrong thing.
	if requiresBankAccount(family) && bankAccount == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errors":{"error":{"message":"bank_account is required"}}}`)
		return
	}

	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"errors":{"error":{"message":"stub failure"}}}`)
		return
	}

	if isDocument {
		_, _ = io.WriteString(w, single)
		return
	}

	// A year-addressed path that was never configured answers 404, which is
	// what the real API does for a tax year the company has no payroll in.
	if yearAddressed(family) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errors":{"error":{"message":"not found"}}}`)
		return
	}

	if !ignoring {
		records = filterUpdatedSince(records, r.URL.Query().Get("updated_since"))
	}
	plural := pluralFor(family)

	// Verbatim payloads bypass the record generator entirely: they exist for
	// the shapes a generated record cannot express.
	if len(rawBodies) > 0 {
		w.Header().Set("Link", fmt.Sprintf(`<%s?page=1>; rel="last"`, r.URL.Path))
		fmt.Fprintf(w, `{%q:[%s]}`, plural, strings.Join(rawBodies, ","))
		return
	}

	page, pages, slice := paginate(records, r.URL.Query().Get("page"), perPage)

	var links []string
	if page < pages {
		links = append(links, fmt.Sprintf(`<%s?page=%d>; rel="next"`, r.URL.Path, page+1))
	}
	links = append(links, fmt.Sprintf(`<%s?page=%d>; rel="last"`, r.URL.Path, pages))
	w.Header().Set("Link", strings.Join(links, ", "))

	items := make([]string, 0, len(slice))
	for _, rec := range slice {
		items = append(items, rec.json(family))
	}
	fmt.Fprintf(w, `{%q:[%s]}`, plural, strings.Join(items, ","))
}

// yearAddressed recognises the payroll paths, which carry a year (and maybe a
// period) after the family name.
// missingRequired reports whether none of the required parameters was given.
func missingRequired(required []string, r *http.Request) bool {
	if len(required) == 0 {
		return false
	}
	for _, param := range required {
		if r.URL.Query().Get(param) != "" {
			return false
		}
	}
	return true
}

func yearAddressed(path string) bool {
	for _, prefix := range []string{"payroll/", "payroll_profiles/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func requiresBankAccount(family string) bool {
	meta, ok := freeagent.Resources[family]
	return ok && meta.RequiresBankAccount
}

func filterUpdatedSince(records []fakeRecord, raw string) []fakeRecord {
	if raw == "" {
		return records
	}
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return records
	}
	var out []fakeRecord
	for _, rec := range records {
		if !rec.UpdatedAt.Before(since) {
			out = append(out, rec)
		}
	}
	return out
}

func paginate(records []fakeRecord, rawPage string, perPage int) (page, pages int, out []fakeRecord) {
	page = 1
	if rawPage != "" {
		if n, err := strconv.Atoi(rawPage); err == nil && n > 0 {
			page = n
		}
	}
	pages = max((len(records)+perPage-1)/perPage, 1)

	start := (page - 1) * perPage
	if start >= len(records) {
		return page, pages, nil
	}
	return page, pages, records[start:min(start+perPage, len(records))]
}

// pluralFor reads the envelope key out of the SDK registry, so the stub
// answers with the same shape the real API does.
//
// A nested path is matched by suffix: /v2/users/2/self_assessment_returns is
// still the income_tax_returns family, and answering with the whole path as the
// key would produce a shape the real API never sends.
func pluralFor(family string) string {
	if meta, ok := freeagent.Resources[family]; ok && meta.Plural != "" {
		return meta.Plural
	}
	for _, meta := range freeagent.Resources {
		if meta.Plural != "" && strings.HasSuffix(family, "/"+meta.Path) {
			return meta.Plural
		}
	}
	return family
}

// harness wires an engine to a stub API and a throwaway archive.
type harness struct {
	t *testing.T
	// apiURL is the stub's base. Resource URLs in payloads have to be on it,
	// because the SDK refuses to follow one to a different host.
	apiURL  string
	fake    *fakeAPI
	db      *store.DB
	client  *api.Client
	engine  *Engine
	account store.Account
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	fake := newFakeAPI()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	db, err := store.Open(t.Context(), t.TempDir()+"/freeagent.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	account, err := db.AddAccount(t.Context(), "test", "Test Co", "sandbox")
	if err != nil {
		t.Fatal(err)
	}

	client, err := api.NewReadOnly(api.Options{
		Environment:       freeagent.Environment{Name: "test", BaseURL: srv.URL},
		TokenSource:       oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"}),
		UserAgent:         "fasync-test",
		RequestsPerMinute: 100000,
		RequestsPerHour:   100000,
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := ui.New(ui.ModeNever, io.Discard, discardLogger{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(report.Close)

	return &harness{
		t: t, apiURL: srv.URL, fake: fake, db: db, client: client, account: *account,
		engine: New(db, client, report, *account),
	}
}

// pull runs the engine over one family unless the caller names others.
func (h *harness) pull(opts Options) Result {
	h.t.Helper()
	if len(opts.Families) == 0 {
		opts.Families = []string{"bills"}
	}
	result, err := h.engine.Pull(h.t.Context(), opts)
	if err != nil {
		h.t.Fatalf("Pull: %v", err)
	}
	return result
}

func (h *harness) familyResult(result Result, family string) FamilyResult {
	h.t.Helper()
	for _, f := range result.Families {
		if f.Family == family {
			return f
		}
	}
	h.t.Fatalf("no result for %s", family)
	return FamilyResult{}
}

func (h *harness) liveCount(family string) int64 {
	h.t.Helper()
	n, err := h.db.LiveRecordCount(h.t.Context(), h.account.ID, family)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) cursor(family string) time.Time {
	return h.scopedCursor(family, "")
}

func (h *harness) scopedCursor(family, scope string) time.Time {
	h.t.Helper()
	state, err := h.db.FamilyState(h.t.Context(), h.account.ID, family, scope)
	if err != nil {
		h.t.Fatal(err)
	}
	return state.Cursor
}

// discardReporter is a progress reporter that swallows everything, for the
// tests that exercise offline work.
func discardReporter(t *testing.T) ui.Reporter {
	t.Helper()
	report, err := ui.New(ui.ModeNever, io.Discard, discardLogger{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(report.Close)
	return report
}

type discardLogger struct{}

func (discardLogger) Info(string, ...any)  {}
func (discardLogger) Warn(string, ...any)  {}
func (discardLogger) Error(string, ...any) {}

// recordingReporter captures what the progress layer was told, so the numbers
// a user sees can be asserted rather than eyeballed.
type recordingReporter struct {
	mu       sync.Mutex
	trackers map[string]*recordingTracker
}

func (r *recordingReporter) Track(name string, total int64, _ ui.Units) ui.Tracker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.trackers == nil {
		r.trackers = map[string]*recordingTracker{}
	}
	tracker := &recordingTracker{total: total}
	r.trackers[name] = tracker
	return tracker
}

func (r *recordingReporter) Logf(string, ...any) {}
func (r *recordingReporter) Close()              {}

// finalFor reports the total and value a tracker ended on.
func (r *recordingReporter) finalFor(name string) (total, value int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tracker, ok := r.trackers[name]
	if !ok {
		return 0, 0
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.total, tracker.value
}

type recordingTracker struct {
	mu    sync.Mutex
	total int64
	value int64
}

func (t *recordingTracker) Add(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.value += n
}

func (t *recordingTracker) SetTotal(n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total = n
}

func (t *recordingTracker) Message(string) {}
func (t *recordingTracker) Done()          {}
func (t *recordingTracker) Fail(error)     {}
