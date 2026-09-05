//go:build integration

// Live coverage of the write path against a sandbox company.
//
// The unit tests prove the guards bite against a fake, which is worth having
// but says nothing about whether the fake resembles FreeAgent. This suite is
// for the other half: that the attachments sub-resource accepts the body this
// package sends, that a receipt actually lands, and that the unexplained
// balance guard refuses a second explanation for a reason the real API agrees
// with.
//
// It refuses to run against production outright rather than skipping, because
// the difference between the two is somebody's accounting records. Everything
// it creates hangs off a bank account of its own, which is deleted last, so a
// failure part way through still leaves nothing behind.
//
// Run it with:
//
//	direnv exec ~/fasync go test -tags integration -run TestLive ./internal/explain/ -v
package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekc/freeagent"
	"github.com/shopspring/decimal"
)

// liveSetup holds the narrow client under test and a full one for the
// arranging and tidying the narrow client deliberately cannot do. Keeping the
// two apart is the point of this package, so the test does not blur it either.
type liveSetup struct {
	explain *Client
	full    *freeagent.Client
	audit   string
}

func liveClient(t *testing.T) *liveSetup {
	t.Helper()

	// The default is the sandbox and the only permitted value is the sandbox.
	// This suite writes, so a misdirected FREEAGENT_ENV is a thing to fail on
	// rather than to skip past.
	env := freeagent.Sandbox
	if name := os.Getenv("FREEAGENT_ENV"); name != "" && name != env.Name {
		t.Fatalf("the write suite runs against the sandbox only, got FREEAGENT_ENV=%q", name)
	}

	id := os.Getenv("FREEAGENT_CLIENT_ID")
	secret := os.Getenv("FREEAGENT_CLIENT_SECRET")
	if id == "" || secret == "" {
		t.Skip("FREEAGENT_CLIENT_ID and FREEAGENT_CLIENT_SECRET are not set")
	}

	path := os.Getenv("FREEAGENT_TOKEN_FILE")
	if path == "" {
		var err error
		if path, err = freeagent.DefaultTokenPath(); err != nil {
			t.Fatalf("DefaultTokenPath: %v", err)
		}
	}
	store, err := freeagent.NewFileStore(path, env.Name)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if _, err := store.Load(t.Context()); err != nil {
		t.Skipf("no usable token in %s for %s: %v (run: facli auth login)", path, env.Name, err)
	}
	source, err := freeagent.NewTokenSource(t.Context(), env.OAuthConfig(id, secret, ""), store)
	if err != nil {
		t.Fatalf("NewTokenSource: %v", err)
	}

	audit := filepath.Join(t.TempDir(), "writes.jsonl")
	narrow, err := New(Options{
		Environment: env,
		TokenSource: source,
		UserAgent:   "famcp-integration/0 (+https://github.com/alekc/freeagent-sync)",
		Account:     "sandbox",
		AuditPath:   audit,
	})
	if err != nil {
		t.Fatalf("explain.New: %v", err)
	}

	full, err := freeagent.NewClient(
		freeagent.WithBaseURL(env.BaseURL),
		freeagent.WithTokenSource(source),
		freeagent.WithUserAgent("famcp-integration/0"),
		freeagent.WithAPIVersion(apiVersion),
	)
	if err != nil {
		t.Fatalf("freeagent.NewClient: %v", err)
	}
	return &liveSetup{explain: narrow, full: full, audit: audit}
}

// runTag makes every record identifiable and unique, so a rerun cannot collide
// with leftovers from a run that failed to clean up.
func runTag() string { return "FAMCPTEST-" + time.Now().UTC().Format("20060102-150405") }

// onePixelPNG is a real PNG rather than arbitrary bytes, because FreeAgent
// checks the content and not only the declared type.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
	0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05, 0x00, 0x01,
	0x0d, 0x0a, 0x2d, 0xb4,
	0x00, 0x00, 0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// TestLiveExplainAndAttach walks the whole feature against the sandbox: an
// unexplained transaction appears, gets explained with a receipt in one call,
// and then refuses both a duplicate receipt and a second explanation.
func TestLiveExplainAndAttach(t *testing.T) {
	s := liveClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	tag := runTag()

	category := s.spendCategory(ctx, t)
	account := s.bankAccount(ctx, t, tag)
	txn := s.unexplainedLine(ctx, t, account, tag)

	// --- Explain, with the receipt in the same call ---------------------------
	res, err := s.explain.Explain(ctx, Request{
		Transaction: txn,
		Category:    category,
		GrossValue:  decimal.RequireFromString("-12.34"),
		Description: tag + " explained",
		Receipt: &Receipt{
			FileName:    tag + ".png",
			ContentType: "image/png",
			Description: "integration test receipt",
			Data:        onePixelPNG,
		},
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	s.deleteLater(t, "explanation", res.Explanation)
	t.Logf("explained %s at %s with %d file(s)", txn, res.Explanation, res.Attachments)

	if res.Attachments != 1 {
		t.Errorf("attachments = %d, want 1", res.Attachments)
	}
	if res.UnexplainedWas != "-12.34" || res.RemainingAfter != "0" {
		t.Errorf("balance reported as was=%q after=%q, want -12.34 and 0",
			res.UnexplainedWas, res.RemainingAfter)
	}

	// The result is this package's own account of what happened. Read the files
	// back from the API to check it against the company's.
	files := s.attachmentsOf(ctx, t, res.Explanation)
	if len(files) != 1 {
		t.Fatalf("the explanation carries %d file(s), want 1", len(files))
	}
	if files[0].FileName != tag+".png" {
		t.Errorf("file_name = %q, want %q", files[0].FileName, tag+".png")
	}
	if files[0].FileSize != len(onePixelPNG) {
		t.Errorf("file_size = %d, want %d", files[0].FileSize, len(onePixelPNG))
	}
	if files[0].ContentSrc == "" {
		t.Error("content_src is empty, so the file did not reach storage")
	}

	// --- The guards, against the real API -------------------------------------
	if _, err := s.explain.AttachReceipt(ctx, AttachRequest{
		Explanation: res.Explanation,
		Receipt: Receipt{
			FileName: tag + ".png", ContentType: "image/png", Data: onePixelPNG,
		},
	}); !errors.Is(err, ErrAlreadyAttached) {
		t.Errorf("re-attaching the same name: err = %v, want ErrAlreadyAttached", err)
	}

	if _, err := s.explain.Explain(ctx, Request{
		Transaction: txn,
		Category:    category,
		GrossValue:  decimal.RequireFromString("-12.34"),
	}); !errors.Is(err, ErrAlreadyExplained) {
		t.Errorf("re-explaining: err = %v, want ErrAlreadyExplained", err)
	}

	// A second file under a different name is the case the old single-slot
	// guard could not express, so it is worth proving the API allows it.
	second, err := s.explain.AttachReceipt(ctx, AttachRequest{
		Explanation: res.Explanation,
		Receipt: Receipt{
			FileName: tag + "-2.png", ContentType: "image/png", Data: onePixelPNG,
		},
	})
	if err != nil {
		t.Fatalf("attaching a second file: %v", err)
	}
	if second.Attachments != 2 {
		t.Errorf("attachments = %d after the second file, want 2", second.Attachments)
	}
	if again := s.attachmentsOf(ctx, t, res.Explanation); len(again) != 2 {
		t.Errorf("the API reports %d file(s) after the second attach, want 2", len(again))
	}

	// Every attempt above, written or refused, should be on the audit file.
	// The count is the assertion that matters: a refusal the caller never sees
	// recorded is the situation the file exists to prevent.
	var trail []string
	for _, line := range auditLines(t, s.audit) {
		trail = append(trail, fmt.Sprintf("%v/%v", line["op"], line["outcome"]))
	}
	if len(trail) != 5 {
		t.Errorf("the audit holds %d line(s), want 5 (explain, attach, two "+
			"refusals, attach), got %s", len(trail), strings.Join(trail, " "))
	}
}

// bankAccount creates an account of this run's own, so the transactions below
// are not mixed in with anything else and one delete removes the lot.
func (s *liveSetup) bankAccount(
	ctx context.Context, t *testing.T, tag string,
) freeagent.ResourceURL {
	t.Helper()
	account, _, err := s.full.BankAccounts.Create(ctx, &freeagent.BankAccount{
		Type:           freeagent.BankAccountTypeStandard,
		Name:           tag + " Account",
		BankName:       "Example Bank",
		Currency:       "GBP",
		OpeningBalance: new(decimal.Zero),
	})
	if err != nil {
		t.Fatalf("BankAccounts.Create: %v", err)
	}
	s.deleteLater(t, "bank account", account.URL)
	return account.URL
}

// unexplainedLine uploads a one-line statement and waits for it to import,
// which is the only way a transaction enters FreeAgent through the API.
func (s *liveSetup) unexplainedLine(
	ctx context.Context, t *testing.T, account freeagent.ResourceURL, tag string,
) freeagent.ResourceURL {
	t.Helper()

	if _, err := s.full.BankTransactions.UploadStatement(ctx, account,
		[]freeagent.StatementLine{{
			DatedOn:     freeagent.DateOf(time.Now().AddDate(0, 0, -1)),
			Amount:      decimal.RequireFromString("-12.34"),
			Description: tag + " card payment",
			FitID:       tag + "-1",
		}}); err != nil {
		t.Fatalf("BankTransactions.UploadStatement: %v", err)
	}

	// Import is asynchronous and the delay runs from under a second to most of
	// a minute, so poll rather than let the server's load decide the result.
	const (
		timeout = 90 * time.Second
		every   = 2 * time.Second
	)
	deadline := time.Now().Add(timeout)
	for attempt := 1; ; attempt++ {
		txns, _, err := s.full.BankTransactions.ListForAccount(ctx, account, nil)
		if err != nil {
			t.Fatalf("BankTransactions.ListForAccount: %v", err)
		}
		for _, txn := range txns {
			if txn.UnexplainedAmount == nil {
				t.Fatalf("unexplained_amount did not decode: %+v", txn)
			}
			t.Logf("statement import settled after %d poll(s), unexplained %v",
				attempt, txn.UnexplainedAmount)
			return txn.URL
		}
		if time.Now().After(deadline) {
			t.Fatalf("the uploaded statement line did not appear within %s", timeout)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context finished while waiting for the import: %v", ctx.Err())
		case <-time.After(every):
		}
	}
}

func (s *liveSetup) spendCategory(ctx context.Context, t *testing.T) freeagent.ResourceURL {
	t.Helper()
	groups, _, err := s.full.Categories.List(ctx, false)
	if err != nil {
		t.Fatalf("Categories.List: %v", err)
	}
	if len(groups.AdminExpenses) == 0 {
		t.Fatal("no admin expense categories in the sandbox company")
	}
	return groups.AdminExpenses[0].URL
}

// attachmentsOf reads the sub-resource with the full client, so the assertion
// does not depend on the same helper the code under test uses.
func (s *liveSetup) attachmentsOf(
	ctx context.Context, t *testing.T, ref freeagent.ResourceURL,
) []freeagent.Attachment {
	t.Helper()
	raw, _, err := s.full.RawURL(ctx, http.MethodGet, attachmentsURL(ref), nil, nil)
	if err != nil {
		t.Fatalf("reading the attachments: %v", err)
	}
	var list attachmentList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decoding the attachments: %v", err)
	}
	return list.Attachments
}

// deleteLater registers a cleanup with the full client. Deleting is exactly
// what the narrow client is built not to do, and lending it the ability for a
// test would remove the property the test is here to support.
//
// Cleanups run last in, first out, so registering each record the moment it
// exists unwinds explanation before account, which is the order the API needs.
// A failed cleanup is logged rather than failed: a record left in the sandbox
// is noise, and masking the real failure with it would be worse.
func (s *liveSetup) deleteLater(t *testing.T, kind string, ref freeagent.ResourceURL) {
	t.Helper()
	if ref.IsZero() {
		return
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, _, err := s.full.RawURL(ctx, http.MethodDelete, ref, nil, nil); err != nil {
			t.Logf("cleanup: could not delete %s %s: %v", kind, ref, err)
			return
		}
		t.Logf("cleanup: deleted %s %s", kind, ref)
	})
}
