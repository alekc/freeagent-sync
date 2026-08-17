package tree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alekc/freeagent-sync/internal/blob"
	"github.com/alekc/freeagent-sync/internal/store"
)

// LinkMode is how a view entry points at its blob.
type LinkMode string

const (
	// LinkHardlink is the default. A hardlink is a directory entry pointing at
	// the same bytes, so the entry's own name is the file's name: a viewer
	// opening receipt.pdf sees a PDF.
	//
	// This matters more than it sounds. A symlink is resolved before the name
	// is examined, and the blobs are named by content hash with no extension,
	// so following one lands on a file that looks like nothing and opens in a
	// text editor. The browsable tree exists to be double-clicked, and with
	// symlinks it could not be.
	LinkHardlink LinkMode = "hardlink"
	// LinkSymlink survives the blob store being on another filesystem, at the
	// cost of the naming problem above.
	LinkSymlink LinkMode = "symlink"
	// LinkCopy duplicates the bytes. Last resort: it doubles the space the
	// attachments take.
	LinkCopy LinkMode = "copy"
)

// ParseLinkMode validates a --link-mode value.
func ParseLinkMode(s string) (LinkMode, error) {
	switch LinkMode(s) {
	case LinkSymlink, LinkHardlink, LinkCopy:
		return LinkMode(s), nil
	case "auto":
		return defaultLinkMode(), nil
	}
	return "", fmt.Errorf("tree: unknown link mode %q, want hardlink, symlink, copy or auto", s)
}

func defaultLinkMode() LinkMode { return LinkHardlink }

// FileStats reports what a relink produced.
type FileStats struct {
	Links   int
	Missing int
	Skipped int
	// Mode is what was actually used, which can differ from what was asked for
	// when hardlinking is impossible.
	Mode LinkMode
}

// BuildFiles regenerates the browsable views of the attachments.
//
// Three views over the same blobs, none of which duplicates the bytes:
//
//	files/by-date/2026/03/2026-03-14-bills-1234-receipt.pdf
//	files/by-family/bills/1234/receipt.pdf
//	files/by-contact/acme-ltd/2026-03-14-bills-1234-receipt.pdf
//
// Regenerated wholesale rather than patched, which is affordable precisely
// because links cost nothing.
func BuildFiles(
	ctx context.Context, db *store.DB, blobs *blob.Store,
	accountID int64, root string, mode LinkMode,
) (FileStats, error) {
	var stats FileStats
	if mode == "" {
		mode = defaultLinkMode()
	}

	attachments, err := db.StoredAttachments(ctx, accountID)
	if err != nil {
		return stats, err
	}
	// Rendered invoices and estimates are documents too, and belong in the
	// same views: from a browsing point of view a sent invoice and a received
	// receipt are the same kind of thing.
	rendered, err := renderedAsAttachments(ctx, db, accountID)
	if err != nil {
		return stats, err
	}
	attachments = append(attachments, rendered...)

	parents, err := loadParents(ctx, db, accountID)
	if err != nil {
		return stats, err
	}

	staging, commit, err := stageDir(root)
	if err != nil {
		return stats, err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	namer := newUniqueNamer()
	entries := &linker{mode: mode}
	for _, att := range attachments {
		if err := ctx.Err(); err != nil {
			return stats, err
		}

		target, err := blobs.Path(att.SHA256)
		if err != nil {
			stats.Skipped++
			continue
		}
		// The database says it is stored; if the file is not there the view
		// simply omits it rather than creating a link that dangles.
		if _, err := os.Stat(target); err != nil {
			stats.Missing++
			continue
		}

		for _, v := range viewsFor(att, parents[att.ParentURL]) {
			name := namer.unique(v.path, v.name, att.SHA256)
			path := filepath.Join(staging, v.path, name)
			if err := entries.link(path, target); err != nil {
				return stats, err
			}
			stats.Links++
		}
	}

	// What was actually used, which differs from what was asked for when
	// hardlinking turned out to be impossible.
	stats.Mode = entries.mode

	if err := commit(); err != nil {
		return stats, err
	}
	return stats, nil
}

// renderedAsAttachments presents generated documents in the same shape as
// attachments, so the view builder has one thing to iterate rather than two
// nearly identical ones.
func renderedAsAttachments(
	ctx context.Context, db *store.DB, accountID int64,
) ([]store.Attachment, error) {
	docs, err := db.StoredDocuments(ctx, accountID)
	if err != nil {
		return nil, err
	}

	out := make([]store.Attachment, 0, len(docs))
	for _, doc := range docs {
		if doc.Family == "" {
			continue
		}
		out = append(out, store.Attachment{
			URL:       doc.ParentURL + "/" + doc.Kind,
			ParentURL: doc.ParentURL,
			Family:    doc.Family,
			// Named for the record it renders, since a generated document has
			// no file name of its own.
			FileName:    store.IDFromURL(doc.ParentURL) + "." + doc.Kind,
			ContentType: "application/" + doc.Kind,
			SHA256:      doc.SHA256,
			State:       store.AttachmentStored,
		})
	}
	return out, nil
}

// view is one place an attachment appears.
type view struct {
	path string
	name string
}

// parent is what a view needs from the record an attachment hangs off.
type parent struct {
	Date    string
	Contact string
	Ref     string
}

// viewsFor decides where one attachment shows up. by-date needs the parent's
// business date and by-contact needs its contact, so an attachment whose
// parent has neither appears only under by-family.
func viewsFor(att store.Attachment, p parent) []view {
	file := FileName(att.FileName)
	if file == "unnamed" {
		file = Slug(store.IDFromURL(att.URL)) + extensionFor(att.ContentType)
	}
	id := Slug(store.IDFromURL(att.ParentURL))

	views := []view{{
		path: filepath.Join("by-family", Slug(att.Family), id),
		name: file,
	}}

	descriptive := strings.Trim(strings.Join(
		[]string{p.Date, Slug(att.Family), id, file}, "-"), "-")

	if year, month, ok := splitDate(p.Date); ok {
		views = append(views, view{
			path: filepath.Join("by-date", year, month),
			name: descriptive,
		})
	}
	if p.Contact != "" {
		views = append(views, view{
			path: filepath.Join("by-contact", Slug(p.Contact)),
			name: descriptive,
		})
	}
	return views
}

func splitDate(date string) (year, month string, ok bool) {
	parts := strings.SplitN(date, "-", 3)
	if len(parts) < 2 || len(parts[0]) != 4 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// loadParents reads the dates and contact names the views are organised by,
// resolving each contact URL to its name so the tree reads as a person rather
// than as an id.
func loadParents(
	ctx context.Context, db *store.DB, accountID int64,
) (map[string]parent, error) {
	contacts := map[string]string{}
	bodies, err := db.LiveRecordBodies(ctx, accountID, "contacts")
	if err != nil {
		return nil, err
	}
	for _, body := range bodies {
		var c struct {
			URL              string `json:"url"`
			OrganisationName string `json:"organisation_name"`
			FirstName        string `json:"first_name"`
			LastName         string `json:"last_name"`
		}
		if err := json.Unmarshal(body, &c); err != nil {
			continue
		}
		name := c.OrganisationName
		if name == "" {
			name = strings.TrimSpace(c.FirstName + " " + c.LastName)
		}
		if c.URL != "" && name != "" {
			contacts[c.URL] = name
		}
	}

	parents := map[string]parent{}
	err = db.EachRecord(ctx, accountID, func(rec store.RecordRow) error {
		fields, ok := parentFields(rec.Body)
		if !ok {
			return nil
		}
		parents[rec.URL] = parent{
			Date:    fields.DatedOn,
			Contact: contacts[fields.Contact],
			Ref:     fields.Reference,
		}
		return nil
	})
	return parents, err
}

// parentFields reads the handful of fields the views organise by. A record
// that carries none of them, or carries them with a different type, simply
// gets no by-date or by-contact entry: that is not a reason to fail a
// rebuild of the whole tree.
func parentFields(body []byte) (fields struct {
	DatedOn   string `json:"dated_on"`
	Contact   string `json:"contact"`
	Reference string `json:"reference"`
}, ok bool,
) {
	if json.Unmarshal(body, &fields) != nil {
		return fields, false
	}
	return fields, true
}

// linker points entries at blobs, downgrading once if the chosen mode turns
// out to be impossible.
type linker struct{ mode LinkMode }

// link points path at target. A hardlink that fails because the blob store is
// on another filesystem downgrades the whole tree to symlinks rather than
// failing: a tree that opens awkwardly is better than no tree.
func (l *linker) link(path, target string) error {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return fmt.Errorf("tree: creating %s: %w", filepath.Dir(path), err)
	}

	switch l.mode {
	case LinkCopy:
		return copyFile(path, target)

	case LinkSymlink:
		return symlink(path, target)

	default:
		if err := os.Link(target, path); err != nil {
			if !errors.Is(err, syscall.EXDEV) && !errors.Is(err, syscall.ENOTSUP) {
				return fmt.Errorf("tree: hardlinking %s: %w", path, err)
			}
			l.mode = LinkSymlink
			return symlink(path, target)
		}
		return nil
	}
}

// symlink points at the target relatively, so the whole data directory can be
// moved without breaking every entry.
func symlink(path, target string) error {
	rel, err := filepath.Rel(filepath.Dir(path), target)
	if err != nil {
		rel = target
	}
	if err := os.Symlink(rel, path); err != nil {
		return fmt.Errorf("tree: linking %s: %w", path, err)
	}
	return nil
}

func copyFile(path, target string) error {
	// target is a validated blob path and path is built from slugged
	// components, neither of which can carry a separator.
	//nolint:gosec // G304: both paths are constructed, not supplied
	src, err := os.Open(target)
	if err != nil {
		return fmt.Errorf("tree: reading %s: %w", target, err)
	}
	defer func() { _ = src.Close() }()

	//nolint:gosec // G304: see above
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return fmt.Errorf("tree: creating %s: %w", path, err)
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("tree: copying to %s: %w", path, err)
	}
	return dst.Close()
}

// extensionFor guesses an extension for an attachment with no usable file
// name, so a receipt still opens in the right application.
func extensionFor(contentType string) string {
	switch strings.ToLower(strings.SplitN(contentType, ";", 2)[0]) {
	case "application/pdf":
		return ".pdf"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/tiff":
		return ".tiff"
	case "image/bmp":
		return ".bmp"
	case "text/csv":
		return ".csv"
	case "application/zip":
		return ".zip"
	}
	return ""
}
