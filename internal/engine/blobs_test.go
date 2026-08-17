package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alekc/freeagent-sync/internal/blob"
	"github.com/alekc/freeagent-sync/internal/store"
)

// content is a stand-in for the third-party host that serves attachment
// bytes. It records whether any request carried an Authorization header,
// because sending FreeAgent credentials there would be a real leak.
type content struct {
	mu       sync.Mutex
	files    map[string]string
	status   map[string]int
	requests int
	authSeen bool
	agents   []string
}

func newContent() *content {
	return &content{files: map[string]string{}, status: map[string]int{}}
}

func (c *content) set(name, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[name] = body
}

func (c *content) failWith(name string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status[name] = status
}

func (c *content) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")

	c.mu.Lock()
	c.requests++
	if r.Header.Get("Authorization") != "" {
		c.authSeen = true
	}
	c.agents = append(c.agents, r.Header.Get("User-Agent"))
	body, ok := c.files[name]
	status := c.status[name]
	c.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = w.Write([]byte(body))
}

func newBlobStore(t *testing.T) *blob.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := blob.NewStore(filepath.Join(dir, "blobs"), filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func digestOf(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// billWithAttachment is a bill payload carrying a scanned receipt, which is
// where the accountant's uploads actually live.
func billWithAttachment(api string, id int, src, expires string) string {
	body := fmt.Sprintf(`{
		"url":"%[1]s/v2/bills/%[2]d",
		"updated_at":"2026-03-14T09:00:00Z",
		"attachment":{
			"url":"%[1]s/v2/attachments/%[2]d",
			"content_src":%[3]q,
			"file_name":"receipt-%[2]d.pdf",
			"content_type":"application/pdf",
			"file_size":9`, api, id, src)
	if expires != "" {
		body += fmt.Sprintf(`,"expires_at":%q`, expires)
	}
	return body + "}}"
}

func TestAttachmentsAreQueuedWhileArchiving(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("bills", billWithAttachment(h.apiURL, 1, "https://cdn.test/receipt-1.pdf", ""))

	h.pull(Options{Mode: store.ModeFull})

	pending, err := h.db.OutstandingAttachments(t.Context(), h.account.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("queued %d attachments, want 1", len(pending))
	}
	got := pending[0]
	if got.ParentURL != h.apiURL+"/v2/bills/1" {
		t.Errorf("parent = %q, want the bill", got.ParentURL)
	}
	if got.FileName != "receipt-1.pdf" || got.ContentType != "application/pdf" {
		t.Errorf("metadata = %+v, want the file name and type", got)
	}
	if got.State != store.AttachmentPending {
		t.Errorf("state = %q, want pending", got.State)
	}
}

func TestFetchBlobsDownloads(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.set("receipt-1.pdf", "PDF-BYTES")

	h.fake.setRaw("bills", billWithAttachment(h.apiURL, 1, srv.URL+"/receipt-1.pdf", ""))
	h.pull(Options{Mode: store.ModeFull})

	blobs := newBlobStore(t)
	result, err := h.engine.FetchBlobs(t.Context(), blobs, BlobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want one stored", result)
	}
	if result.Bytes != int64(len("PDF-BYTES")) {
		t.Errorf("bytes = %d, want %d", result.Bytes, len("PDF-BYTES"))
	}
	if !blobs.Has(digestOf("PDF-BYTES")) {
		t.Error("the bytes are not in the blob store")
	}

	counts, err := h.db.AttachmentCounts(t.Context(), h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Stored != 1 || counts.Pending != 0 {
		t.Errorf("counts = %+v, want one stored and none pending", counts)
	}
}

// The content host is not FreeAgent. Sending it the OAuth token would leak
// the credential to a third party, so no request may carry one.
func TestBlobDownloadsCarryNoCredentials(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.set("receipt-1.pdf", "PDF")

	h.fake.setRaw("bills", billWithAttachment(h.apiURL, 1, srv.URL+"/receipt-1.pdf", ""))
	h.pull(Options{Mode: store.ModeFull})

	if _, err := h.engine.FetchBlobs(t.Context(), newBlobStore(t), BlobOptions{}); err != nil {
		t.Fatal(err)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	if host.authSeen {
		t.Fatal("an Authorization header reached the content host")
	}
	if len(host.agents) == 0 || host.agents[0] != BlobUserAgent {
		t.Errorf("user agent = %v, want %q", host.agents, BlobUserAgent)
	}
}

// Downloads must not spend the API rate budget: they go to a different host
// entirely, and counting them would throttle the archive for no reason.
func TestBlobDownloadsSpendNoAPIBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.set("receipt-1.pdf", "PDF")

	h.fake.setRaw("bills", billWithAttachment(h.apiURL, 1, srv.URL+"/receipt-1.pdf", ""))
	h.pull(Options{Mode: store.ModeFull})
	before := h.fake.requestCount()

	if _, err := h.engine.FetchBlobs(t.Context(), newBlobStore(t), BlobOptions{}); err != nil {
		t.Fatal(err)
	}
	if after := h.fake.requestCount(); after != before {
		t.Errorf("the API saw %d extra requests during a blob pass", after-before)
	}
}

// content_src is a time-limited link. A resumed run has to re-read the
// metadata before it can fetch the bytes, which is one API request.
func TestExpiredAttachmentIsReResolved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.set("fresh.pdf", "FRESH-BYTES")

	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	h.fake.setRaw("bills", billWithAttachment(h.apiURL, 1, srv.URL+"/stale.pdf", expired))
	h.pull(Options{Mode: store.ModeFull})

	// What the API now answers for that attachment: a fresh link.
	h.fake.setSingle("attachments/1", fmt.Sprintf(
		`{"attachment":{"url":%q,"content_src":%q,`+
			`"file_name":"receipt-1.pdf","content_type":"application/pdf"}}`,
		h.apiURL+"/v2/attachments/1", srv.URL+"/fresh.pdf"))

	blobs := newBlobStore(t)
	result, err := h.engine.FetchBlobs(t.Context(), blobs, BlobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 1 {
		t.Fatalf("result = %+v, want the re-resolved attachment stored", result)
	}
	if !blobs.Has(digestOf("FRESH-BYTES")) {
		t.Error("the refreshed link was not the one downloaded")
	}
}

// A link that has not expired must be used as it is: re-resolving every
// attachment would double the API cost of a blob pass for nothing.
func TestUnexpiredAttachmentIsNotReResolved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.set("receipt-1.pdf", "PDF")

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	h.fake.setRaw("bills", billWithAttachment(h.apiURL, 1, srv.URL+"/receipt-1.pdf", future))
	h.pull(Options{Mode: store.ModeFull})
	before := h.fake.requestCount()

	if _, err := h.engine.FetchBlobs(t.Context(), newBlobStore(t), BlobOptions{}); err != nil {
		t.Fatal(err)
	}
	if after := h.fake.requestCount(); after != before {
		t.Errorf("a live link was re-resolved anyway: %d extra API requests", after-before)
	}
}

// A failed download records why and is retried later, rather than stopping
// the pass or being lost.
func TestFailedDownloadIsRecordedAndRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.failWith("receipt-1.pdf", http.StatusInternalServerError)

	h.fake.setRaw("bills", billWithAttachment(h.apiURL, 1, srv.URL+"/receipt-1.pdf", ""))
	h.pull(Options{Mode: store.ModeFull})

	blobs := newBlobStore(t)
	result, err := h.engine.FetchBlobs(t.Context(), blobs, BlobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 || result.Stored != 0 {
		t.Fatalf("result = %+v, want one failure", result)
	}

	pending, err := h.db.OutstandingAttachments(t.Context(), h.account.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("outstanding = %d, want the failure kept for retry", len(pending))
	}
	if pending[0].Attempts != 1 || pending[0].LastError == "" {
		t.Errorf("attachment = %+v, want an attempt and a reason recorded", pending[0])
	}

	// It succeeds on the next pass once the host recovers.
	host.set("receipt-1.pdf", "RECOVERED")
	host.failWith("receipt-1.pdf", 0)
	result, err = h.engine.FetchBlobs(t.Context(), blobs, BlobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 1 {
		t.Errorf("result = %+v, want the retry to succeed", result)
	}
}

// One broken file must not stop the others in the same pass.
func TestOneFailedDownloadDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.set("receipt-2.pdf", "GOOD")
	host.failWith("receipt-1.pdf", http.StatusForbidden)

	h.fake.setRaw("bills",
		billWithAttachment(h.apiURL, 1, srv.URL+"/receipt-1.pdf", ""),
		billWithAttachment(h.apiURL, 2, srv.URL+"/receipt-2.pdf", ""))
	h.pull(Options{Mode: store.ModeFull})

	result, err := h.engine.FetchBlobs(t.Context(), newBlobStore(t), BlobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 1 || result.Failed != 1 {
		t.Errorf("result = %+v, want one of each", result)
	}
}

// The same receipt attached to two records is one blob on disk.
func TestIdenticalAttachmentsDedupe(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.set("a.pdf", "IDENTICAL")
	host.set("b.pdf", "IDENTICAL")

	h.fake.setRaw("bills",
		billWithAttachment(h.apiURL, 1, srv.URL+"/a.pdf", ""),
		billWithAttachment(h.apiURL, 2, srv.URL+"/b.pdf", ""))
	h.pull(Options{Mode: store.ModeFull})

	blobs := newBlobStore(t)
	result, err := h.engine.FetchBlobs(t.Context(), blobs, BlobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stored != 2 {
		t.Fatalf("result = %+v, want both attachments stored", result)
	}

	counts, err := h.db.AttachmentCounts(t.Context(), h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Two attachment rows, one blob, so the size is counted once.
	if counts.Total != 2 || counts.Stored != 2 {
		t.Errorf("counts = %+v, want two stored attachments", counts)
	}
	if counts.Bytes != int64(len("IDENTICAL")) {
		t.Errorf("bytes = %d, want the deduped size %d", counts.Bytes, len("IDENTICAL"))
	}
}

// Re-archiving a parent must not send an already-downloaded attachment back
// to pending, or every run would re-fetch every file.
func TestReArchivingDoesNotUndoADownload(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	host := newContent()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	host.set("receipt-1.pdf", "PDF")

	h.fake.setRaw("bills", billWithAttachment(h.apiURL, 1, srv.URL+"/receipt-1.pdf", ""))
	h.pull(Options{Mode: store.ModeFull})
	if _, err := h.engine.FetchBlobs(t.Context(), newBlobStore(t), BlobOptions{}); err != nil {
		t.Fatal(err)
	}

	h.pull(Options{Mode: store.ModeFull})

	pending, err := h.db.OutstandingAttachments(t.Context(), h.account.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("%d attachments went back to pending after a re-read", len(pending))
	}
}

// Attachments hang off bank transaction explanations too, and those arrive
// nested inside the transaction rather than as a top-level key.
func TestNestedAttachmentsAreFound(t *testing.T) {
	t.Parallel()
	body := `{
		"url":"https://api.test/v2/bank_transactions/9",
		"bank_transaction_explanations":[
			{"url":"https://api.test/v2/bank_transaction_explanations/5",
			 "attachment":{"url":"https://api.test/v2/attachments/7",
			               "content_src":"https://cdn.test/nested.pdf",
			               "file_name":"nested.pdf"}}
		]
	}`

	found, err := extractAttachments("bank_transactions", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("found %d attachments, want the nested one", len(found))
	}
	if found[0].FileName != "nested.pdf" {
		t.Errorf("file name = %q, want nested.pdf", found[0].FileName)
	}
	if found[0].ParentURL != "https://api.test/v2/bank_transactions/9" {
		t.Errorf("parent = %q, want the transaction", found[0].ParentURL)
	}
}

// An object without a content_src is not an attachment, however many other
// url fields a payload carries.
func TestExtractIgnoresOrdinaryReferences(t *testing.T) {
	t.Parallel()
	body := `{
		"url":"https://api.test/v2/bills/1",
		"contact":"https://api.test/v2/contacts/2",
		"category":{"url":"https://api.test/v2/categories/285","name":"Software"}
	}`

	found, err := extractAttachments("bills", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("found %d attachments in a payload with none: %+v", len(found), found)
	}
}

func TestFetchBlobsWithNothingOutstanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	result, err := h.engine.FetchBlobs(t.Context(), newBlobStore(t), BlobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempted != 0 {
		t.Errorf("result = %+v, want nothing attempted", result)
	}
}

// An indeterminate bar renders as ??? for the whole download, which is the one
// thing a progress display must not do. The pending rows already carry
// file_size, so the total is known before the first byte arrives.
func TestBlobTrackerIsSizedFromKnownFileSizes(t *testing.T) {
	t.Parallel()
	pending := []store.Attachment{
		{FileSize: 1024}, {FileSize: 2048}, {FileSize: 0},
	}
	if got := knownBytes(pending); got != 3072 {
		t.Errorf("knownBytes = %d, want 3072", got)
	}
	// Bytes are the useful measure when they are known.
	if !progressIsBytes(pending) {
		t.Error("a pending set with known sizes should measure in bytes")
	}
}

// When no size is known, counting attachments still gives a determinate bar,
// which beats no bar at all.
func TestBlobTrackerFallsBackToCounting(t *testing.T) {
	t.Parallel()
	pending := []store.Attachment{{FileSize: 0}, {FileSize: 0}}
	if got := knownBytes(pending); got != 0 {
		t.Errorf("knownBytes = %d, want 0", got)
	}
	if progressIsBytes(pending) {
		t.Error("with no known sizes the bar should count attachments")
	}
	if got := progressFor(false, 9999); got != 1 {
		t.Errorf("progressFor(count mode) = %d, want 1 per attachment", got)
	}
	if got := progressFor(true, 9999); got != 9999 {
		t.Errorf("progressFor(byte mode) = %d, want the byte count", got)
	}
}

func progressIsBytes(pending []store.Attachment) bool { return knownBytes(pending) > 0 }
