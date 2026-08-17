package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// DocumentKindPDF is the only generated document kind so far: the rendering
// FreeAgent produces for an invoice, estimate or credit note.
const DocumentKindPDF = "pdf"

// DocumentTask is a record whose generated document is missing or stale.
type DocumentTask struct {
	ParentURL string
	Family    string
	RemoteID  string
	// UpdatedAt is the parent's modification time as stored. Rendering is
	// keyed on it, so a document is re-rendered exactly when its record moves.
	UpdatedAt string
}

// PendingDocuments lists records in the given families whose document has not
// been rendered, or was rendered for an older version of the record.
//
// Rendering costs one API request per document, so this is deliberately
// incremental: a routine run re-renders only what actually changed.
func (d *DB) PendingDocuments(
	ctx context.Context, accountID int64, kind string, families []string, limit int,
) ([]DocumentTask, error) {
	if len(families) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10000
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(families)), ",")
	args := []any{kind, accountID}
	for _, family := range families {
		args = append(args, family)
	}
	args = append(args, limit)

	rows, err := d.QueryContext(ctx,
		`SELECT r.url, r.family, r.remote_id, coalesce(r.remote_updated_at, '')
		 FROM records r
		 LEFT JOIN documents docs
		   ON docs.account_id = r.account_id
		   AND docs.parent_url = r.url
		   AND docs.kind = ?
		 WHERE r.account_id = ?
		   AND r.family IN (`+placeholders+`)
		   AND r.deleted_at IS NULL
		   AND (docs.sha256 IS NULL
		        OR coalesce(docs.rendered_for_updated_at, '')
		           <> coalesce(r.remote_updated_at, ''))
		 ORDER BY r.family, r.url
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing documents to render: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []DocumentTask
	for rows.Next() {
		var task DocumentTask
		if err := rows.Scan(&task.ParentURL, &task.Family,
			&task.RemoteID, &task.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: listing documents to render: %w", err)
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

// SaveDocument records a rendered document and the blob holding it, in one
// transaction so a document can never point at a blob row that is missing.
func (d *DB) SaveDocument(
	ctx context.Context, accountID int64, parentURL, kind, digest string,
	size int64, contentType, renderedFor string,
) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: recording a document: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := FormatTime(time.Now())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO blobs (sha256, size, content_type, stored_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (sha256) DO UPDATE SET
		   content_type = CASE WHEN excluded.content_type <> ''
		                       THEN excluded.content_type ELSE blobs.content_type END`,
		digest, size, contentType, now); err != nil {
		return fmt.Errorf("store: recording blob %s: %w", digest, err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO documents
		 (account_id, parent_url, kind, sha256, rendered_at, rendered_for_updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (account_id, parent_url, kind) DO UPDATE SET
		   sha256 = excluded.sha256,
		   rendered_at = excluded.rendered_at,
		   rendered_for_updated_at = excluded.rendered_for_updated_at`,
		accountID, parentURL, kind, digest, now,
		nullString(renderedFor)); err != nil {
		return fmt.Errorf("store: recording the document for %s: %w", parentURL, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: recording a document: %w", err)
	}
	return nil
}

// Document is a rendered document, for verification and the file trees.
type Document struct {
	ParentURL  string
	Kind       string
	SHA256     string
	Family     string
	RenderedAt time.Time
}

// StoredDocuments lists every rendered document with the family of the record
// it belongs to.
func (d *DB) StoredDocuments(ctx context.Context, accountID int64) ([]Document, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT docs.parent_url, docs.kind, coalesce(docs.sha256, ''),
		        coalesce(r.family, ''), docs.rendered_at
		 FROM documents docs
		 LEFT JOIN records r
		   ON r.account_id = docs.account_id AND r.url = docs.parent_url
		 WHERE docs.account_id = ? AND docs.sha256 IS NOT NULL
		 ORDER BY docs.parent_url`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: listing documents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Document
	for rows.Next() {
		var (
			doc      Document
			rendered sql.NullString
		)
		if err := rows.Scan(&doc.ParentURL, &doc.Kind, &doc.SHA256,
			&doc.Family, &rendered); err != nil {
			return nil, fmt.Errorf("store: listing documents: %w", err)
		}
		if doc.RenderedAt, err = parseNullTime(rendered); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

// DocumentCount reports how many documents of a kind are stored.
func (d *DB) DocumentCount(ctx context.Context, accountID int64, kind string) (int64, error) {
	var n int64
	err := d.QueryRowContext(ctx,
		`SELECT count(*) FROM documents
		 WHERE account_id = ? AND kind = ? AND sha256 IS NOT NULL`,
		accountID, kind).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting documents: %w", err)
	}
	return n, nil
}
