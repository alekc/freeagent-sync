// Package explain is the only code in this tool that can write to FreeAgent.
//
// It is a separate package from internal/api on purpose. That package has no
// writable constructor at all, so the read path cannot gain write capability
// by passing a different argument, and a reader of either package can tell at
// a glance which one they are looking at.
//
// The write surface is bounded twice over. This client reaches exactly two
// operations, and neither can overwrite or remove anything, so the worst case
// is an unwanted record rather than a lost one. Creating an explanation is
// refused unless a precondition read against the live API says the transaction
// still has an unexplained balance. Attaching a file needs no such guard: it
// goes to the attachments sub-resource, whose POST only ever appends.
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
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alekc/freeagent"
	"github.com/shopspring/decimal"
	"golang.org/x/oauth2"
)

// apiVersion pins X-Api-Version for this client alone, which is what makes the
// attachments sub-resource reachable: FreeAgent gates it behind 2026-09-01 and
// the SDK otherwise sends its own older default.
//
// The pin is deliberately not shared with the read client. From this version
// an explanation no longer carries a singular attachment attribute, so a guard
// reading that field would find nil every time and pass every call. Nothing
// here reads it; the attachments sub-resource is the only source consulted.
//
// From 1 December 2026 this becomes the API's default rather than an opt-in.
// That changes nothing here, because the version is stated rather than
// inherited.
const apiVersion = "2026-09-01"

// What FreeAgent will hold, documented on the attachments endpoint. The
// per-call limit is not a constraint this package can reach, since it sends
// one file at a time, but the total is worth refusing locally rather than
// discovering as a 422 after uploading several megabytes.
const (
	maxAttachments = 50
	requestTimeout = 2 * time.Minute
)

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
	// ErrAlreadyAttached means a file of that name is on the explanation
	// already. Attaching is additive, so this is not protecting a stored file
	// from being replaced; it is refusing to file the same receipt twice,
	// which is what a retried call would otherwise do.
	ErrAlreadyAttached = errors.New(
		"the explanation already carries a file of that name")
	// ErrTooManyAttachments means the explanation is at the documented limit.
	ErrTooManyAttachments = errors.New(
		"the explanation already holds the maximum number of attachments")
)

// contentTypes FreeAgent accepts for an attachment. Checked here so an
// oversized or unsupported upload fails locally instead of after transferring
// several megabytes and coming back as a 422.
// application/x-pdf is here because the bank transaction explanations page
// lists it where the bills and expenses pages say application/pdf. Both come
// from FreeAgent's own documentation for the family this package writes to,
// so both are accepted rather than guessing which page is stale.
var contentTypes = map[string]bool{
	"image/png":         true,
	"image/jpeg":        true,
	"image/gif":         true,
	"application/pdf":   true,
	"application/x-pdf": true,
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
		freeagent.WithAPIVersion(apiVersion),
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

func (r Receipt) attachment() freeagent.Attachment {
	return freeagent.Attachment{
		Data:        r.Data,
		FileName:    r.FileName,
		ContentType: r.ContentType,
		Description: r.Description,
	}
}

// attachmentList is the envelope the attachments sub-resource uses in both
// directions: a POST body carries one, and every response returns the whole
// set including anything that was already there.
type attachmentList struct {
	Attachments []freeagent.Attachment `json:"attachments"`
}

// attachmentsURL addresses the sub-resource of one explanation. Building it
// as a ResourceURL rather than a path keeps the SDK's same-host check, which
// is what stops a caller steering an upload at another server.
func attachmentsURL(ref freeagent.ResourceURL) freeagent.ResourceURL {
	return freeagent.ResourceURL(strings.TrimSuffix(ref.String(), "/") + "/attachments")
}

// attachmentsOf reads the files an explanation already carries.
//
// It asks the sub-resource rather than reading the explanation body, and that
// is load-bearing rather than stylistic: from API version 2026-09-01 the
// explanation has no singular attachment attribute, so a guard reading that
// field would find nil every time and wave through every call. A guard that
// fails open is worse than no guard, because it still reads like protection.
func (c *Client) attachmentsOf(
	ctx context.Context, ref freeagent.ResourceURL,
) ([]freeagent.Attachment, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body, _, err := c.fa.RawURL(ctx, http.MethodGet, attachmentsURL(ref), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("reading the attachments back: %w", err)
	}
	var list attachmentList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("decoding the attachments: %w", err)
	}
	return list.Attachments, nil
}

// addReceipt is the entire attachment write path.
//
// It is a POST to the sub-resource, which appends. There is no code here that
// can reach the PUT the same endpoint offers, and that is the point: PUT is
// where replacing a file's contents and deleting one both live, the latter as
// a _destroy flag rather than a DELETE verb, so it would not look destructive
// at the HTTP layer either.
func (c *Client) addReceipt(
	ctx context.Context, ref freeagent.ResourceURL, receipt Receipt,
) ([]freeagent.Attachment, error) {
	existing, err := c.attachmentsOf(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := checkRoom(existing, receipt.FileName); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	body, _, err := c.fa.RawURL(ctx, http.MethodPost, attachmentsURL(ref), nil,
		attachmentList{Attachments: []freeagent.Attachment{receipt.attachment()}})
	if err != nil {
		return nil, fmt.Errorf("attaching the receipt: %w", err)
	}

	var after attachmentList
	if err := json.Unmarshal(body, &after); err != nil {
		return nil, fmt.Errorf("decoding the attachments: %w", err)
	}
	// The response is the full set, so this confirms the file landed instead
	// of inferring it from a 2xx. Worth doing: a version mismatch would answer
	// a POST cheerfully and store nothing.
	if !hasName(after.Attachments, receipt.FileName) {
		return nil, fmt.Errorf(
			"the upload was accepted but %q is not among the %d attachments "+
				"the API returned", receipt.FileName, len(after.Attachments))
	}
	return after.Attachments, nil
}

// checkRoom refuses a duplicate and a full explanation. Neither is a
// destructive case, because POST appends; they are the two ways an append
// still produces something nobody wanted.
func checkRoom(existing []freeagent.Attachment, name string) error {
	if hasName(existing, name) {
		return fmt.Errorf("%w: %q", ErrAlreadyAttached, name)
	}
	if len(existing) >= maxAttachments {
		return fmt.Errorf("%w: %d of %d used",
			ErrTooManyAttachments, len(existing), maxAttachments)
	}
	return nil
}

func hasName(list []freeagent.Attachment, name string) bool {
	for _, a := range list {
		if strings.EqualFold(a.FileName, name) {
			return true
		}
	}
	return false
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
	// Attachments is how many files the explanation carries afterwards, read
	// from the API's own response rather than counted locally.
	Attachments int       `json:"attachments,omitempty"`
	AuditedAt   time.Time `json:"audited_at"`
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

	// The receipt is deliberately not sent inline here. From API version
	// 2026-09-01 attachments are managed only through the sub-resource, and an
	// inline attachment that the server ignores rather than rejects would
	// report a receipt this tool never filed.
	value := req.GrossValue
	created, _, err := c.fa.BankTransactionExplanations.Create(ctx,
		&freeagent.BankTransactionExplanation{
			BankTransaction: req.Transaction,
			BankAccount:     txn.BankAccount,
			DatedOn:         dated,
			GrossValue:      &value,
			Category:        req.Category,
			Description:     req.Description,
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
	if err := c.record("explain", req.auditFields(), res, nil); err != nil {
		return nil, err
	}
	if req.Receipt == nil {
		return res, nil
	}

	// Two calls, so they can part-succeed. The explanation exists either way,
	// and losing its URL to an error string would leave a caller unable to
	// finish the job, so the URL goes in the message and attach_receipt can
	// complete it.
	after, err := c.addReceipt(ctx, created.URL, *req.Receipt)
	fields := attachFields(created.URL, *req.Receipt)
	if err != nil {
		return nil, c.record("attach", fields, nil, fmt.Errorf(
			"the explanation was created at %s but the receipt was not "+
				"attached: %w; attach_receipt can add it", created.URL, err))
	}

	res.AttachedName = req.Receipt.FileName
	res.AttachedBytes = len(req.Receipt.Data)
	res.Attachments = len(after)
	return res, c.record("attach", fields, res, nil)
}

// AttachRequest describes a file to add to an existing explanation.
type AttachRequest struct {
	Explanation freeagent.ResourceURL
	Receipt     Receipt
}

// AttachReceipt adds a file to an existing explanation.
//
// An explanation holds up to fifty files and the call appends to them, so this
// no longer has to protect a stored receipt from being overwritten: nothing
// reachable from here can overwrite one. What it still refuses is filing the
// same receipt twice, and filling an explanation past the documented limit.
func (c *Client) AttachReceipt(ctx context.Context, req AttachRequest) (*Result, error) {
	if req.Explanation.IsZero() {
		return nil, errors.New("an explanation URL is required")
	}
	if err := req.Receipt.validate(); err != nil {
		return nil, err
	}

	fields := attachFields(req.Explanation, req.Receipt)
	after, err := c.addReceipt(ctx, req.Explanation, req.Receipt)
	if err != nil {
		return nil, c.record("attach", fields, nil, err)
	}

	res := &Result{
		Explanation:   req.Explanation,
		AttachedName:  req.Receipt.FileName,
		AttachedBytes: len(req.Receipt.Data),
		Attachments:   len(after),
	}
	return res, c.record("attach", fields, res, nil)
}

func attachFields(ref freeagent.ResourceURL, receipt Receipt) map[string]any {
	return map[string]any{
		"explanation": ref.String(),
		"file_name":   receipt.FileName,
		"file_bytes":  len(receipt.Data),
	}
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
