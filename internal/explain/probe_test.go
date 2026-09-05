//go:build integration

package explain

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/alekc/freeagent"
	"github.com/shopspring/decimal"
)

// probe reads one explanation as a raw map, so fields the SDK struct calls
// read-only are still visible.
func (s *liveSetup) probe(
	ctx context.Context, t *testing.T, ref freeagent.ResourceURL,
) map[string]any {
	t.Helper()
	raw, _, err := s.full.RawURL(ctx, http.MethodGet, ref, nil, nil)
	if err != nil {
		t.Fatalf("reading %s: %v", ref, err)
	}
	var env map[string]map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decoding %s: %v", ref, err)
	}
	return env["bank_transaction_explanation"]
}

// explainRaw posts an explanation with an arbitrary body, so fields the SDK
// omits can be sent.
func (s *liveSetup) explainRaw(
	ctx context.Context, t *testing.T, fields map[string]any,
) freeagent.ResourceURL {
	t.Helper()
	raw, _, err := s.full.Raw(ctx, http.MethodPost, "bank_transaction_explanations",
		nil, map[string]any{"bank_transaction_explanation": fields})
	if err != nil {
		t.Fatalf("creating an explanation: %v", err)
	}
	var env map[string]map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decoding the create: %v", err)
	}
	url, _ := env["bank_transaction_explanation"]["url"].(string)
	if url == "" {
		t.Fatalf("the create returned no url: %v", env)
	}
	ref := freeagent.ResourceURL(url)
	s.deleteLater(t, "explanation", ref)
	return ref
}

// TestProbeMarkedForReview answers whether marked_for_review can be written.
// FreeAgent documents it as read-only and the SDK marks it read-only, so the
// expected answer throughout is no.
func TestProbeMarkedForReview(t *testing.T) {
	s := liveClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	tag := runTag()

	category := s.spendCategory(ctx, t)
	account := s.bankAccount(ctx, t, tag)

	body := func(txn freeagent.ResourceURL, daysAgo int, extra map[string]any) map[string]any {
		out := map[string]any{
			"bank_transaction": txn.String(),
			"bank_account":     account.String(),
			"category":         category.String(),
			"dated_on":         time.Now().AddDate(0, 0, -daysAgo).Format("2006-01-02"),
			"gross_value":      "-25.00",
			"description":      tag + " probe",
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	// --- Control: what does an ordinary create report? ------------------------
	plainTxn := s.lineDated(ctx, t, account, tag+" CONTROL", "-25.00", 9, tag+"-ctl")
	plain := s.probe(ctx, t, s.explainRaw(ctx, t, body(plainTxn, 9, nil)))
	t.Logf("control, field not sent:      marked_for_review=%v", plain["marked_for_review"])

	// --- Q1: does a create honour the field? ----------------------------------
	markedTxn := s.lineDated(ctx, t, account, tag+" MARKED", "-25.00", 7, tag+"-mrk")
	marked := s.explainRaw(ctx, t, body(markedTxn, 7,
		map[string]any{"marked_for_review": true}))
	t.Logf("Q1 create sent true:          marked_for_review=%v",
		s.probe(ctx, t, marked)["marked_for_review"])
	t.Logf("   account counter now:       marked_for_review_count=%d",
		s.reviewCount(ctx, t, account))

	// --- Q2: does an unrelated update clear it as a side effect? --------------
	if _, _, err := s.full.RawURL(ctx, http.MethodPut, marked, nil,
		map[string]any{"bank_transaction_explanation": map[string]any{
			"description": tag + " probe EDITED",
		}}); err != nil {
		t.Fatalf("description-only update: %v", err)
	}
	after := s.probe(ctx, t, marked)
	t.Logf("Q2 after description update:  marked_for_review=%v description=%q",
		after["marked_for_review"], after["description"])

	// --- Q3: does writing the field itself clear it? --------------------------
	if _, _, err := s.full.RawURL(ctx, http.MethodPut, marked, nil,
		map[string]any{"bank_transaction_explanation": map[string]any{
			"marked_for_review": false,
		}}); err != nil {
		t.Fatalf("writing marked_for_review=false: %v", err)
	}
	t.Logf("Q3 after writing false:       marked_for_review=%v",
		s.probe(ctx, t, marked)["marked_for_review"])

	t.Logf("Q4 account counter after:     marked_for_review_count=%d",
		s.reviewCount(ctx, t, account))
}

// reviewCount reads the account-level counter that list_bank_accounts reports,
// which is the number a caller would act on.
func (s *liveSetup) reviewCount(
	ctx context.Context, t *testing.T, account freeagent.ResourceURL,
) int {
	t.Helper()
	acct, _, err := s.full.BankAccounts.GetURL(ctx, account)
	if err != nil {
		t.Fatalf("BankAccounts.GetURL: %v", err)
	}
	if acct.MarkedForReviewCount == nil {
		t.Fatal("marked_for_review_count did not decode")
	}
	return *acct.MarkedForReviewCount
}

// lineDated uploads one statement line daysAgo days back and returns its
// transaction URL. The date varies because the upload deduplicates on date,
// amount and description together.
func (s *liveSetup) lineDated(
	ctx context.Context, t *testing.T,
	account freeagent.ResourceURL, description, amount string, daysAgo int, fitID string,
) freeagent.ResourceURL {
	t.Helper()
	if _, err := s.full.BankTransactions.UploadStatement(ctx, account,
		[]freeagent.StatementLine{{
			DatedOn:     freeagent.DateOf(time.Now().AddDate(0, 0, -daysAgo)),
			Amount:      decimal.RequireFromString(amount),
			Description: description,
			FitID:       fitID,
		}}); err != nil {
		t.Fatalf("UploadStatement(%s): %v", fitID, err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		txns, _, err := s.full.BankTransactions.ListForAccount(ctx, account, nil)
		if err != nil {
			t.Fatalf("ListForAccount: %v", err)
		}
		for _, txn := range txns {
			if txn.TransactionID == fitID {
				return txn.URL
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("statement line %s did not import within 90s", fitID)
		}
		time.Sleep(3 * time.Second)
	}
}
