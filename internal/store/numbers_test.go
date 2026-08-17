package store

import (
	"database/sql"
	"testing"
)

func projectFixture(t *testing.T, bodies ...string) *DB {
	t.Helper()
	db := openTemp(t)
	seedAccount(t, db)

	var records []Record
	for _, body := range bodies {
		rec, err := NewRecord("invoices", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	if _, err := db.UpsertRecords(t.Context(), 1, records); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ProjectNumbers(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	return db
}

func sumE6(t *testing.T, db *DB, field string) int64 {
	t.Helper()
	var total sql.NullInt64
	err := db.QueryRowContext(t.Context(),
		`SELECT sum(value_e6) FROM record_numbers
		 WHERE account_id = 1 AND field = ?`, field).Scan(&total)
	if err != nil {
		t.Fatal(err)
	}
	return total.Int64
}

// The whole point: summing the archive's own text through SQLite coerces it to
// REAL and reintroduces float error. Summing value_e6 is exact.
func TestProjectedSumsAreExact(t *testing.T) {
	t.Parallel()
	db := projectFixture(t,
		`{"url":"https://api.test/v2/invoices/1","total_value":"0.10"}`,
		`{"url":"https://api.test/v2/invoices/2","total_value":"0.20"}`)

	if got := sumE6(t, db, "total_value"); got != 300000 {
		t.Errorf("sum(value_e6) = %d, want 300000 (0.30 exactly)", got)
	}

	// The same sum through the raw archive is what this table exists to avoid.
	var viaText float64
	err := db.QueryRowContext(t.Context(),
		`SELECT sum(json_extract(body, '$.total_value')) FROM records
		 WHERE account_id = 1`).Scan(&viaText)
	if err != nil {
		t.Fatal(err)
	}
	if viaText == 0.3 {
		t.Skip("this SQLite sums text exactly, so the projection is not needed here")
	}
	t.Logf("summing the text gives %.20f, which is why value_e6 exists", viaText)
}

// The exact text is always kept, whatever happens to the integer.
func TestProjectionKeepsTheExactText(t *testing.T) {
	t.Parallel()
	db := projectFixture(t,
		`{"url":"https://api.test/v2/invoices/1","total_value":"1440.00"}`)

	var text string
	err := db.QueryRowContext(t.Context(),
		`SELECT text_value FROM record_numbers
		 WHERE account_id = 1 AND field = 'total_value'`).Scan(&text)
	if err != nil {
		t.Fatal(err)
	}
	// Trailing zeros and all: this is what FreeAgent sent.
	if text != "1440.00" {
		t.Errorf("text_value = %q, want the value as it arrived", text)
	}
}

// A value needing more precision than the scale gets a NULL integer, not a
// rounded one. A column that silently rounds money is worse than one that
// admits it cannot hold the value.
func TestProjectionRefusesToRound(t *testing.T) {
	t.Parallel()
	db := projectFixture(t,
		`{"url":"https://api.test/v2/invoices/1","exchange_rate":"1.1234567890"}`)

	var text string
	var scaled sql.NullInt64
	err := db.QueryRowContext(t.Context(),
		`SELECT text_value, value_e6 FROM record_numbers
		 WHERE account_id = 1 AND field = 'exchange_rate'`).Scan(&text, &scaled)
	if err != nil {
		t.Fatal(err)
	}
	if scaled.Valid {
		t.Errorf("value_e6 = %d, want NULL for a value beyond the scale", scaled.Int64)
	}
	if text != "1.1234567890" {
		t.Errorf("text_value = %q, want the full precision kept", text)
	}
}

// FreeAgent sends money as a quoted string almost everywhere and as a bare
// number in some reports. Both have to be projected.
func TestProjectionAcceptsBothNumberForms(t *testing.T) {
	t.Parallel()
	db := projectFixture(t,
		`{"url":"https://api.test/v2/invoices/1","total_value":"12.34","quantity":2.5}`)

	for field, want := range map[string]int64{
		"total_value": 12340000,
		"quantity":    2500000,
	} {
		if got := sumE6(t, db, field); got != want {
			t.Errorf("%s = %d, want %d", field, got, want)
		}
	}
}

// Identifiers that happen to parse as numbers are not quantities, and summing
// a reference is meaningless.
func TestProjectionSkipsIdentifiers(t *testing.T) {
	t.Parallel()
	db := projectFixture(t, `{"url":"https://api.test/v2/invoices/1",
		"reference":"1001","nominal_code":"750","total_value":"5.00",
		"contact":"https://api.test/v2/contacts/1","status":"Paid"}`)

	rows, err := db.QueryContext(t.Context(),
		"SELECT field FROM record_numbers WHERE account_id = 1 ORDER BY field")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var fields []string
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			t.Fatal(err)
		}
		fields = append(fields, field)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields[0] != "total_value" {
		t.Errorf("projected %v, want only total_value", fields)
	}
}

// The projection is derived, so a rebuild has to reflect the archive rather
// than accumulate what used to be in it.
func TestProjectionIsRebuiltWholesale(t *testing.T) {
	t.Parallel()
	db := projectFixture(t,
		`{"url":"https://api.test/v2/invoices/1","total_value":"10.00"}`)

	// The invoice is amended.
	rec, err := NewRecord("invoices",
		[]byte(`{"url":"https://api.test/v2/invoices/1","total_value":"20.00"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRecords(t.Context(), 1, []Record{rec}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ProjectNumbers(t.Context(), 1); err != nil {
		t.Fatal(err)
	}

	if got := sumE6(t, db, "total_value"); got != 20000000 {
		t.Errorf("sum = %d, want only the current value (20.00)", got)
	}
}

func TestProjectionStats(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	rec, err := NewRecord("invoices", []byte(
		`{"url":"https://api.test/v2/invoices/1","total_value":"1.00",
		  "rate":"0.1234567890"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRecords(t.Context(), 1, []Record{rec}); err != nil {
		t.Fatal(err)
	}

	stats, err := db.ProjectNumbers(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records != 1 || stats.Values != 2 {
		t.Errorf("stats = %+v, want one record and two values", stats)
	}
	if stats.Inexact != 1 {
		t.Errorf("Inexact = %d, want 1 for the over-precise rate", stats.Inexact)
	}
}

func TestScaledValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in    string
		want  int64
		exact bool
	}{
		{"0", 0, true},
		{"1", 1000000, true},
		{"1440.00", 1440000000, true},
		{"-90.5", -90500000, true},
		{"0.000001", 1, true},
		// Beyond the scale: kept as text, no integer.
		{"0.0000001", 0, false},
		{"1.1234567", 0, false},
		// Beyond an int64 once scaled.
		{"99999999999999999999", 0, false},
	}
	for _, tc := range tests {
		got, exact := scaledValue(tc.in)
		if exact != tc.exact {
			t.Errorf("scaledValue(%q) exact = %v, want %v", tc.in, exact, tc.exact)
			continue
		}
		if !tc.exact {
			continue
		}
		if got != tc.want {
			t.Errorf("scaledValue(%q) = %v, want %d", tc.in, got, tc.want)
		}
	}
}

// The views should read the archive without needing json_extract at the call
// site, and should degrade to NULL on a field the API stopped sending.
func TestViewsReadTheArchive(t *testing.T) {
	t.Parallel()
	db := openTemp(t)
	seedAccount(t, db)

	rec, err := NewRecord("invoices", []byte(
		`{"url":"https://api.test/v2/invoices/1","reference":"INV-1",
		  "total_value":"1440.00","status":"Paid"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertRecords(t.Context(), 1, []Record{rec}); err != nil {
		t.Fatal(err)
	}

	var reference, total, status string
	var dueOn sql.NullString
	err = db.QueryRowContext(t.Context(),
		"SELECT reference, total_value, status, due_on FROM v_invoices").
		Scan(&reference, &total, &status, &dueOn)
	if err != nil {
		t.Fatal(err)
	}
	if reference != "INV-1" || total != "1440.00" || status != "Paid" {
		t.Errorf("view returned %q %q %q", reference, total, status)
	}
	if dueOn.Valid {
		t.Errorf("due_on = %q, want NULL for a field the payload omits", dueOn.String)
	}
}
