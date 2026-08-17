package tree

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alekc/freeagent-sync/internal/store"
)

const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// RecordStats reports what a rebuild wrote.
type RecordStats struct {
	Records  int
	Versions int
}

// BuildRecords writes every archived record out as browsable JSON.
//
// Layout, one file per record with its history beside it:
//
//	records/<family>/<id>.json
//	records/<family>/<id>.versions/<timestamp>-<sha8>.json
//
// The tree is replaced rather than patched, because it is derived: rebuilding
// from scratch is the only way to guarantee it matches the archive after a
// record has been deleted or an id has changed shape.
func BuildRecords(
	ctx context.Context, db *store.DB, accountID int64, root string,
) (RecordStats, error) {
	var stats RecordStats

	staging, commit, err := stageDir(root)
	if err != nil {
		return stats, err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	// Built during the records pass so the versions pass can find each
	// record's family without a query per record.
	family := make(map[string]string)

	err = db.EachRecord(ctx, accountID, func(rec store.RecordRow) error {
		family[rec.URL] = rec.Family
		path := filepath.Join(staging, Slug(rec.Family), Slug(rec.RemoteID)+".json")
		if err := writeJSON(path, rec.Body); err != nil {
			return err
		}
		stats.Records++
		return ctx.Err()
	})
	if err != nil {
		return stats, err
	}

	// Version files are named by when the body was seen and by a short hash,
	// so the directory sorts chronologically and a re-run overwrites the same
	// names instead of accumulating duplicates.
	err = db.EachVersion(ctx, accountID, func(v store.VersionRow) error {
		// A version whose record is gone is skipped rather than written to a
		// directory nothing points at.
		fam, ok := family[v.URL]
		if !ok {
			return nil
		}
		name := v.SeenAt.UTC().Format("2006-01-02T15-04-05Z") + "-" + short(v.SHA256) + ".json"
		path := filepath.Join(staging, Slug(fam), Slug(store.IDFromURL(v.URL))+".versions", name)
		if err := writeJSON(path, v.Body); err != nil {
			return err
		}
		stats.Versions++
		return ctx.Err()
	})
	if err != nil {
		return stats, err
	}

	if err := commit(); err != nil {
		return stats, err
	}
	return stats, nil
}

// writeJSON writes an indented, key-sorted copy so ordinary diff tools work
// on the history.
func writeJSON(path string, body []byte) error {
	pretty, err := prettyJSON(body)
	if err != nil {
		return fmt.Errorf("tree: formatting %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("tree: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, pretty, filePerm); err != nil {
		return fmt.Errorf("tree: writing %s: %w", path, err)
	}
	return nil
}

// prettyJSON indents a body. Decoding through a map would sort the keys but
// would also turn every exact decimal string into a float, so the bytes are
// indented as they are and the key order stays whatever FreeAgent sent.
func prettyJSON(body []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

func short(digest string) string {
	if len(digest) > 8 {
		return digest[:8]
	}
	return digest
}

// stageDir builds the tree in a sibling directory and swaps it in at the end,
// so an interrupted rebuild leaves the previous tree intact rather than a
// half-written one.
func stageDir(root string) (staging string, commit func() error, err error) {
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, dirPerm); err != nil {
		return "", nil, fmt.Errorf("tree: creating %s: %w", parent, err)
	}
	staging = root + ".building"
	if err := os.RemoveAll(staging); err != nil {
		return "", nil, fmt.Errorf("tree: clearing %s: %w", staging, err)
	}
	if err := os.MkdirAll(staging, dirPerm); err != nil {
		return "", nil, fmt.Errorf("tree: creating %s: %w", staging, err)
	}

	commit = func() error {
		previous := root + ".previous"
		_ = os.RemoveAll(previous)
		if _, err := os.Stat(root); err == nil {
			if err := os.Rename(root, previous); err != nil {
				return fmt.Errorf("tree: rotating %s: %w", root, err)
			}
		}
		if err := os.Rename(staging, root); err != nil {
			return fmt.Errorf("tree: installing %s: %w", root, err)
		}
		return os.RemoveAll(previous)
	}
	return staging, commit, nil
}
