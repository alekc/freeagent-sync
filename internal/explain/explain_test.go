package explain

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alekc/freeagent"
	"github.com/shopspring/decimal"
	"golang.org/x/oauth2"
)

// fakeAPI answers the calls this package makes and records what it was asked,
// so a test can assert on the order of a read and a write rather than only on
// the outcome.
type fakeAPI struct {
	mu sync.Mutex

	unexplained string   // the transaction's unexplained_amount, "" for absent
	files       []string // file names already on the explanation
	dropUpload  bool     // accept a POST but store nothing, as a stale version would

	calls      []string
	versions   []string
	writeBody  map[string]any
	uploadBody map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.versions = append(f.versions, r.Header.Get("X-Api-Version"))

	switch {
	case strings.HasSuffix(r.URL.Path, "/attachments"):
		if r.Method == http.MethodPost {
			var body attachmentList
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			_ = json.Unmarshal(raw, &f.uploadBody)
			if !f.dropUpload {
				for _, a := range body.Attachments {
					f.files = append(f.files, a.FileName)
				}
			}
			w.WriteHeader(http.StatusCreated)
		}
		f.writeAttachments(w)

	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "bank_transactions/"):
		amount := ""
		if f.unexplained != "" {
			amount = fmt.Sprintf(`,"unexplained_amount":%q`, f.unexplained)
		}
		fmt.Fprintf(w, `{"bank_transaction":{"url":%q,"bank_account":"%s/v2/bank_accounts/9",`+
			`"dated_on":"2026-04-01","amount":"-100.0"%s}}`,
			"http://"+r.Host+r.URL.Path, "http://"+r.Host, amount)

	case r.Method == http.MethodGet:
		// Version 2026-09-01 no longer returns a singular attachment, so the
		// fake does not either. A guard reading it would find nil here just
		// as it would in production.
		fmt.Fprintf(w, `{"bank_transaction_explanation":{"url":%q}}`,
			"http://"+r.Host+r.URL.Path)

	default:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if inner, ok := body["bank_transaction_explanation"].(map[string]any); ok {
			f.writeBody = inner
		} else {
			f.writeBody = body
		}
		fmt.Fprintf(w, `{"bank_transaction_explanation":{"url":"%s/v2/bank_transaction_explanations/77"}}`,
			"http://"+r.Host)
	}
}

func (f *fakeAPI) writeAttachments(w http.ResponseWriter) {
	list := attachmentList{}
	for i, name := range f.files {
		list.Attachments = append(list.Attachments, freeagent.Attachment{
			URL:      freeagent.ResourceURL(fmt.Sprintf("http://x/v2/attachments/%d", i+1)),
			FileName: name,
		})
	}
	_ = json.NewEncoder(w).Encode(list)
}

func (f *fakeAPI) saw(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, method) {
			return true
		}
	}
	return false
}

func newClient(t *testing.T, fake *fakeAPI) (*Client, string, string) {
	t.Helper()

	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	audit := filepath.Join(t.TempDir(), "writes.jsonl")
	c, err := New(Options{
		Environment: freeagent.Environment{Name: "test", BaseURL: srv.URL},
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"}),
		UserAgent:   "famcp-test",
		Account:     "test",
		AuditPath:   audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, audit, srv.URL
}

func auditLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("audit line is not JSON: %v", err)
		}
		out = append(out, line)
	}
	return out
}

func req(base string, value string) Request {
	return Request{
		Transaction: freeagent.ResourceURL(base + "/v2/bank_transactions/1"),
		Category:    freeagent.ResourceURL(base + "/v2/categories/285"),
		GrossValue:  decimal.RequireFromString(value),
		Description: "test",
	}
}

// The whole safety model rests on this: a transaction somebody has already
// settled must never be touched, however confident the caller is.
func TestExplainRefusesASettledTransaction(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{unexplained: "0.0"}
	c, audit, base := newClient(t, fake)

	_, err := c.Explain(t.Context(), req(base, "-100.0"))
	if !errors.Is(err, ErrAlreadyExplained) {
		t.Fatalf("err = %v, want ErrAlreadyExplained", err)
	}
	if fake.saw(http.MethodPost) {
		t.Error("a write reached the API for a settled transaction")
	}
	if lines := auditLines(t, audit); len(lines) != 1 || lines[0]["outcome"] != "refused" {
		t.Errorf("the refusal was not audited: %v", lines)
	}
}

// A partially explained line has a remaining balance, and explaining more
// than remains is how a caller quietly doubles a cost.
func TestExplainRefusesMoreThanRemains(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{unexplained: "-40.0"}
	c, _, base := newClient(t, fake)

	_, err := c.Explain(t.Context(), req(base, "-100.0"))
	if !errors.Is(err, ErrOverExplained) {
		t.Fatalf("err = %v, want ErrOverExplained", err)
	}
	if fake.saw(http.MethodPost) {
		t.Error("an over-sized explanation reached the API")
	}
}

// Explaining money out as money in is a direction error, not a rounding one.
func TestExplainRefusesTheWrongDirection(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{unexplained: "-100.0"}
	c, _, base := newClient(t, fake)

	_, err := c.Explain(t.Context(), req(base, "100.0"))
	if !errors.Is(err, ErrSignMismatch) {
		t.Fatalf("err = %v, want ErrSignMismatch", err)
	}
}

// Part of a balance is a legitimate explanation, so the guard must not be so
// strict that it only allows the exact total.
func TestExplainAllowsPartOfTheBalance(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{unexplained: "-100.0"}
	c, _, base := newClient(t, fake)

	res, err := c.Explain(t.Context(), req(base, "-40.0"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RemainingAfter != "-60" {
		t.Errorf("remaining after = %q, want -60", res.RemainingAfter)
	}
}

// The precondition is only a guard if it is read from the API at write time.
// A caller that decided the transaction was unexplained several turns ago is
// acting on a fact that may already be false.
func TestExplainReadsTheTransactionBeforeWriting(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{unexplained: "-100.0"}
	c, audit, base := newClient(t, fake)

	if _, err := c.Explain(t.Context(), req(base, "-100.0")); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	calls := append([]string(nil), fake.calls...)
	fake.mu.Unlock()
	if len(calls) < 2 {
		t.Fatalf("calls = %v, want a read then a write", calls)
	}
	if !strings.HasPrefix(calls[0], http.MethodGet) {
		t.Errorf("first call was %q, want the precondition read", calls[0])
	}
	if !strings.HasPrefix(calls[1], http.MethodPost) {
		t.Errorf("second call was %q, want the write", calls[1])
	}

	lines := auditLines(t, audit)
	if len(lines) != 1 || lines[0]["outcome"] != "written" {
		t.Fatalf("the write was not audited: %v", lines)
	}
	if lines[0]["category"] == "" || lines[0]["gross_value"] != "-100" {
		t.Errorf("the audit line does not carry what was written: %v", lines[0])
	}
}

// The explanation must be created without an inline attachment. Under version
// 2026-09-01 attachments are managed only through the sub-resource, and a
// server that ignores an inline one rather than rejecting it would leave this
// tool reporting a receipt it never filed.
func TestExplainSendsTheReceiptSeparately(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{unexplained: "-100.0"}
	c, audit, base := newClient(t, fake)

	in := req(base, "-100.0")
	in.Receipt = &Receipt{
		FileName: "r.pdf", ContentType: "application/pdf", Data: []byte("%PDF"),
	}
	res, err := c.Explain(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attachments != 1 || res.AttachedName != "r.pdf" {
		t.Errorf("result does not report the attachment: %+v", res)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, inline := fake.writeBody["attachment"]; inline {
		t.Errorf("the explanation was created with an inline attachment: %v", fake.writeBody)
	}
	if !strings.HasSuffix(fake.calls[len(fake.calls)-1], "/attachments") {
		t.Errorf("the receipt did not go to the sub-resource: %v", fake.calls)
	}
	// Two writes, so two audit lines. One line covering both would hide which
	// half happened when only one of them did.
	if lines := auditLines(t, audit); len(lines) != 2 {
		t.Errorf("want an explain line and an attach line, got %v", lines)
	}
}

// The two calls can part-succeed. The explanation exists either way, so its
// URL has to survive the error or the caller cannot finish the job.
func TestExplainNamesTheExplanationWhenTheReceiptFails(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{unexplained: "-100.0", dropUpload: true}
	c, audit, base := newClient(t, fake)

	in := req(base, "-100.0")
	in.Receipt = &Receipt{
		FileName: "r.pdf", ContentType: "application/pdf", Data: []byte("%PDF"),
	}
	_, err := c.Explain(t.Context(), in)
	if err == nil {
		t.Fatal("a failed attachment was reported as full success")
	}
	if !strings.Contains(err.Error(), "/bank_transaction_explanations/77") {
		t.Errorf("the error does not name the explanation that was created: %v", err)
	}

	lines := auditLines(t, audit)
	if len(lines) != 2 || lines[0]["outcome"] != "written" || lines[1]["outcome"] != "refused" {
		t.Errorf("the audit does not show one half succeeding: %v", lines)
	}
}

// A transaction with no unexplained_amount at all is not evidence that it is
// unexplained, so it must be treated as settled rather than as unknown.
func TestExplainRefusesWhenTheFieldIsAbsent(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{unexplained: ""}
	c, _, base := newClient(t, fake)

	_, err := c.Explain(t.Context(), req(base, "-10.0"))
	if !errors.Is(err, ErrAlreadyExplained) {
		t.Fatalf("err = %v, want ErrAlreadyExplained", err)
	}
}

func attach(base, name string) AttachRequest {
	return AttachRequest{
		Explanation: freeagent.ResourceURL(base + "/v2/bank_transaction_explanations/77"),
		Receipt: Receipt{
			FileName: name, ContentType: "application/pdf", Data: []byte("%PDF"),
		},
	}
}

// Filing the same receipt twice is what a retried call does, and appending
// makes it silently succeed unless something refuses it.
func TestAttachRefusesAFileOfTheSameName(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{files: []string{"r.pdf"}}
	c, audit, base := newClient(t, fake)

	_, err := c.AttachReceipt(t.Context(), attach(base, "r.pdf"))
	if !errors.Is(err, ErrAlreadyAttached) {
		t.Fatalf("err = %v, want ErrAlreadyAttached", err)
	}
	if fake.saw(http.MethodPost) {
		t.Error("an upload reached the API for a name that was already there")
	}
	if lines := auditLines(t, audit); len(lines) != 1 || lines[0]["outcome"] != "refused" {
		t.Errorf("the refusal was not audited: %v", lines)
	}
}

// A second, differently named receipt is a legitimate addition now that an
// explanation holds up to fifty. The old single-slot guard would refuse it.
func TestAttachAddsToWhatIsAlreadyThere(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{files: []string{"first.pdf"}}
	c, _, base := newClient(t, fake)

	res, err := c.AttachReceipt(t.Context(), attach(base, "second.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Attachments != 2 {
		t.Errorf("attachments = %d, want 2", res.Attachments)
	}
}

// The write must be a POST. PUT on the same endpoint is what replaces a file's
// contents and, with _destroy, removes one, so reaching it at all would put
// the destructive verbs back within range.
func TestAttachOnlyEverPosts(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{}
	c, _, base := newClient(t, fake)

	if _, err := c.AttachReceipt(t.Context(), attach(base, "r.pdf")); err != nil {
		t.Fatal(err)
	}
	if fake.saw(http.MethodPut) || fake.saw(http.MethodDelete) {
		t.Errorf("a destructive verb was used: %v", fake.calls)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	files, ok := fake.uploadBody["attachments"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("the upload body is not a one-element attachments array: %v", fake.uploadBody)
	}
	if _, replacing := files[0].(map[string]any)["url"]; replacing {
		t.Error("the upload named an existing attachment, which would replace it")
	}
}

// The explanation is at the documented limit, so appending is refused rather
// than discovered as a 422 after uploading the file.
func TestAttachRefusesAFullExplanation(t *testing.T) {
	t.Parallel()
	full := make([]string, maxAttachments)
	for i := range full {
		full[i] = fmt.Sprintf("receipt-%d.pdf", i)
	}
	fake := &fakeAPI{files: full}
	c, _, base := newClient(t, fake)

	_, err := c.AttachReceipt(t.Context(), attach(base, "one-too-many.pdf"))
	if !errors.Is(err, ErrTooManyAttachments) {
		t.Fatalf("err = %v, want ErrTooManyAttachments", err)
	}
	if fake.saw(http.MethodPost) {
		t.Error("an upload reached a full explanation")
	}
}

// A server on the wrong version answers a POST cheerfully and stores nothing.
// Trusting the status code would report a receipt that was never filed.
func TestAttachVerifiesTheFileLanded(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{dropUpload: true}
	c, _, base := newClient(t, fake)

	_, err := c.AttachReceipt(t.Context(), attach(base, "r.pdf"))
	if err == nil {
		t.Fatal("a silently discarded upload was reported as success")
	}
	if !strings.Contains(err.Error(), "r.pdf") {
		t.Errorf("the error does not name the missing file: %v", err)
	}
}

// The version header is what makes the sub-resource reachable at all, so it
// has to be on every request rather than assumed from the SDK default.
func TestEveryRequestStatesTheAPIVersion(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{}
	c, _, base := newClient(t, fake)

	if _, err := c.AttachReceipt(t.Context(), attach(base, "r.pdf")); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.versions) == 0 {
		t.Fatal("no requests were made")
	}
	for i, v := range fake.versions {
		if v != apiVersion {
			t.Errorf("%s sent X-Api-Version %q, want %q", fake.calls[i], v, apiVersion)
		}
	}
}

// The size and type limits belong locally: they are documented constraints,
// and finding out remotely costs a multi-megabyte upload first.
func TestReceiptValidationHappensLocally(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]Receipt{
		"no name":   {ContentType: "application/pdf", Data: []byte("x")},
		"bad type":  {FileName: "r.txt", ContentType: "text/plain", Data: []byte("x")},
		"empty":     {FileName: "r.pdf", ContentType: "application/pdf"},
		"oversized": {FileName: "r.pdf", ContentType: "application/pdf", Data: make([]byte, freeagent.MaxAttachmentBytes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAPI{unexplained: "-100.0"}
			c, _, base := newClient(t, fake)

			in := req(base, "-100.0")
			in.Receipt = &r
			if _, err := c.Explain(t.Context(), in); err == nil {
				t.Fatal("an invalid receipt was accepted")
			}
			if fake.saw(http.MethodGet) {
				t.Error("an invalid receipt still cost an API call")
			}
		})
	}
}

// An audit path is not optional. A write nobody can review afterwards is the
// situation the file exists to prevent.
func TestAnAuditPathIsRequired(t *testing.T) {
	t.Parallel()
	_, err := New(Options{
		Environment: freeagent.Environment{Name: "test", BaseURL: "http://example.invalid"},
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"}),
	})
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("err = %v, want a complaint about the audit path", err)
	}
}
