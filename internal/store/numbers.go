package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// NumberScale is how many decimal places value_e6 preserves. Six covers every
// amount and rate FreeAgent sends, with room for exchange rates, while leaving
// an int64 able to hold nine trillion currency units.
const NumberScale = 6

// scaleFactor is 10^NumberScale.
var scaleFactor = decimal.New(1, NumberScale)

// notNumeric are fields whose values sometimes parse as a decimal but are
// identifiers, not quantities. Summing a reference is meaningless, and leaving
// them in makes the table harder to read for no gain.
var notNumeric = map[string]bool{
	"reference":                     true,
	"transaction_id":                true,
	"nominal_code":                  true,
	"display_nominal_code":          true,
	"sales_tax_registration_number": true,
	"company_registration_number":   true,
	"account_number":                true,
	"sort_code":                     true,
	"period":                        true,
	"year":                          true,
	"box_number":                    true,
}

// NumberStats reports what a projection pass produced.
type NumberStats struct {
	Records int
	Values  int
	// Inexact counts values whose exact integer would not fit at NumberScale.
	// Their text is still stored; only the integer is NULL.
	Inexact int
}

// ProjectNumbers rebuilds the exact numeric projection.
//
// It exists because json_extract returns an exact decimal as TEXT and SQLite
// coerces TEXT to REAL to aggregate it, so summing amounts straight out of the
// archive reintroduces exactly the float error the rest of this tool refuses.
// Summing value_e6 instead is exact.
//
// Rebuilt wholesale rather than patched: it is derived, and a full rebuild is
// the only way to guarantee it matches the archive after a record changed
// shape or was deleted.
func (d *DB) ProjectNumbers(ctx context.Context, accountID int64) (NumberStats, error) {
	var stats NumberStats

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("store: starting the numeric projection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM record_numbers WHERE account_id = ?", accountID); err != nil {
		return stats, fmt.Errorf("store: clearing the numeric projection: %w", err)
	}

	insert, err := tx.PrepareContext(ctx,
		`INSERT INTO record_numbers
		 (account_id, url, family, field, text_value, value_e6)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return stats, fmt.Errorf("store: preparing the numeric projection: %w", err)
	}
	defer func() { _ = insert.Close() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT url, family, body FROM records WHERE account_id = ? ORDER BY url`,
		accountID)
	if err != nil {
		return stats, fmt.Errorf("store: reading records to project: %w", err)
	}

	err = func() error {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var url, family, body string
			if err := rows.Scan(&url, &family, &body); err != nil {
				return fmt.Errorf("store: reading a record to project: %w", err)
			}
			stats.Records++

			for _, value := range numericFields([]byte(body)) {
				scaled, exact := scaledValue(value.text)
				if !exact {
					stats.Inexact++
				}
				if _, err := insert.ExecContext(ctx, accountID, url, family,
					value.field, value.text, scaled); err != nil {
					return fmt.Errorf("store: projecting %s.%s: %w", url, value.field, err)
				}
				stats.Values++
			}
		}
		return rows.Err()
	}()
	if err != nil {
		return stats, err
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("store: committing the numeric projection: %w", err)
	}
	return stats, nil
}

// numericValue is one projected field.
type numericValue struct {
	field string
	text  string
}

// numericFields finds the top-level fields that hold a number.
//
// Only the top level: a nested object is a different record's worth of data,
// and flattening it here would invent field names that do not exist in the
// payload. Nested amounts are reachable through json_extract when wanted.
func numericFields(body []byte) []numericValue {
	var parsed map[string]json.RawMessage
	if json.Unmarshal(body, &parsed) != nil {
		return nil
	}

	var out []numericValue
	for field, raw := range parsed {
		if notNumeric[field] || strings.HasSuffix(field, "_url") || field == "url" {
			continue
		}
		text, ok := numericText(raw)
		if !ok {
			continue
		}
		out = append(out, numericValue{field: field, text: text})
	}
	return out
}

// numericText accepts a bare number or a quoted one, which is how FreeAgent
// sends money: a string almost everywhere, a bare number in some reports.
func numericText(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}

	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		trimmed = strings.TrimSpace(s)
	}
	if trimmed == "" {
		return "", false
	}
	if _, err := decimal.NewFromString(trimmed); err != nil {
		return "", false
	}
	return trimmed, true
}

// scaledValue converts an exact decimal to an integer at NumberScale,
// reporting whether it fits. A value that does not is stored as NULL rather
// than rounded, because a column that silently rounds money is worse than one
// that admits it cannot represent the value.
func scaledValue(text string) (any, bool) {
	value, err := decimal.NewFromString(text)
	if err != nil {
		return nil, false
	}
	if value.Exponent() < -NumberScale {
		return nil, false
	}

	scaled := value.Mul(scaleFactor)
	if !scaled.IsInteger() {
		return nil, false
	}
	if !scaled.BigInt().IsInt64() {
		return nil, false
	}
	return scaled.IntPart(), true
}
