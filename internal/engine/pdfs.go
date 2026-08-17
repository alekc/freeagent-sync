package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/blob"
	"github.com/alekc/freeagent-sync/internal/store"
	"github.com/alekc/freeagent-sync/internal/ui"
)

// PDFFamilies are the records FreeAgent renders a document for.
//
// These are the sales-side documents. Their PDF is the thing that was actually
// sent to a customer, so for an archive it is the counterpart of the scanned
// receipt on a bill: the evidence, not just the figures.
var PDFFamilies = []string{"invoices", "estimates", "credit_notes"}

// pdfContentType is what these renders are, whatever the API says.
const pdfContentType = "application/pdf"

// maxPDFBytes caps one render. Well above any real invoice, so it only fires
// on a response that is not what it claims to be.
const maxPDFBytes = 32 << 20

// RenderOptions configures a rendering pass.
type RenderOptions struct {
	// Families limits which record types are rendered. Empty means all of
	// PDFFamilies.
	Families []string
	// Limit caps how many documents this pass renders. Zero means all
	// outstanding.
	Limit int
	// Deadline stops the pass at a wall-clock time.
	Deadline time.Time
	// MaxRequests stops the pass after this many API calls.
	MaxRequests int64
}

// RenderResult is what a rendering pass achieved.
type RenderResult struct {
	Rendered  int
	Failed    int
	Remaining int
	Bytes     int64
	Errs      []error
}

// RenderDocuments fetches the PDF for every record whose document is missing or
// was rendered for an older version of it.
//
// Unlike attachments, these come from the API and so cost rate budget: one
// request per document. That is why the pass is incremental rather than
// wholesale, keyed on the parent's modification time, and why it is not part of
// a routine pull by default.
func (e *Engine) RenderDocuments(
	ctx context.Context, blobs *blob.Store, opts RenderOptions,
) (RenderResult, error) {
	var result RenderResult

	families := opts.Families
	if len(families) == 0 {
		families = PDFFamilies
	}

	pending, err := e.db.PendingDocuments(
		ctx, e.account.ID, store.DocumentKindPDF, families, opts.Limit)
	if err != nil {
		return result, err
	}
	if len(pending) == 0 {
		return result, nil
	}

	tracker := e.report.Track("documents", int64(len(pending)), ui.UnitsCount)
	defer tracker.Done()

	budget := newBudget(opts.MaxRequests, opts.Deadline, e.client)
	for i, task := range pending {
		if reason := budget.exceeded(ctx); reason != nil {
			result.Remaining = len(pending) - i
			break
		}

		size, err := e.renderOne(ctx, blobs, task)
		tracker.Add(1)
		if err != nil {
			result.Failed++
			result.Errs = append(result.Errs,
				fmt.Errorf("%s %s: %w", task.Family, task.RemoteID, err))
			continue
		}
		result.Rendered++
		result.Bytes += size
	}
	return result, nil
}

// renderOne fetches and stores a single document.
func (e *Engine) renderOne(
	ctx context.Context, blobs *blob.Store, task store.DocumentTask,
) (int64, error) {
	body, _, err := e.client.GetURL(
		ctx, freeagent.ResourceURL(task.ParentURL+"/pdf"), nil)
	if err != nil {
		return 0, err
	}

	decoded, err := decodePDF(body)
	if err != nil {
		return 0, err
	}
	if len(decoded) > maxPDFBytes {
		return 0, fmt.Errorf("the render is %d bytes, over the %d cap",
			len(decoded), int64(maxPDFBytes))
	}

	info, err := blobs.Put(bytes.NewReader(decoded))
	if err != nil {
		return 0, err
	}

	// rendered_for is the parent's modification time, so the next pass
	// re-renders exactly when the record moves and not otherwise.
	if err := e.db.SaveDocument(ctx, e.account.ID, task.ParentURL,
		store.DocumentKindPDF, info.SHA256, info.Size,
		pdfContentType, task.UpdatedAt); err != nil {
		return 0, err
	}
	return info.Size, nil
}

// decodePDF unwraps the base64 rendering.
func decodePDF(body []byte) ([]byte, error) {
	var envelope struct {
		PDF struct {
			Content string `json:"content"`
		} `json:"pdf"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("the render did not decode: %w", err)
	}
	if envelope.PDF.Content == "" {
		return nil, fmt.Errorf("the render carried no content")
	}

	decoded, err := base64.StdEncoding.DecodeString(envelope.PDF.Content)
	if err != nil {
		return nil, fmt.Errorf("the render is not valid base64: %w", err)
	}
	return decoded, nil
}
