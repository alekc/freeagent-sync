package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/alekc/freeagent"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shopspring/decimal"

	"github.com/alekc/freeagent-sync/internal/explain"
	"github.com/alekc/freeagent-sync/internal/timeframe"
)

// registerWriteTools adds the two tools that can change the ledger. They are
// registered only when -allow-writes was given, so a server without the flag
// does not advertise them at all.
//
// Both are annotated as not read-only and not destructive, which is the
// accurate pair: they create records, and neither can overwrite or remove
// anything. Explaining is refused unless the transaction still has an
// unexplained balance, and attaching appends to a sub-resource that has no
// replace or delete within reach of this code.
func registerWriteTools(s *mcp.Server, w *explain.Client) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "explain_bank_transaction",
		Description: "Explain a bank transaction that is still unexplained, " +
			"optionally attaching a receipt in the same call. The transaction " +
			"is re-read before writing and the call is refused if it has since " +
			"been explained, if the value runs the other way, or if it exceeds " +
			"what is left unexplained. An already explained transaction is " +
			"never modified. Every attempt is recorded to a local audit file.",
		OutputSchema: writeOutputSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: new(false),
			IdempotentHint:  false,
		},
	}, explainTransaction(w))

	mcp.AddTool(s, &mcp.Tool{
		Name: "attach_receipt",
		Description: "Attach a file to an existing bank transaction explanation. " +
			"An explanation holds up to 50 files and this adds to them, so a " +
			"receipt somebody already filed is never replaced or removed. A " +
			"file whose name is already on the explanation is refused, so " +
			"repeating the call does not file the same receipt twice.",
		OutputSchema: writeOutputSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: new(false),
			IdempotentHint:  false,
		},
	}, attachReceipt(w))
}

// receiptInput carries a file inline, which is how FreeAgent takes an
// attachment: on the parent record, base64 encoded, not as a separate upload.
type receiptInput struct {
	FileName    string `json:"file_name" jsonschema:"the file name to store, for example receipt-2026-04-01.pdf"`
	ContentType string `json:"content_type" jsonschema:"image/png, image/jpeg, image/gif or application/pdf; FreeAgent accepts nothing else, and rejects it before the upload is read"`
	DataBase64  string `json:"data_base64" jsonschema:"the file content, base64 encoded, at most 5 MB decoded"`
	Description string `json:"description,omitempty" jsonschema:"an optional note stored with the file"`
}

func (in *receiptInput) decode() (*explain.Receipt, error) {
	if in == nil {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(in.DataBase64)
	if err != nil {
		return nil, fmt.Errorf("data_base64 is not valid base64: %w", err)
	}
	return &explain.Receipt{
		FileName:    in.FileName,
		ContentType: in.ContentType,
		Description: in.Description,
		Data:        data,
	}, nil
}

type explainInput struct {
	Transaction string `json:"transaction" jsonschema:"the bank transaction URL, taken from list_bank_transactions"`
	Category    string `json:"category" jsonschema:"the accounting category URL, taken from list_categories; this is the field no guard can check, so choose it deliberately"`
	GrossValue  string `json:"gross_value" jsonschema:"the amount to explain, as a decimal string to keep its precision, negative for money out; it must run the same way as the transaction's unexplained amount and must not exceed it"`

	DatedOn     string `json:"dated_on,omitempty" jsonschema:"defaults to the transaction's own date"`
	Description string `json:"description,omitempty" jsonschema:"a note stored on the explanation"`

	Receipt *receiptInput `json:"receipt,omitempty" jsonschema:"an optional receipt to attach in the same call"`
}

type attachInput struct {
	Explanation string       `json:"explanation" jsonschema:"the bank transaction explanation URL to attach to"`
	Receipt     receiptInput `json:"receipt" jsonschema:"the file to attach"`
}

// writeOutput mirrors explain.Result, including the balance the guard read,
// so an answer shows its working rather than only asserting success.
type writeOutput struct {
	Explanation       string `json:"explanation"`
	Transaction       string `json:"transaction,omitempty"`
	GrossValue        string `json:"gross_value,omitempty"`
	UnexplainedBefore string `json:"unexplained_before,omitempty"`
	UnexplainedAfter  string `json:"unexplained_after,omitempty"`
	AttachedFile      string `json:"attached_file,omitempty"`
	AttachedBytes     int    `json:"attached_bytes,omitempty"`
	Attachments       int    `json:"attachments,omitempty"`
	Audited           string `json:"audited_at,omitempty"`
}

var writeOutputSchema = &jsonschema.Schema{
	Type: "object",
	Properties: map[string]*jsonschema.Schema{
		"explanation": {Type: "string", Description: "URL of the explanation written"},
		"transaction": {Type: "string", Description: "the transaction it explains"},
		"gross_value": {Type: "string", Description: "the amount explained, as a string"},
		"unexplained_before": {Type: "string", Description: "the transaction's " +
			"unexplained balance as the guard read it, immediately before writing"},
		"unexplained_after": {Type: "string", Description: "what remains " +
			"unexplained afterwards; not zero when only part was explained"},
		"attached_file":  {Type: "string", Description: "the file name attached, if any"},
		"attached_bytes": {Type: "integer", Description: "its decoded size"},
		"attachments": {Type: "integer", Description: "how many files the " +
			"explanation carries afterwards, as the API reported them"},
		"audited_at": {Type: "string", Description: "when the audit line was written"},
	},
}

func explainTransaction(w *explain.Client) mcp.ToolHandlerFor[explainInput, writeOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in explainInput,
	) (*mcp.CallToolResult, writeOutput, error) {
		value, err := decimal.NewFromString(in.GrossValue)
		if err != nil {
			return nil, writeOutput{}, fmt.Errorf(
				"gross_value %q is not a decimal: %w", in.GrossValue, err)
		}

		var dated freeagent.Date
		if in.DatedOn != "" {
			parsed, err := timeframe.ParseDate(in.DatedOn, time.Now())
			if err != nil {
				return nil, writeOutput{}, fmt.Errorf("dated_on: %w", err)
			}
			dated = freeagent.Date{Time: parsed}
		}

		receipt, err := in.Receipt.decode()
		if err != nil {
			return nil, writeOutput{}, err
		}

		res, err := w.Explain(ctx, explain.Request{
			Transaction: freeagent.ResourceURL(in.Transaction),
			Category:    freeagent.ResourceURL(in.Category),
			GrossValue:  value,
			DatedOn:     dated,
			Description: in.Description,
			Receipt:     receipt,
		})
		if err != nil {
			return nil, writeOutput{}, err
		}
		return nil, asWriteOutput(res), nil
	}
}

func attachReceipt(w *explain.Client) mcp.ToolHandlerFor[attachInput, writeOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in attachInput,
	) (*mcp.CallToolResult, writeOutput, error) {
		receipt, err := in.Receipt.decode()
		if err != nil {
			return nil, writeOutput{}, err
		}
		res, err := w.AttachReceipt(ctx, explain.AttachRequest{
			Explanation: freeagent.ResourceURL(in.Explanation),
			Receipt:     *receipt,
		})
		if err != nil {
			return nil, writeOutput{}, err
		}
		return nil, asWriteOutput(res), nil
	}
}

func asWriteOutput(res *explain.Result) writeOutput {
	out := writeOutput{
		Explanation:       res.Explanation.String(),
		Transaction:       res.Transaction.String(),
		GrossValue:        res.GrossValue,
		UnexplainedBefore: res.UnexplainedWas,
		UnexplainedAfter:  res.RemainingAfter,
		AttachedFile:      res.AttachedName,
		AttachedBytes:     res.AttachedBytes,
		Attachments:       res.Attachments,
	}
	if !res.AuditedAt.IsZero() {
		out.Audited = res.AuditedAt.Format(time.RFC3339)
	}
	return out
}
