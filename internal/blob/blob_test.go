package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "blobs"), filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func digestOf(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func TestPutAndOpen(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const data = "a scanned receipt"

	info, err := s.Put(strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if info.SHA256 != digestOf(data) {
		t.Errorf("digest = %s, want %s", info.SHA256, digestOf(data))
	}
	if info.Size != int64(len(data)) {
		t.Errorf("size = %d, want %d", info.Size, len(data))
	}
	if !s.Has(info.SHA256) {
		t.Error("the store does not report holding what it just stored")
	}

	r, err := s.Open(info.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != data {
		t.Errorf("read back %q, want %q", got, data)
	}
}

// Content addressing gives dedupe for free, which matters because accountants
// attach the same receipt to more than one record.
func TestPutIsIdempotent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	first, err := s.Put(strings.NewReader("same bytes"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(strings.NewReader("same bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("identical content produced different digests")
	}

	var files int
	_ = filepath.WalkDir(s.Root(), func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Errorf("the store holds %d files for one distinct blob, want 1", files)
	}
}

// The digest is only known at the last byte, so a failed read must leave no
// file behind under a name that does not describe its contents.
func TestPutLeavesNothingOnAFailedRead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	_, err := s.Put(io.MultiReader(
		strings.NewReader("partial"),
		&failingReader{err: errors.New("connection reset")},
	))
	if err == nil {
		t.Fatal("a failed read was accepted")
	}

	var files int
	_ = filepath.WalkDir(s.Root(), func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			files++
		}
		return nil
	})
	if files != 0 {
		t.Errorf("a failed download left %d files in the store", files)
	}
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestPathFansOut(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	digest := digestOf("x")
	path, err := s.Path(digest)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.Root(), digest[0:2], digest[2:4], digest)
	if path != want {
		t.Errorf("Path = %q, want %q", path, want)
	}
}

// A digest reaches a file path, so anything that is not one has to be
// rejected before it gets there.
func TestPathRejectsANonDigest(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	for _, bad := range []string{
		"", "abc", "../../etc/passwd", strings.Repeat("g", 64),
		strings.ToUpper(digestOf("x")), digestOf("x") + "a",
	} {
		if _, err := s.Path(bad); err == nil {
			t.Errorf("Path(%q) was accepted", bad)
		}
		if s.Has(bad) {
			t.Errorf("Has(%q) reported true", bad)
		}
	}
}

func TestOpenReportsMissing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	_, err := s.Open(digestOf("never stored"))
	if !errors.Is(err, ErrNotStored) {
		t.Fatalf("error = %v, want ErrNotStored", err)
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	info, err := s.Put(strings.NewReader("intact"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(info.SHA256); err != nil {
		t.Errorf("a freshly stored blob failed verification: %v", err)
	}
}

// Verification has to actually re-hash. Comparing the file against anything
// else stored alongside it would just be comparing two copies of the same
// possibly-wrong thing.
func TestVerifyDetectsCorruption(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	info, err := s.Put(strings.NewReader("intact"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.Path(info.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = s.Verify(info.SHA256)
	if err == nil {
		t.Fatal("a corrupted blob passed verification")
	}
	if !strings.Contains(err.Error(), "corrupted") {
		t.Errorf("error = %q, want it to say the file is corrupted", err)
	}
}

func TestStoredBlobsAreOwnerOnly(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)

	info, err := s.Put(strings.NewReader("a receipt"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := s.Path(info.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := stat.Mode().Perm(); perm != filePerm {
		t.Errorf("blob mode = %o, want %o", perm, filePerm)
	}
}

func TestSweepClearsStagingFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	s, err := NewStore(filepath.Join(dir, "blobs"), tmp)
	if err != nil {
		t.Fatal(err)
	}

	// What a killed run leaves behind.
	for _, name := range []string{"blob-123", "blob-456"} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("half"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := s.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("swept %d staging files, want 2", removed)
	}
}

func TestNewStoreValidatesItsArguments(t *testing.T) {
	t.Parallel()
	if _, err := NewStore("", t.TempDir()); err == nil {
		t.Error("an empty root was accepted")
	}
	if _, err := NewStore(t.TempDir(), ""); err == nil {
		t.Error("an empty tmp was accepted")
	}
}
