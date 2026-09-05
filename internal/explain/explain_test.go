package explain

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
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

// fakeAPI answers the four calls this package makes and records what it was
// asked, so a test can assert on the order of a read and a write rather than
// only on the outcome.
type fakeAPI struct {
	mu sync.Mutex

	unexplained string // the transaction's unexplained_amount, "" for absent
	attached    bool   // whether the explanation already carries a file

	calls     []string
	writeBody map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)

	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "bank_transactions/"):
		amount := ""
		if f.unexplained != "" {
			amount = fmt.Sprintf(`,"unexplained_amount":%q`, f.unexplained)
		}
		fmt.Fprintf(w, `{"bank_transaction":{"url":%q,"bank_account":"%s/v2/bank_accounts/9",`+
			`"dated_on":"2026-04-01","amount":"-100.0"%s}}`,
			"http://"+r.Host+r.URL.Path, "http://"+r.Host, amount)

	case r.Method == http.MethodGet:
		attachment := ""
		if f.attached {
			attachment = `,"attachment":{"url":"http://x/v2/attachments/5","file_name":"old.pdf"}`
		}
		fmt.Fprintf(w, `{"bank_transaction_explanation":{"url":%q%s}}`,
			"http://"+r.Host+r.URL.Path, attachment)

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

// Replacing a receipt somebody already filed would destroy the one piece of
// evidence the record carries.
func TestAttachRefusesWhenAFileIsAlreadyThere(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{attached: true}
	c, audit, base := newClient(t, fake)

	_, err := c.AttachReceipt(t.Context(), AttachRequest{
		Explanation: freeagent.ResourceURL(base + "/v2/bank_transaction_explanations/77"),
		Receipt:     Receipt{FileName: "r.pdf", ContentType: "application/pdf", Data: []byte("%PDF")},
	})
	if !errors.Is(err, ErrAlreadyAttached) {
		t.Fatalf("err = %v, want ErrAlreadyAttached", err)
	}
	if fake.saw(http.MethodPut) {
		t.Error("an update reached the API for an explanation that already had a file")
	}
	if lines := auditLines(t, audit); len(lines) != 1 || lines[0]["outcome"] != "refused" {
		t.Errorf("the refusal was not audited: %v", lines)
	}
}

// The update must carry the attachment and nothing else. Any other field in
// the body is a field this tool could overwrite by accident.
func TestAttachSendsOnlyTheAttachment(t *testing.T) {
	t.Parallel()
	fake := &fakeAPI{}
	c, _, base := newClient(t, fake)

	_, err := c.AttachReceipt(t.Context(), AttachRequest{
		Explanation: freeagent.ResourceURL(base + "/v2/bank_transaction_explanations/77"),
		Receipt:     Receipt{FileName: "r.pdf", ContentType: "application/pdf", Data: []byte("%PDF")},
	})
	if err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.writeBody) != 1 {
		t.Fatalf("the update body carried %v, want the attachment alone", fake.writeBody)
	}
	if _, ok := fake.writeBody["attachment"]; !ok {
		t.Errorf("the update body has no attachment: %v", fake.writeBody)
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
