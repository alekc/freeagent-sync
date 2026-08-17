package tree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekc/freeagent-sync/internal/blob"
	"github.com/alekc/freeagent-sync/internal/store"
)

func newDB(t *testing.T) (*store.DB, int64) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "freeagent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	account, err := db.AddAccount(t.Context(), "test", "Test Co", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	return db, account.ID
}

func archive(t *testing.T, db *store.DB, accountID int64, family string, bodies ...string) {
	t.Helper()
	var records []store.Record
	for _, body := range bodies {
		rec, err := store.NewRecord(family, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	if _, err := db.UpsertRecords(t.Context(), accountID, records); err != nil {
		t.Fatal(err)
	}
}

func TestSlug(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Acme Ltd":               "acme-ltd",
		"Acme  //  Ltd":          "acme-ltd",
		"../../etc/passwd":       "etc-passwd",
		"C:\\Windows\\notes":     "c-windows-notes",
		"  trimmed  ":            "trimmed",
		"Ünïcödé Náme":           "ünïcödé-náme",
		"":                       "unnamed",
		"///":                    "unnamed",
		"...":                    "unnamed",
		"a_b-c d":                "a-b-c-d",
		strings.Repeat("x", 300): strings.Repeat("x", maxNameLength),
	}
	for in, want := range tests {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// A slug becomes a path component, so nothing it produces may contain a
// separator or escape upwards.
func TestSlugNeverEscapes(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"../../../etc/shadow", "..", ".", "a/b", `a\b`, "a\x00b", "con:", "x*y?z",
	} {
		got := Slug(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("Slug(%q) = %q, which contains a separator", in, got)
		}
		if got == ".." || got == "." || got == "" {
			t.Errorf("Slug(%q) = %q, which is a traversal component", in, got)
		}
	}
}

func TestFileNameKeepsTheExtension(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Receipt Jan 2026.PDF": "receipt-jan-2026.pdf",
		"scan.jpeg":            "scan.jpeg",
		"no-extension":         "no-extension",
		"../../evil.sh":        "evil.sh",
		"weird..name.pdf":      "weird..name.pdf",
		"a/b/c.pdf":            "a-b-c.pdf",
	}
	for in, want := range tests {
		if got := FileName(in); got != want {
			t.Errorf("FileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildRecordsWritesJSONPerRecord(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "bills",
		`{"url":"https://api.test/v2/bills/1","total_value":"1440.00"}`,
		`{"url":"https://api.test/v2/bills/2","total_value":"12.34"}`)

	root := filepath.Join(t.TempDir(), "records")
	stats, err := BuildRecords(t.Context(), db, accountID, root)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 2 {
		t.Errorf("wrote %d records, want 2", stats.Records)
	}

	body, err := os.ReadFile(filepath.Join(root, "bills", "1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "\n  ") {
		t.Error("the record was not written indented")
	}
	// Money must survive the round trip through the tree untouched. Decoding
	// and re-encoding would turn it into a float.
	if !strings.Contains(string(body), `"1440.00"`) {
		t.Errorf("the exact decimal was lost: %s", body)
	}
}

func TestBuildRecordsWritesHistory(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	for _, value := range []string{"10.00", "20.00", "30.00"} {
		archive(t, db, accountID, "bills",
			`{"url":"https://api.test/v2/bills/1","total_value":"`+value+`"}`)
		time.Sleep(time.Millisecond)
	}

	root := filepath.Join(t.TempDir(), "records")
	stats, err := BuildRecords(t.Context(), db, accountID, root)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Versions != 3 {
		t.Errorf("wrote %d versions, want 3", stats.Versions)
	}

	entries, err := os.ReadDir(filepath.Join(root, "bills", "1.versions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("history holds %d files, want 3", len(entries))
	}
	// Named so the directory sorts chronologically without needing mtimes.
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !sortedAscending(names) {
		t.Errorf("history files do not sort chronologically: %v", names)
	}
}

func sortedAscending(names []string) bool {
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			return false
		}
	}
	return true
}

// The tree is derived, so a rebuild must reflect the archive exactly rather
// than accumulating what used to be there.
func TestBuildRecordsReplacesTheTree(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "bills", `{"url":"https://api.test/v2/bills/1"}`)

	root := filepath.Join(t.TempDir(), "records")
	if _, err := BuildRecords(t.Context(), db, accountID, root); err != nil {
		t.Fatal(err)
	}

	// Something left over from an older layout.
	stale := filepath.Join(root, "bills", "999.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := BuildRecords(t.Context(), db, accountID, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("a rebuild kept a file the archive no longer has")
	}
}

func TestBuildFilesLinksIntoTheBlobStore(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	blobs, digest := seedBlob(t, "RECEIPT BYTES")

	archive(t, db, accountID, "contacts",
		`{"url":"https://api.test/v2/contacts/7","organisation_name":"Acme Ltd"}`)
	archive(t, db, accountID, "bills",
		`{"url":"https://api.test/v2/bills/1","dated_on":"2026-03-14",`+
			`"contact":"https://api.test/v2/contacts/7"}`)
	seedAttachment(t, db, accountID, store.Attachment{
		URL:         "https://api.test/v2/attachments/1",
		ParentURL:   "https://api.test/v2/bills/1",
		Family:      "bills",
		FileName:    "Acme Invoice.pdf",
		ContentType: "application/pdf",
	}, digest)

	root := filepath.Join(t.TempDir(), "files")
	stats, err := BuildFiles(t.Context(), db, blobs, accountID, root, LinkSymlink)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Links != 3 {
		t.Errorf("made %d links, want one per view", stats.Links)
	}

	// All three views, and each one resolves to the actual bytes.
	for _, path := range []string{
		filepath.Join(root, "by-family", "bills", "1", "acme-invoice.pdf"),
		filepath.Join(root, "by-date", "2026", "03", "2026-03-14-bills-1-acme-invoice.pdf"),
		filepath.Join(root, "by-contact", "acme-ltd", "2026-03-14-bills-1-acme-invoice.pdf"),
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if string(got) != "RECEIPT BYTES" {
			t.Errorf("%s resolved to %q", path, got)
		}
	}
}

// Relative links mean the whole data directory can be moved without breaking
// every view in it.
func TestBuildFilesUsesRelativeSymlinks(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	blobs, digest := seedBlob(t, "BYTES")

	archive(t, db, accountID, "bills", `{"url":"https://api.test/v2/bills/1"}`)
	seedAttachment(t, db, accountID, store.Attachment{
		URL:       "https://api.test/v2/attachments/1",
		ParentURL: "https://api.test/v2/bills/1",
		Family:    "bills",
		FileName:  "r.pdf",
	}, digest)

	root := filepath.Join(t.TempDir(), "files")
	if _, err := BuildFiles(t.Context(), db, blobs, accountID, root, LinkSymlink); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(root, "by-family", "bills", "1", "r.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(target) {
		t.Errorf("symlink target %q is absolute; moving the data directory would break it", target)
	}
}

// Two different attachments with the same name in the same view must both
// survive, distinguished by their content address.
func TestBuildFilesResolvesNameCollisions(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	dir := t.TempDir()
	blobs, err := blob.NewStore(filepath.Join(dir, "blobs"), filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}

	archive(t, db, accountID, "bills",
		`{"url":"https://api.test/v2/bills/1","dated_on":"2026-03-14"}`,
		`{"url":"https://api.test/v2/bills/2","dated_on":"2026-03-14"}`)

	for i, content := range []string{"FIRST", "SECOND"} {
		info, err := blobs.Put(strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		seedAttachment(t, db, accountID, store.Attachment{
			URL:       fmt.Sprintf("https://api.test/v2/attachments/%d", i+1),
			ParentURL: fmt.Sprintf("https://api.test/v2/bills/%d", i+1),
			Family:    "bills",
			FileName:  "scan.pdf",
		}, info.SHA256)
	}

	root := filepath.Join(t.TempDir(), "files")
	if _, err := BuildFiles(t.Context(), db, blobs, accountID, root, LinkSymlink); err != nil {
		t.Fatal(err)
	}

	// by-date puts both under the same directory with the same date and name.
	entries, err := os.ReadDir(filepath.Join(root, "by-date", "2026", "03"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("by-date holds %d entries, want 2: %v", len(entries), names)
	}
}

func TestBuildFilesSkipsAMissingBlob(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	dir := t.TempDir()
	blobs, err := blob.NewStore(filepath.Join(dir, "blobs"), filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}

	archive(t, db, accountID, "bills", `{"url":"https://api.test/v2/bills/1"}`)
	// Recorded as stored, but the bytes are not on disk.
	seedAttachment(t, db, accountID, store.Attachment{
		URL:       "https://api.test/v2/attachments/1",
		ParentURL: "https://api.test/v2/bills/1",
		Family:    "bills",
		FileName:  "gone.pdf",
	}, hashOf("never written"))

	root := filepath.Join(t.TempDir(), "files")
	stats, err := BuildFiles(t.Context(), db, blobs, accountID, root, LinkSymlink)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Missing != 1 || stats.Links != 0 {
		t.Errorf("stats = %+v, want the missing blob skipped rather than linked", stats)
	}
}

func TestBuildFilesHardlinkMode(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	blobs, digest := seedBlob(t, "HARD")

	archive(t, db, accountID, "bills", `{"url":"https://api.test/v2/bills/1"}`)
	seedAttachment(t, db, accountID, store.Attachment{
		URL:       "https://api.test/v2/attachments/1",
		ParentURL: "https://api.test/v2/bills/1",
		Family:    "bills",
		FileName:  "r.pdf",
	}, digest)

	root := filepath.Join(t.TempDir(), "files")
	if _, err := BuildFiles(t.Context(), db, blobs, accountID, root, LinkHardlink); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, "by-family", "bills", "1", "r.pdf")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("hardlink mode produced a symlink")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "HARD" {
		t.Errorf("read %q, want HARD", got)
	}
}

func TestParseLinkMode(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"symlink", "hardlink", "copy", "auto"} {
		if _, err := ParseLinkMode(in); err != nil {
			t.Errorf("ParseLinkMode(%q) returned %v", in, err)
		}
	}
	if _, err := ParseLinkMode("magic"); err == nil {
		t.Error("an unknown link mode was accepted")
	}
}

func seedBlob(t *testing.T, content string) (*blob.Store, string) {
	t.Helper()
	dir := t.TempDir()
	blobs, err := blob.NewStore(filepath.Join(dir, "blobs"), filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := blobs.Put(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return blobs, info.SHA256
}

func seedAttachment(
	t *testing.T, db *store.DB, accountID int64, att store.Attachment, digest string,
) {
	t.Helper()
	att.ContentSrc = "https://cdn.test/x"
	if _, err := db.UpsertAttachments(t.Context(), accountID, []store.Attachment{att}); err != nil {
		t.Fatal(err)
	}
	if err := db.StoreBlob(
		t.Context(), accountID, att.URL, digest, 1, att.ContentType); err != nil {
		t.Fatal(err)
	}
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A rendered invoice and a scanned receipt are the same kind of thing when you
// are browsing, so both belong in the same views.
func TestBuildFilesIncludesRenderedDocuments(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	blobs, digest := seedBlob(t, "%PDF invoice")

	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/88","dated_on":"2026-03-14"}`)
	if err := db.SaveDocument(t.Context(), accountID,
		"https://api.test/v2/invoices/88", store.DocumentKindPDF,
		digest, 12, "application/pdf", "2026-03-14T09:00:00Z"); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(t.TempDir(), "files")
	stats, err := BuildFiles(t.Context(), db, blobs, accountID, root, LinkSymlink)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Links == 0 {
		t.Fatal("the rendered document produced no links")
	}

	got, err := os.ReadFile(filepath.Join(root, "by-family", "invoices", "88", "88.pdf"))
	if err != nil {
		t.Fatalf("the render is not in the by-family view: %v", err)
	}
	if string(got) != "%PDF invoice" {
		t.Errorf("resolved to %q", got)
	}

	// And it appears by date, which is how anyone looks for an invoice they
	// sent in a particular month.
	if _, err := os.Stat(filepath.Join(root, "by-date", "2026", "03")); err != nil {
		t.Errorf("the render is not in the by-date view: %v", err)
	}
}

// The default has to be hardlink. A symlink is resolved before the name is
// examined, and the blobs are named by content hash with no extension, so a
// viewer following one lands on a file that looks like nothing. The browsable
// tree exists to be double-clicked.
func TestDefaultLinkModeIsHardlink(t *testing.T) {
	t.Parallel()
	if got := defaultLinkMode(); got != LinkHardlink {
		t.Errorf("default = %s, want hardlink", got)
	}
	resolved, err := ParseLinkMode("auto")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != LinkHardlink {
		t.Errorf("auto resolved to %s, want hardlink", resolved)
	}
}

// A hard-linked entry must be a real directory entry, not a link to follow, and
// its own name must carry the extension a viewer dispatches on.
func TestDefaultEntriesCarryTheirOwnName(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	blobs, digest := seedBlob(t, "%PDF-1.4 fake")

	archive(t, db, accountID, "bills", `{"url":"https://api.test/v2/bills/1"}`)
	seedAttachment(t, db, accountID, store.Attachment{
		URL:         "https://api.test/v2/attachments/1",
		ParentURL:   "https://api.test/v2/bills/1",
		Family:      "bills",
		FileName:    "Acme Invoice.pdf",
		ContentType: "application/pdf",
	}, digest)

	root := filepath.Join(t.TempDir(), "files")
	stats, err := BuildFiles(t.Context(), db, blobs, accountID, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Mode != LinkHardlink {
		t.Errorf("Mode = %s, want hardlink", stats.Mode)
	}

	path := filepath.Join(root, "by-family", "bills", "1", "acme-invoice.pdf")
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Lstat, not Stat: a symlink would still Stat as a regular file, and the
	// whole point is that there is nothing to resolve.
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the default produced a symlink, so a viewer would see the blob name")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "%PDF-1.4 fake" {
		t.Errorf("read %q, %v", got, err)
	}
}

// The mode actually used is reported, since it can differ from what was asked
// for when hardlinking is impossible.
func TestBuildFilesReportsTheModeUsed(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	blobs, digest := seedBlob(t, "bytes")

	archive(t, db, accountID, "bills", `{"url":"https://api.test/v2/bills/1"}`)
	seedAttachment(t, db, accountID, store.Attachment{
		URL:       "https://api.test/v2/attachments/1",
		ParentURL: "https://api.test/v2/bills/1",
		Family:    "bills",
		FileName:  "r.pdf",
	}, digest)

	for _, mode := range []LinkMode{LinkHardlink, LinkSymlink, LinkCopy} {
		stats, err := BuildFiles(
			t.Context(), db, blobs, accountID, filepath.Join(t.TempDir(), "f"), mode)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Mode != mode {
			t.Errorf("asked for %s, reported %s", mode, stats.Mode)
		}
	}
}
