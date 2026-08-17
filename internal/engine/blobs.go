package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/blob"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// DefaultBlobConcurrency is how many attachments download at once. Higher
// than the API concurrency because these go to a third-party host and spend
// no FreeAgent rate budget.
const DefaultBlobConcurrency = 8

// maxAttachmentBytes caps a single download. FreeAgent's own upload limit is
// 5MB; the slack absorbs any per-file metadata without letting a
// misconfigured URL stream forever into the archive.
const maxAttachmentBytes = 32 << 20

// blobTimeout bounds one download.
const blobTimeout = 5 * time.Minute

// BlobUserAgent identifies the downloader to the content host. Deliberately
// separate from the API user agent, because this client is not talking to
// FreeAgent and carries none of its credentials.
var BlobUserAgent = "fasync-attachments"

// BlobOptions configures a download pass.
type BlobOptions struct {
	// Concurrency is how many downloads run at once.
	Concurrency int
	// Limit caps how many attachments this pass takes on. Zero means all
	// outstanding.
	Limit int
	// Deadline stops the pass at a wall-clock time.
	Deadline time.Time
}

// BlobResult is what a download pass achieved.
type BlobResult struct {
	Attempted int
	Stored    int
	Failed    int
	Skipped   int
	Bytes     int64
	Errs      []error
}

// FetchBlobs downloads outstanding attachments into the blob store.
//
// Downloads go through a plain HTTP client that carries no OAuth token: the
// content URLs point at a third-party host, and sending FreeAgent credentials
// somewhere that is not FreeAgent would be a real leak. It also means these
// fetches cost none of the API rate budget.
func (e *Engine) FetchBlobs(
	ctx context.Context, blobs *blob.Store, opts BlobOptions,
) (BlobResult, error) {
	var result BlobResult
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultBlobConcurrency
	}

	pending, err := e.db.OutstandingAttachments(ctx, e.account.ID, opts.Limit)
	if err != nil {
		return result, err
	}
	if len(pending) == 0 {
		return result, nil
	}

	// Sized from the file_size the API already told us, so the bar is
	// determinate instead of rendering as ??? for the whole download. Where
	// no size is known, count attachments rather than bytes: an approximate
	// bar beats no bar.
	tracker := e.newBlobTracker(pending)
	defer tracker.Done()

	byBytes := knownBytes(pending) > 0
	client := &http.Client{Timeout: blobTimeout}
	queue := make(chan store.Attachment)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for range opts.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for att := range queue {
				bytes, err := e.fetchOne(ctx, client, blobs, att)

				mu.Lock()
				result.Attempted++
				switch {
				case err != nil:
					result.Failed++
					result.Errs = append(result.Errs,
						fmt.Errorf("%s: %w", att.FileName, err))
				default:
					result.Stored++
					result.Bytes += bytes
				}
				mu.Unlock()
				tracker.Add(progressFor(byBytes, bytes))
			}
		}()
	}

	for _, att := range pending {
		if ctx.Err() != nil || pastDeadline(opts.Deadline) {
			mu.Lock()
			result.Skipped++
			mu.Unlock()
			continue
		}
		queue <- att
	}
	close(queue)
	wg.Wait()

	return result, nil
}

// newBlobTracker sizes the download bar from the file sizes already known.
func (e *Engine) newBlobTracker(pending []store.Attachment) ui.Tracker {
	if total := knownBytes(pending); total > 0 {
		return e.report.Track("attachments", total, ui.UnitsBytes)
	}
	return e.report.Track("attachments", int64(len(pending)), ui.UnitsCount)
}

// knownBytes is how much of the pending set the API told us the size of.
func knownBytes(pending []store.Attachment) int64 {
	var total int64
	for _, att := range pending {
		if att.FileSize > 0 {
			total += att.FileSize
		}
	}
	return total
}

// progressFor advances by bytes when the bar is measured in them, and by one
// attachment when it is not.
func progressFor(byBytes bool, bytes int64) int64 {
	if byBytes {
		return bytes
	}
	return 1
}

// fetchOne downloads a single attachment, re-resolving its source first when
// the previous URL has expired.
func (e *Engine) fetchOne(
	ctx context.Context, client *http.Client, blobs *blob.Store, att store.Attachment,
) (int64, error) {
	if att.Expired(time.Now()) || att.ContentSrc == "" {
		refreshed, err := e.refreshAttachment(ctx, att)
		if err != nil {
			return 0, e.recordFailure(ctx, att, err)
		}
		att = refreshed
	}

	info, err := e.download(ctx, client, blobs, att)
	if err != nil {
		return 0, e.recordFailure(ctx, att, err)
	}
	if err := e.db.StoreBlob(
		ctx, e.account.ID, att.URL, info.SHA256, info.Size, att.ContentType); err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (e *Engine) download(
	ctx context.Context, client *http.Client, blobs *blob.Store, att store.Attachment,
) (blob.Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, att.ContentSrc, nil)
	if err != nil {
		return blob.Info{}, fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("User-Agent", BlobUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return blob.Info{}, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return blob.Info{}, fmt.Errorf("the content host answered %s", resp.Status)
	}

	info, err := blobs.Put(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		return blob.Info{}, err
	}
	if info.Size > maxAttachmentBytes {
		return blob.Info{}, fmt.Errorf(
			"the download exceeded the %d byte cap", int64(maxAttachmentBytes))
	}
	return info, nil
}

// refreshAttachment re-reads the metadata through the API to get a fresh
// content_src. This costs one API request, which is why it only happens when
// the stored URL has actually expired.
func (e *Engine) refreshAttachment(
	ctx context.Context, att store.Attachment,
) (store.Attachment, error) {
	meta, ok := freeagent.Resources["attachments"]
	if !ok {
		return att, errors.New("the SDK has no attachments entry")
	}

	body, _, err := e.client.GetURL(ctx, freeagent.ResourceURL(att.URL), nil)
	if err != nil {
		return att, err
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return att, fmt.Errorf("decoding the refreshed attachment: %w", err)
	}
	raw, ok := envelope[meta.Singular]
	if !ok {
		return att, fmt.Errorf("the refreshed attachment has no %q key", meta.Singular)
	}

	found, err := extractAttachments(att.Family, raw)
	if err != nil || len(found) == 0 {
		return att, errors.New("the refreshed attachment carried no content_src")
	}

	att.ContentSrc = found[0].ContentSrc
	att.ExpiresAt = found[0].ExpiresAt
	if err := e.db.RefreshAttachmentSource(
		ctx, e.account.ID, att.URL, att.ContentSrc, att.ExpiresAt); err != nil {
		return att, err
	}
	return att, nil
}

// recordFailure stores the reason and returns it, so a failed download is
// retried next run instead of blocking this one.
func (e *Engine) recordFailure(ctx context.Context, att store.Attachment, cause error) error {
	if err := e.db.FailAttachment(ctx, e.account.ID, att.URL, cause); err != nil {
		return err
	}
	return cause
}

func pastDeadline(deadline time.Time) bool {
	return !deadline.IsZero() && time.Now().After(deadline)
}
