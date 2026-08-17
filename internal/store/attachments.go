package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Attachment states.
const (
	// AttachmentPending is seen but not downloaded.
	AttachmentPending = "pending"
	// AttachmentStored has its bytes in the blob store.
	AttachmentStored = "stored"
	// AttachmentFailed exhausted its attempts for now and will be retried.
	AttachmentFailed = "failed"
)

// Attachment is one file hanging off a bill, expense or bank transaction
// explanation, as seen in the parent's payload.
type Attachment struct {
	URL         string
	ParentURL   string
	Family      string
	FileName    string
	ContentType string
	FileSize    int64
	ContentSrc  string
	ExpiresAt   time.Time
	SHA256      string
	State       string
	Attempts    int
	LastError   string
}

// Expired reports whether the download URL has passed its expiry. These are
// time-limited links on a third-party host, so a resumed run has to re-read
// the metadata before it can fetch the bytes.
func (a Attachment) Expired(now time.Time) bool {
	return !a.ExpiresAt.IsZero() && !now.Before(a.ExpiresAt)
}

// UpsertAttachments records attachments seen while archiving. Metadata is
// refreshed on every sighting, because content_src rotates, but the state and
// the digest are preserved: a file already downloaded must not go back to
// pending just because its parent was re-read.
func (d *DB) UpsertAttachments(
	ctx context.Context, accountID int64, attachments []Attachment,
) (int, error) {
	if len(attachments) == 0 {
		return 0, nil
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: recording attachments: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := FormatTime(time.Now())
	var written int
	for _, a := range attachments {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO attachments
			 (account_id, url, parent_url, family, file_name, content_type,
			  file_size, content_src, expires_at, state, first_seen_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (account_id, url) DO UPDATE SET
			   parent_url = excluded.parent_url,
			   family = excluded.family,
			   file_name = excluded.file_name,
			   content_type = excluded.content_type,
			   file_size = excluded.file_size,
			   content_src = excluded.content_src,
			   expires_at = excluded.expires_at`,
			accountID, a.URL, a.ParentURL, a.Family, a.FileName, a.ContentType,
			a.FileSize, a.ContentSrc, NullTime(a.ExpiresAt), AttachmentPending, now)
		if err != nil {
			return written, fmt.Errorf("store: recording attachment %s: %w", a.URL, err)
		}
		written++
	}

	if err := tx.Commit(); err != nil {
		return written, fmt.Errorf("store: recording attachments: %w", err)
	}
	return written, nil
}

// OutstandingAttachments lists what still needs downloading, least-attempted
// first so a persistently broken file cannot starve the rest.
func (d *DB) OutstandingAttachments(
	ctx context.Context, accountID int64, limit int,
) ([]Attachment, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := d.QueryContext(ctx,
		`SELECT url, parent_url, family, file_name, content_type, file_size,
		        content_src, expires_at, coalesce(sha256, ''), state, attempts,
		        coalesce(last_error, '')
		 FROM attachments
		 WHERE account_id = ? AND state <> ?
		 ORDER BY attempts, first_seen_at
		 LIMIT ?`, accountID, AttachmentStored, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing outstanding attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Attachment
	for rows.Next() {
		var (
			a       Attachment
			expires sql.NullString
		)
		if err := rows.Scan(&a.URL, &a.ParentURL, &a.Family, &a.FileName,
			&a.ContentType, &a.FileSize, &a.ContentSrc, &expires, &a.SHA256,
			&a.State, &a.Attempts, &a.LastError); err != nil {
			return nil, fmt.Errorf("store: listing outstanding attachments: %w", err)
		}
		if a.ExpiresAt, err = parseNullTime(expires); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing outstanding attachments: %w", err)
	}
	return out, nil
}

// RefreshAttachmentSource records a re-resolved download URL.
func (d *DB) RefreshAttachmentSource(
	ctx context.Context, accountID int64, url, contentSrc string, expiresAt time.Time,
) error {
	_, err := d.ExecContext(ctx,
		`UPDATE attachments SET content_src = ?, expires_at = ?
		 WHERE account_id = ? AND url = ?`,
		contentSrc, NullTime(expiresAt), accountID, url)
	if err != nil {
		return fmt.Errorf("store: refreshing %s: %w", url, err)
	}
	return nil
}

// StoreBlob records a downloaded blob and links the attachment to it, in one
// transaction so an attachment can never point at a blob row that is missing.
func (d *DB) StoreBlob(
	ctx context.Context, accountID int64, attachmentURL, digest string,
	size int64, contentType string,
) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: recording a blob: %w", err)
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
		`UPDATE attachments SET sha256 = ?, state = ?, last_error = NULL
		 WHERE account_id = ? AND url = ?`,
		digest, AttachmentStored, accountID, attachmentURL); err != nil {
		return fmt.Errorf("store: linking %s: %w", attachmentURL, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: recording a blob: %w", err)
	}
	return nil
}

// FailAttachment records a failed download. Attempts accumulate so a
// permanently broken file drifts to the back of the queue instead of being
// retried ahead of everything else on every run.
func (d *DB) FailAttachment(
	ctx context.Context, accountID int64, url string, cause error,
) error {
	_, err := d.ExecContext(ctx,
		`UPDATE attachments
		 SET state = ?, attempts = attempts + 1, last_error = ?
		 WHERE account_id = ? AND url = ?`,
		AttachmentFailed, cause.Error(), accountID, url)
	if err != nil {
		return fmt.Errorf("store: recording the failure of %s: %w", url, err)
	}
	return nil
}

// AttachmentCounts summarises the blob queue for status output.
type AttachmentCounts struct {
	Total   int64
	Stored  int64
	Pending int64
	Failed  int64
	Bytes   int64
}

// AttachmentCounts reports the state of the blob queue.
func (d *DB) AttachmentCounts(ctx context.Context, accountID int64) (AttachmentCounts, error) {
	var c AttachmentCounts
	err := d.QueryRowContext(ctx,
		`SELECT count(*),
		        count(*) FILTER (WHERE state = ?),
		        count(*) FILTER (WHERE state = ?),
		        count(*) FILTER (WHERE state = ?)
		 FROM attachments WHERE account_id = ?`,
		AttachmentStored, AttachmentPending, AttachmentFailed, accountID).
		Scan(&c.Total, &c.Stored, &c.Pending, &c.Failed)
	if err != nil {
		return c, fmt.Errorf("store: counting attachments: %w", err)
	}

	// Sized from the blobs actually referenced, so the same file attached
	// twice is counted once.
	err = d.QueryRowContext(ctx,
		`SELECT coalesce(sum(size), 0) FROM blobs
		 WHERE sha256 IN (SELECT DISTINCT sha256 FROM attachments
		                  WHERE account_id = ? AND sha256 IS NOT NULL)`,
		accountID).Scan(&c.Bytes)
	if err != nil {
		return c, fmt.Errorf("store: sizing attachments: %w", err)
	}
	return c, nil
}

// StoredAttachments lists everything downloaded, for verification and for
// building the browsable file trees.
func (d *DB) StoredAttachments(ctx context.Context, accountID int64) ([]Attachment, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT url, parent_url, family, file_name, content_type, file_size,
		        coalesce(sha256, '')
		 FROM attachments
		 WHERE account_id = ? AND state = ? AND sha256 IS NOT NULL
		 ORDER BY parent_url, url`, accountID, AttachmentStored)
	if err != nil {
		return nil, fmt.Errorf("store: listing stored attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Attachment
	for rows.Next() {
		var a Attachment
		if err := rows.Scan(&a.URL, &a.ParentURL, &a.Family, &a.FileName,
			&a.ContentType, &a.FileSize, &a.SHA256); err != nil {
			return nil, fmt.Errorf("store: listing stored attachments: %w", err)
		}
		a.State = AttachmentStored
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing stored attachments: %w", err)
	}
	return out, nil
}
