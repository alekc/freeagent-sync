// Package explain is the only code in this tool that can write to FreeAgent.
//
// It is a separate package from internal/api on purpose. That package has no
// writable constructor at all, so the read path cannot gain write capability
// by passing a different argument, and a reader of either package can tell at
// a glance which one they are looking at.
//
// The write surface is bounded twice over. This client reaches exactly two
// operations, and each refuses unless a precondition read against the live API
// says the record still has a hole to fill: a transaction with an unexplained
// balance, or an explanation carrying no attachment. Neither can overwrite a
// value a human already set, so the worst case is an unwanted record rather
// than a lost one.
//
// What no precondition can catch is a well-formed explanation posted to the
// wrong category. That is why every attempt, successful or not, is appended to
// an audit file before the caller is told what happened.
package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alekc/freeagent"
	"github.com/shopspring/decimal"
	"golang.org/x/oauth2"
)

// requestTimeout bounds one call. A receipt upload carries up to
// MaxAttachmentBytes over the same connection, so this is more generous than
// a plain read would need.
const requestTimeout = 2 * time.Minute

// Guard failures. They are values rather than formatted strings because a
// caller has to be able to tell "the guard stopped me" from "the API is
// broken", and report the first as an ordinary answer.
var (
	// ErrAlreadyExplained means the transaction has no unexplained balance
	// left, so there is nothing here to fill in.
	ErrAlreadyExplained = errors.New(
		"the transaction is already fully explained, so nothing was written")
	// ErrOverExplained means the value asked for is larger than what remains.
	ErrOverExplained = errors.New(
		"the value is larger than the transaction's unexplained amount")
	// ErrSignMismatch means money-in was offered for a money-out line or the
	// reverse, which would be an explanation of the wrong direction.
	ErrSignMismatch = errors.New(
		"the value runs in the opposite direction to the unexplained amount")
	// ErrAlreadyAttached means the explanation carries a file already.
	ErrAlreadyAttached = errors.New(
		"the explanation already carries an attachment, which is never replaced")
)

// contentTypes FreeAgent accepts for an attachment. Checked here so an
// oversized or unsupported upload fails locally instead of after transferring
// several megabytes and coming back as a 422.
var contentTypes = map[string]bool{
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"application/pdf": true,
}

// Options configures the writable client.
type Options struct {
	Environment       freeagent.Environment
	TokenSource       oauth2.TokenSource
	UserAgent         string
	RequestsPerMinute int
	RequestsPerHour   int

	// Account is the slug recorded on every audit line, so a file covering
	// several companies stays readable.
	Account string
	// AuditPath is the file every attempt is appended to. It is required: a
	// write nobody can review afterwards is not one this package will make.
	AuditPath string
}

// Client writes to FreeAgent, narrowly.
type Client struct {
	fa      *freeagent.Client
	env     freeagent.Environment
	account string

	// mu serialises audit appends. Two tool calls can land concurrently and
	// a torn line would defeat the point of keeping the file.
	mu    sync.Mutex
	audit string
}

// New builds the writable client. Unlike api.NewReadOnly it omits
// WithReadOnly, which is the entire difference and the reason this
// constructor lives in its own package.
func New(opts Options) (*Client, error) {
	switch {
	case opts.TokenSource == nil:
		return nil, errors.New("explain: a token source is required")
	case opts.Environment.BaseURL == "":
		return nil, errors.New("explain: an environment is required")
	case opts.AuditPath == "":
		return nil, errors.New("explain: an audit path is required")
	}

	settings := []freeagent.Option{
		freeagent.WithBaseURL(opts.Environment.BaseURL),
		freeagent.WithTokenSource(opts.TokenSource),
		freeagent.WithUserAgent(opts.UserAgent),
	}
	if opts.RequestsPerMinute > 0 || opts.RequestsPerHour > 0 {
		settings = append(settings,
			freeagent.WithRateLimits(opts.RequestsPerMinute, opts.RequestsPerHour))
	}

	fa, err := freeagent.NewClient(settings...)
	if err != nil {
		return nil, fmt.Errorf("explain: building the client: %w", err)
	}
	return &Client{
		fa: fa, env: opts.Environment,
		account: opts.Account, audit: opts.AuditPath,
	}, nil
}

// Receipt is a file to attach to a record.
type Receipt struct {
	FileName    string
	ContentType string
	Description string
	Data        []byte
}

func (r *Receipt) validate() error {
	switch {
	case r == nil:
		return nil
	case r.FileName == "":
		return errors.New("the receipt needs a file name")
	case !contentTypes[r.ContentType]:
		return fmt.Errorf(
			"content type %q is not one FreeAgent accepts: image/png, "+
				"image/jpeg, image/gif or application/pdf", r.ContentType)
	case len(r.Data) == 0:
		return errors.New("the receipt is empty")
	case len(r.Data) > freeagent.MaxAttachmentBytes:
		return fmt.Errorf("the receipt is %d bytes, the limit is %d",
			len(r.Data), freeagent.MaxAttachmentBytes)
	}
	return nil
}

func (r *Receipt) attachment() *freeagent.Attachment {
	if r == nil {
		return nil
	}
	return &freeagent.Attachment{
		Data:        r.Data,
		FileName:    r.FileName,
		ContentType: r.ContentType,
		Description: r.Description,
	}
}

// Request describes one explanation to create.
type Request struct {
	Transaction freeagent.ResourceURL
	Category    freeagent.ResourceURL
	GrossValue  decimal.Decimal
	DatedOn     freeagent.Date
	Description string
	Receipt     *Receipt
}

// Result reports what was written, including the balance the guard saw, so
// the caller can show its working rather than asserting success.
type Result struct {
	Explanation    freeagent.ResourceURL `json:"explanation"`
	Transaction    freeagent.ResourceURL `json:"transaction,omitempty"`
	GrossValue     string                `json:"gross_value,omitempty"`
	UnexplainedWas string                `json:"unexplained_before,omitempty"`
	RemainingAfter string                `json:"unexplained_after,omitempty"`
	AttachedName   string                `json:"attached_file,omitempty"`
	AttachedBytes  int                   `json:"attached_bytes,omitempty"`
	AuditedAt      time.Time             `json:"audited_at"`
}

// Explain creates an explanation for a transaction that still has an
// unexplained balance.
//
// The transaction is re-read here rather than trusted from the request. A
// caller that decided it was unexplained several turns ago, or read it from
// the archive, is acting on a fact that may already be false, and the guard
// has to bite on what is true now.
func (c *Client) Explain(ctx context.Context, req Request) (*Result, error) {
	if req.Transaction.IsZero() {
		return nil, errors.New("a bank transaction URL is required")
	}
	if req.Category.IsZero() {
		return nil, errors.New("a category URL is required")
	}
	if err := req.Receipt.validate(); err != nil {
		return nil, err
	}

	txn, err := c.transaction(ctx, req.Transaction)
	if err != nil {
		return nil, c.record("explain", req.auditFields(), nil, err)
	}

	remaining := decimal.Decimal{}
	if txn.UnexplainedAmount != nil {
		remaining = *txn.UnexplainedAmount
	}
	if err := checkAmount(req.GrossValue, remaining); err != nil {
		return nil, c.record("explain", req.auditFields(), nil, err)
	}

	dated := req.DatedOn
	if dated.IsZero() {
		dated = txn.DatedOn
	}

	value := req.GrossValue
	created, _, err := c.fa.BankTransactionExplanations.Create(ctx,
		&freeagent.BankTransactionExplanation{
			BankTransaction: req.Transaction,
			BankAccount:     txn.BankAccount,
			DatedOn:         dated,
			GrossValue:      &value,
			Category:        req.Category,
			Description:     req.Description,
			Attachment:      req.Receipt.attachment(),
		})
	if err != nil {
		return nil, c.record("explain", req.auditFields(), nil,
			fmt.Errorf("creating the explanation: %w", err))
	}

	res := &Result{
		Explanation:    created.URL,
		Transaction:    req.Transaction,
		GrossValue:     req.GrossValue.String(),
		UnexplainedWas: remaining.String(),
		RemainingAfter: remaining.Sub(req.GrossValue).String(),
	}
	if req.Receipt != nil {
		res.AttachedName = req.Receipt.FileName
		res.AttachedBytes = len(req.Receipt.Data)
	}
	return res, c.record("explain", req.auditFields(), res, nil)
}

// AttachRequest describes a file to add to an existing explanation.
type AttachRequest struct {
	Explanation freeagent.ResourceURL
	Receipt     Receipt
}

// AttachReceipt adds a file to an explanation that has none.
//
// Only the attachment is sent, so the update carries no other field that
// could overwrite something already recorded, and an explanation that already
// has a file is refused rather than having it replaced.
func (c *Client) AttachReceipt(ctx context.Context, req AttachRequest) (*Result, error) {
	if req.Explanation.IsZero() {
		return nil, errors.New("an explanation URL is required")
	}
	if err := req.Receipt.validate(); err != nil {
		return nil, err
	}

	fields := map[string]any{
		"explanation": req.Explanation.String(),
		"file_name":   req.Receipt.FileName,
		"file_bytes":  len(req.Receipt.Data),
	}

	existing, err := c.explanation(ctx, req.Explanation)
	if err != nil {
		return nil, c.record("attach", fields, nil, err)
	}
	if existing.Attachment != nil && !existing.Attachment.URL.IsZero() {
		return nil, c.record("attach", fields, nil, ErrAlreadyAttached)
	}

	id, err := req.Explanation.ID()
	if err != nil {
		return nil, c.record("attach", fields, nil, err)
	}

	updated, _, err := c.fa.BankTransactionExplanations.Update(ctx, id,
		&freeagent.BankTransactionExplanation{Attachment: req.Receipt.attachment()})
	if err != nil {
		return nil, c.record("attach", fields, nil,
			fmt.Errorf("attaching the receipt: %w", err))
	}

	res := &Result{
		Explanation:   updated.URL,
		AttachedName:  req.Receipt.FileName,
		AttachedBytes: len(req.Receipt.Data),
	}
	if res.Explanation.IsZero() {
		res.Explanation = req.Explanation
	}
	return res, c.record("attach", fields, res, nil)
}

// checkAmount is the guard. It refuses anything that would do more than fill
// part of an existing hole: a settled line, a value pointing the other way,
// or one larger than what is left.
func checkAmount(value, remaining decimal.Decimal) error {
	if remaining.IsZero() {
		return ErrAlreadyExplained
	}
	if value.IsZero() {
		return errors.New("the value must not be zero")
	}
	if value.Sign() != remaining.Sign() {
		return fmt.Errorf("%w: %s against an unexplained balance of %s",
			ErrSignMismatch, value, remaining)
	}
	if value.Abs().GreaterThan(remaining.Abs()) {
		return fmt.Errorf("%w: %s against %s remaining",
			ErrOverExplained, value, remaining)
	}
	return nil
}

func (c *Client) transaction(
	ctx context.Context, ref freeagent.ResourceURL,
) (*freeagent.BankTransaction, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	txn, _, err := c.fa.BankTransactions.GetURL(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("reading the transaction back: %w", err)
	}
	return txn, nil
}

func (c *Client) explanation(
	ctx context.Context, ref freeagent.ResourceURL,
) (*freeagent.BankTransactionExplanation, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	found, _, err := c.fa.BankTransactionExplanations.GetURL(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("reading the explanation back: %w", err)
	}
	return found, nil
}

func (r Request) auditFields() map[string]any {
	fields := map[string]any{
		"transaction": r.Transaction.String(),
		"category":    r.Category.String(),
		"gross_value": r.GrossValue.String(),
		"description": r.Description,
	}
	if r.Receipt != nil {
		fields["file_name"] = r.Receipt.FileName
		fields["file_bytes"] = len(r.Receipt.Data)
	}
	return fields
}

// record appends one line to the audit file and returns the error it was
// given, so every exit from a write path runs through here.
//
// A failure to write the audit line is returned even when the API call
// succeeded. An unrecorded write is the situation this file exists to
// prevent, and the caller needs to know the record is incomplete.
func (c *Client) record(
	op string, fields map[string]any, res *Result, cause error,
) error {
	line := map[string]any{
		"at":          time.Now().UTC().Format(time.RFC3339),
		"account":     c.account,
		"environment": c.env.Name,
		"op":          op,
	}
	for k, v := range fields {
		line[k] = v
	}
	switch {
	case cause != nil:
		line["outcome"] = "refused"
		line["error"] = cause.Error()
	case res != nil:
		line["outcome"] = "written"
		line["explanation"] = res.Explanation.String()
	}

	if err := c.append(line); err != nil {
		if cause != nil {
			return fmt.Errorf("%w (the audit line also failed: %w)", cause, err)
		}
		return fmt.Errorf("the write succeeded but its audit line failed: %w", err)
	}
	if res != nil {
		res.AuditedAt = time.Now().UTC()
	}
	return cause
}

func (c *Client) append(line map[string]any) error {
	encoded, err := json.Marshal(line)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	f, err := os.OpenFile(c.audit, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// AuditPath reports where writes are being recorded, so the server can say so
// at startup rather than leaving the operator to guess.
func (c *Client) AuditPath() string { return c.audit }
