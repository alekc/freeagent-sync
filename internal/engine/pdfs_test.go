package engine

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/alekc/freeagent-sync/internal/store"
)

// pdfBody is what the API answers for a render: base64 under a pdf key.
func pdfBody(content string) string {
	return fmt.Sprintf(`{"pdf":{"content":%q}}`,
		base64.StdEncoding.EncodeToString([]byte(content)))
}

func invoice(api string, id int, updatedAt string) string {
	return fmt.Sprintf(
		`{"url":"%s/v2/invoices/%d","reference":"INV-%d","updated_at":%q}`,
		api, id, id, updatedAt)
}

func TestRenderDocuments(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("invoices", invoice(h.apiURL, 1, "2026-03-14T09:00:00Z"))
	h.fake.setDocument("invoices/1/pdf", pdfBody("%PDF-1.4 invoice one"))

	h.pull(Options{Mode: store.ModeFull, Families: []string{"invoices"}})

	blobs := newBlobStore(t)
	result, err := h.engine.RenderDocuments(t.Context(), blobs, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rendered != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want one rendered", result)
	}
	if !blobs.Has(digestOf("%PDF-1.4 invoice one")) {
		t.Error("the decoded PDF is not in the blob store")
	}

	count, err := h.db.DocumentCount(t.Context(), h.account.ID, store.DocumentKindPDF)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("documents = %d, want 1", count)
	}
}

// Each render costs an API request, so a routine pass must re-render nothing
// that has not changed.
func TestRenderIsIncremental(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("invoices", invoice(h.apiURL, 1, "2026-03-14T09:00:00Z"))
	h.fake.setDocument("invoices/1/pdf", pdfBody("original"))

	h.pull(Options{Mode: store.ModeFull, Families: []string{"invoices"}})
	blobs := newBlobStore(t)

	if _, err := h.engine.RenderDocuments(t.Context(), blobs, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	before := h.fake.requestCount()

	result, err := h.engine.RenderDocuments(t.Context(), blobs, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rendered != 0 {
		t.Errorf("re-rendered %d unchanged documents, want 0", result.Rendered)
	}
	if after := h.fake.requestCount(); after != before {
		t.Errorf("an unchanged document cost %d requests", after-before)
	}
}

// A record that moved must be re-rendered: the document people care about is
// the current one, and the old render is now wrong.
func TestRenderRedoesAChangedRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("invoices", invoice(h.apiURL, 1, "2026-03-14T09:00:00Z"))
	h.fake.setDocument("invoices/1/pdf", pdfBody("original"))

	h.pull(Options{Mode: store.ModeFull, Families: []string{"invoices"}})
	blobs := newBlobStore(t)
	if _, err := h.engine.RenderDocuments(t.Context(), blobs, RenderOptions{}); err != nil {
		t.Fatal(err)
	}

	// The invoice is edited, so its render is stale.
	h.fake.setRaw("invoices", invoice(h.apiURL, 1, "2026-04-01T09:00:00Z"))
	h.fake.setDocument("invoices/1/pdf", pdfBody("amended"))
	h.pull(Options{Mode: store.ModeFull, Families: []string{"invoices"}})

	result, err := h.engine.RenderDocuments(t.Context(), blobs, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rendered != 1 {
		t.Fatalf("result = %+v, want the amended invoice re-rendered", result)
	}
	if !blobs.Has(digestOf("amended")) {
		t.Error("the amended render was not stored")
	}
	// Both renders survive: the old one is still what was sent at the time.
	if !blobs.Has(digestOf("original")) {
		t.Error("the original render was discarded")
	}
}

func TestRenderCoversEverySalesFamily(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for i, family := range PDFFamilies {
		h.fake.setRaw(family, fmt.Sprintf(
			`{"url":"%s/v2/%s/%d","updated_at":"2026-03-14T09:00:00Z"}`,
			h.apiURL, family, i+1))
		h.fake.setDocument(fmt.Sprintf("%s/%d/pdf", family, i+1),
			pdfBody("doc-"+family))
	}

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: PDFFamilies,
	}); err != nil {
		t.Fatal(err)
	}

	blobs := newBlobStore(t)
	result, err := h.engine.RenderDocuments(t.Context(), blobs, RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rendered != len(PDFFamilies) {
		t.Errorf("rendered %d, want one per sales family (%d)",
			result.Rendered, len(PDFFamilies))
	}
}

func TestRenderReportsAFailureAndCarriesOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("invoices",
		invoice(h.apiURL, 1, "2026-03-14T09:00:00Z"),
		invoice(h.apiURL, 2, "2026-03-14T09:00:00Z"))
	h.fake.setDocument("invoices/2/pdf", pdfBody("second"))
	h.fake.failWith("invoices/1/pdf", http.StatusInternalServerError)

	h.pull(Options{Mode: store.ModeFull, Families: []string{"invoices"}})

	result, err := h.engine.RenderDocuments(t.Context(), newBlobStore(t), RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rendered != 1 || result.Failed != 1 {
		t.Errorf("result = %+v, want one of each", result)
	}
	if len(result.Errs) != 1 || !strings.Contains(result.Errs[0].Error(), "invoices") {
		t.Errorf("errors = %v, want one naming the family", result.Errs)
	}
}

// Renders come from the API, so they spend rate budget and the budget has to
// be able to stop them.
func TestRenderRespectsItsBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var invoices []string
	for i := 1; i <= 6; i++ {
		invoices = append(invoices, invoice(h.apiURL, i, "2026-03-14T09:00:00Z"))
		h.fake.setDocument(fmt.Sprintf("invoices/%d/pdf", i), pdfBody(fmt.Sprintf("doc %d", i)))
	}
	h.fake.setRaw("invoices", invoices...)

	h.pull(Options{Mode: store.ModeFull, Families: []string{"invoices"}})
	spent := int64(h.fake.requestCount())

	result, err := h.engine.RenderDocuments(t.Context(), newBlobStore(t), RenderOptions{
		MaxRequests: spent + 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rendered != 2 {
		t.Errorf("rendered %d against a budget of 2, want 2", result.Rendered)
	}
	if result.Remaining != 4 {
		t.Errorf("remaining = %d, want the 4 not attempted", result.Remaining)
	}
}

func TestRenderLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var invoices []string
	for i := 1; i <= 5; i++ {
		invoices = append(invoices, invoice(h.apiURL, i, "2026-03-14T09:00:00Z"))
		h.fake.setDocument(fmt.Sprintf("invoices/%d/pdf", i), pdfBody(fmt.Sprintf("doc %d", i)))
	}
	h.fake.setRaw("invoices", invoices...)

	h.pull(Options{Mode: store.ModeFull, Families: []string{"invoices"}})

	result, err := h.engine.RenderDocuments(t.Context(), newBlobStore(t),
		RenderOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rendered != 2 {
		t.Errorf("rendered %d with a limit of 2", result.Rendered)
	}
}

func TestRenderWithNothingOutstanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	result, err := h.engine.RenderDocuments(t.Context(), newBlobStore(t), RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rendered != 0 || result.Failed != 0 {
		t.Errorf("result = %+v, want nothing attempted", result)
	}
}

func TestDecodePDFRejectsBadPayloads(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"not json":   `{nope}`,
		"no pdf key": `{"invoice":{}}`,
		"empty":      `{"pdf":{"content":""}}`,
		"not base64": `{"pdf":{"content":"not!!base64"}}`,
	}
	for name, body := range tests {
		if _, err := decodePDF([]byte(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestDecodePDFRoundTrips(t *testing.T) {
	t.Parallel()
	const content = "%PDF-1.4\nbinary\x00bytes\xff"
	got, err := decodePDF([]byte(pdfBody(content)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("decoded %q, want %q", got, content)
	}
}
