// Package export writes archived records out as CSV or JSON.
//
// Two shapes, and the distinction matters. The faithful shape mirrors the
// payload, so it round-trips and nothing is lost. The flat shape resolves
// references to names and drops the machinery, which is what a spreadsheet
// wants and is lossy by construction. Faithful is the default: the lossy one
// has to be asked for.
package export

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/alekc/freeagent-sync/internal/store"
)

// Format is the output encoding.
type Format string

const (
	// FormatCSV is one row per record.
	FormatCSV Format = "csv"
	// FormatJSON is an array of records.
	FormatJSON Format = "json"
	// FormatJSONL is one record per line, for streaming into other tools.
	FormatJSONL Format = "jsonl"
)

// ParseFormat validates a --format value.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatCSV, FormatJSON, FormatJSONL:
		return Format(s), nil
	}
	return "", fmt.Errorf("export: unknown format %q, want csv, json or jsonl", s)
}

// Options configures an export.
type Options struct {
	// Family is the record family to write. Required.
	Family string
	// Format is the encoding.
	Format Format
	// Flat resolves references to names and flattens nested values, for a
	// spreadsheet. Lossy: it is not a substitute for the archive.
	Flat bool
	// FromDate and ToDate bound the business date. Empty means unbounded.
	FromDate string
	ToDate   string
	// IncludeDeleted writes records the far end no longer has. Off by default,
	// because the common question is "what do I have now".
	IncludeDeleted bool
}

// Result reports what was written.
type Result struct {
	Records int
	Fields  int
}

// Write exports one family.
func Write(
	ctx context.Context, db *store.DB, accountID int64, out io.Writer, opts Options,
) (Result, error) {
	var result Result
	if opts.Family == "" {
		return result, fmt.Errorf("export: a family is required")
	}
	if opts.Format == "" {
		opts.Format = FormatCSV
	}

	records, err := selectRecords(ctx, db, accountID, opts)
	if err != nil {
		return result, err
	}
	result.Records = len(records)

	var names map[string]string
	if opts.Flat {
		if names, err = referenceNames(ctx, db, accountID); err != nil {
			return result, err
		}
	}

	rows := make([]row, 0, len(records))
	for _, body := range records {
		row, err := decodeRow(body, opts, names)
		if err != nil {
			return result, err
		}
		rows = append(rows, row)
	}

	switch opts.Format {
	case FormatJSON:
		return result, writeJSON(out, rows)
	case FormatJSONL:
		return result, writeJSONL(out, rows)
	default:
		fields, err := writeCSV(out, rows)
		result.Fields = fields
		return result, err
	}
}

func selectRecords(
	ctx context.Context, db *store.DB, accountID int64, opts Options,
) ([][]byte, error) {
	if opts.IncludeDeleted {
		return db.RecordBodiesForExport(
			ctx, accountID, opts.Family, opts.FromDate, opts.ToDate)
	}
	return db.LiveRecordBodiesInWindow(
		ctx, accountID, opts.Family, opts.FromDate, opts.ToDate)
}

// cell is one exported value, kept in both forms.
//
// A CSV cell has to be text, so a nested value is rendered as the JSON it was.
// A JSON export must not do that: stringifying an array there would break the
// round trip that is the whole reason the faithful shape exists.
type cell struct {
	raw  json.RawMessage
	text string
}

// row is one exported record.
type row map[string]cell

// decodeRow turns one body into the shape being written.
//
// The faithful shape keeps every key and every value as it arrived. The flat
// shape resolves references and drops them, which is where the loss is.
func decodeRow(body []byte, opts Options, names map[string]string) (row, error) {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("export: a %s record does not decode: %w", opts.Family, err)
	}

	out := make(row, len(parsed))
	for key, raw := range parsed {
		text := scalarOrJSON(raw)

		if opts.Flat {
			if resolved, ok := resolveReference(text, names); ok {
				// The name replaces the URL under a friendlier key, because a
				// column of URLs is the least useful thing in a spreadsheet.
				encoded, err := json.Marshal(resolved)
				if err != nil {
					return nil, fmt.Errorf("export: encoding a resolved name: %w", err)
				}
				out[key+"_name"] = cell{raw: encoded, text: resolved}
				continue
			}
		}
		out[key] = cell{raw: raw, text: text}
	}
	return out, nil
}

// scalarOrJSON renders a value for a cell: strings and numbers as themselves,
// anything structured as the JSON it was, so nothing is silently dropped.
func scalarOrJSON(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" || trimmed == "null":
		return ""
	case trimmed[0] == '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return trimmed
}

// resolveReference turns a resource URL into the name of what it points at.
func resolveReference(value string, names map[string]string) (string, bool) {
	if names == nil || !strings.HasPrefix(value, "http") {
		return "", false
	}
	name, ok := names[value]
	return name, ok
}

// referenceNames maps the URLs worth resolving to human names. Contacts,
// categories, projects, users and bank accounts are the references that appear
// on the records anyone exports.
func referenceNames(
	ctx context.Context, db *store.DB, accountID int64,
) (map[string]string, error) {
	sources := map[string][]string{
		"contacts":      {"organisation_name", "first_name", "last_name"},
		"categories":    {"description", "name"},
		"projects":      {"name"},
		"users":         {"first_name", "last_name", "email"},
		"bank_accounts": {"name"},
	}

	out := map[string]string{}
	for family, fields := range sources {
		bodies, err := db.LiveRecordBodies(ctx, accountID, family)
		if err != nil {
			return nil, err
		}
		for _, body := range bodies {
			var parsed map[string]json.RawMessage
			if json.Unmarshal(body, &parsed) != nil {
				continue
			}
			url := scalarOrJSON(parsed["url"])
			if url == "" {
				continue
			}
			if name := firstNonEmpty(parsed, fields); name != "" {
				out[url] = name
			}
		}
	}
	return out, nil
}

// firstNonEmpty joins the name-ish fields a family uses, in order, so a
// contact with only a person's name still gets one.
func firstNonEmpty(parsed map[string]json.RawMessage, fields []string) string {
	var parts []string
	for _, field := range fields {
		if value := scalarOrJSON(parsed[field]); value != "" {
			parts = append(parts, value)
			// An organisation name stands alone; a person's needs both halves.
			if field == "organisation_name" || field == "description" || field == "name" {
				return value
			}
		}
	}
	return strings.Join(parts, " ")
}

// writeCSV writes the union of every row's keys as the header, so a record
// missing a field yields an empty cell rather than a shifted row.
func writeCSV(out io.Writer, rows []row) (int, error) {
	columns := unionOfKeys(rows)

	writer := csv.NewWriter(out)
	defer writer.Flush()

	if err := writer.Write(columns); err != nil {
		return 0, fmt.Errorf("export: writing the header: %w", err)
	}
	for _, row := range rows {
		record := make([]string, len(columns))
		for i, column := range columns {
			if value, ok := row[column]; ok {
				record[i] = value.text
			}
		}
		if err := writer.Write(record); err != nil {
			return 0, fmt.Errorf("export: writing a row: %w", err)
		}
	}

	writer.Flush()
	return len(columns), writer.Error()
}

// unionOfKeys collects every key any row has, so the header covers the whole
// selection rather than whatever the first record happened to carry.
func unionOfKeys(rows []row) []string {
	seen := map[string]struct{}{}
	for _, row := range rows {
		for key := range row {
			seen[key] = struct{}{}
		}
	}
	columns := slices.Sorted(maps.Keys(seen))

	// url first: it is the identity, and a spreadsheet is easier to read with
	// it in the leftmost column.
	if i := slices.Index(columns, "url"); i > 0 {
		columns = slices.Insert(slices.Delete(columns, i, i+1), 0, "url")
	}
	return columns
}

func writeJSON(out io.Writer, rows []row) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(asJSON(rows)); err != nil {
		return fmt.Errorf("export: writing JSON: %w", err)
	}
	return nil
}

func writeJSONL(out io.Writer, rows []row) error {
	encoder := json.NewEncoder(out)
	for _, r := range asJSON(rows) {
		if err := encoder.Encode(r); err != nil {
			return fmt.Errorf("export: writing JSON lines: %w", err)
		}
	}
	return nil
}

// asJSON hands the encoder the original values, so a nested object stays an
// object and the export round-trips.
func asJSON(rows []row) []map[string]json.RawMessage {
	out := make([]map[string]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		encoded := make(map[string]json.RawMessage, len(r))
		for key, value := range r {
			encoded[key] = value.raw
		}
		out = append(out, encoded)
	}
	return out
}
