// Package blob is a content-addressed file store for attachments.
//
// Files are named by the SHA-256 of their contents, which gives dedupe for
// free (accountants re-attach the same receipt) and makes integrity checking
// a re-hash rather than a comparison against something else that could also
// be wrong.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// digestPattern is what a valid content address looks like. Checked before
// any digest reaches a file path, so a malformed one cannot escape the store.
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrNotStored reports a digest the store does not hold.
var ErrNotStored = errors.New("blob: not stored")

// Store holds blobs under root, staging partial writes in tmp.
type Store struct {
	root string
	tmp  string
}

// Info describes a stored blob.
type Info struct {
	SHA256 string
	Size   int64
}

// NewStore prepares the directories. tmp must be on the same filesystem as
// root, because the final step is a rename and a rename across filesystems
// is not atomic.
func NewStore(root, tmp string) (*Store, error) {
	if root == "" || tmp == "" {
		return nil, errors.New("blob: NewStore requires a root and a tmp directory")
	}
	for _, dir := range []string{root, tmp} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return nil, fmt.Errorf("blob: creating %s: %w", dir, err)
		}
	}
	return &Store{root: root, tmp: tmp}, nil
}

// Root reports the directory holding the blobs.
func (s *Store) Root() string { return s.root }

// Put streams a reader into the store and returns its content address.
//
// The digest is not known until the last byte, so the data is written to tmp
// and renamed into place only once it is. A killed run therefore leaves a
// stray temporary file, never a truncated blob under a digest that does not
// describe it.
func (s *Store) Put(r io.Reader) (Info, error) {
	var info Info

	temp, err := os.CreateTemp(s.tmp, "blob-*")
	if err != nil {
		return info, fmt.Errorf("blob: staging a download: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(temp, hash), r)
	if err != nil {
		return info, fmt.Errorf("blob: writing a download: %w", err)
	}
	if err := temp.Close(); err != nil {
		return info, fmt.Errorf("blob: closing a download: %w", err)
	}

	info = Info{SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size}
	final, err := s.Path(info.SHA256)
	if err != nil {
		return info, err
	}

	// Already held: the same file attached twice, or a retry after the rename
	// but before the database caught up. Either way the bytes are correct.
	if _, err := os.Stat(final); err == nil {
		return info, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return info, fmt.Errorf("blob: creating a blob directory: %w", err)
	}
	if err := os.Rename(tempName, final); err != nil {
		return info, fmt.Errorf("blob: storing %s: %w", info.SHA256, err)
	}
	if err := os.Chmod(final, filePerm); err != nil {
		return info, fmt.Errorf("blob: tightening permissions on %s: %w", info.SHA256, err)
	}
	return info, nil
}

// Path is where a digest lives. Two levels of fan-out keep any one directory
// from growing to tens of thousands of entries.
func (s *Store) Path(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("blob: %q is not a sha256 digest", digest)
	}
	return filepath.Join(s.root, digest[0:2], digest[2:4], digest), nil
}

// Has reports whether the store holds a digest.
func (s *Store) Has(digest string) bool {
	path, err := s.Path(digest)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Open returns a reader for a stored blob.
func (s *Store) Open(digest string) (io.ReadCloser, error) {
	path, err := s.Path(digest)
	if err != nil {
		return nil, err
	}
	// The path came from Path, which rejects anything that is not 64 hex
	// characters, so there is no caller-controlled component in it.
	//nolint:gosec // G304: the path is derived from a validated digest
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotStored, digest)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: opening %s: %w", digest, err)
	}
	return f, nil
}

// Verify re-hashes a stored blob and reports whether it still matches the
// name it is filed under.
func (s *Store) Verify(digest string) error {
	f, err := s.Open(digest)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return fmt.Errorf("blob: reading %s: %w", digest, err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != digest {
		return fmt.Errorf("blob: %s hashes to %s; the file has been corrupted", digest, got)
	}
	return nil
}

// Sweep removes leftover staging files, which a killed run leaves behind.
func (s *Store) Sweep() (int, error) {
	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		return 0, fmt.Errorf("blob: reading the staging directory: %w", err)
	}
	var removed int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(s.tmp, entry.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}
