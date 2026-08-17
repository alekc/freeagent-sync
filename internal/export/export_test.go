package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alekc/freeagent-sync/internal/store"
)

func newDB(t *testing.T) (*store.DB, int64) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "freeagent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	account, err := db.AddAccount(t.Context(), "test", "Test Co", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	return db, account.ID
}

func archive(t *testing.T, db *store.DB, accountID int64, family string, bodies ...string) {
	t.Helper()
	var records []store.Record
	for _, body := range bodies {
		rec, err := store.NewRecord(family, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	if _, err := db.UpsertRecords(t.Context(), accountID, records); err != nil {
		t.Fatal(err)
	}
}

func exportTo(t *testing.T, db *store.DB, accountID int64, opts Options) (string, Result) {
	t.Helper()
	var out bytes.Buffer
	result, err := Write(t.Context(), db, accountID, &out, opts)
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), result
}

func readCSV(t *testing.T, data string) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(data)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestParseFormat(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"csv", "json", "jsonl"} {
		if _, err := ParseFormat(in); err != nil {
			t.Errorf("ParseFormat(%q) returned %v", in, err)
		}
	}
	if _, err := ParseFormat("xlsx"); err == nil {
		t.Error("an unsupported format was accepted")
	}
}

// The faithful shape keeps every key. It is the default because the flat one
// is lossy, and a lossy export should have to be asked for.
func TestFaithfulExportKeepsEveryField(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/1","reference":"INV-1",
		  "total_value":"1440.00","contact":"https://api.test/v2/contacts/7"}`)

	data, result := exportTo(t, db, accountID, Options{Family: "invoices"})
	if result.Records != 1 {
		t.Fatalf("exported %d records, want 1", result.Records)
	}

	rows := readCSV(t, data)
	header := strings.Join(rows[0], ",")
	for _, want := range []string{"url", "reference", "total_value", "contact"} {
		if !strings.Contains(header, want) {
			t.Errorf("header %q is missing %q", header, want)
		}
	}
	// The reference stays a URL: nothing is resolved or dropped.
	if !strings.Contains(data, "https://api.test/v2/contacts/7") {
		t.Errorf("the contact reference was not preserved:\n%s", data)
	}
}

// url leftmost, because it is the identity and a spreadsheet is read from the
// left.
func TestFaithfulExportPutsURLFirst(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"aaa":"first alphabetically","url":"https://api.test/v2/invoices/1"}`)

	data, _ := exportTo(t, db, accountID, Options{Family: "invoices"})
	rows := readCSV(t, data)
	if rows[0][0] != "url" {
		t.Errorf("first column = %q, want url", rows[0][0])
	}
}

// Money must survive an export exactly. This is the last place it could be
// turned into a float, and a CSV of rounded amounts is worse than no CSV.
func TestExportKeepsMoneyExact(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/1","total_value":"1440.00",
		  "rate":"0.1234567890123"}`)

	data, _ := exportTo(t, db, accountID, Options{Family: "invoices"})
	for _, want := range []string{"1440.00", "0.1234567890123"} {
		if !strings.Contains(data, want) {
			t.Errorf("export lost the exact value %s:\n%s", want, data)
		}
	}
}

// A record missing a field must leave an empty cell, not shift every value in
// the row one column left.
func TestExportHeaderCoversEveryRecord(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/1","reference":"INV-1"}`,
		`{"url":"https://api.test/v2/invoices/2","status":"Paid"}`)

	data, result := exportTo(t, db, accountID, Options{Family: "invoices"})
	rows := readCSV(t, data)

	if result.Fields != len(rows[0]) {
		t.Errorf("Fields = %d but the header has %d columns", result.Fields, len(rows[0]))
	}
	for i, row := range rows {
		if len(row) != len(rows[0]) {
			t.Errorf("row %d has %d cells, want %d", i, len(row), len(rows[0]))
		}
	}

	header := strings.Join(rows[0], ",")
	if !strings.Contains(header, "reference") || !strings.Contains(header, "status") {
		t.Errorf("header %q does not cover both records", header)
	}
}

// A nested value goes in the cell as the JSON it was, so the faithful export
// really is faithful rather than quietly dropping structure.
func TestFaithfulExportKeepsNestedValues(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/1",
		  "invoice_items":[{"description":"Consultancy","price":"125.50"}]}`)

	data, _ := exportTo(t, db, accountID, Options{Family: "invoices"})
	for _, want := range []string{"Consultancy", "125.50"} {
		if !strings.Contains(data, want) {
			t.Errorf("nested value %s was dropped:\n%s", want, data)
		}
	}
}

// The flat shape is what a spreadsheet wants: names, not URLs.
func TestFlatExportResolvesReferences(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "contacts",
		`{"url":"https://api.test/v2/contacts/7","organisation_name":"Acme Ltd"}`)
	archive(t, db, accountID, "categories",
		`{"url":"https://api.test/v2/categories/285","description":"Software"}`)
	archive(t, db, accountID, "bills",
		`{"url":"https://api.test/v2/bills/1","contact":"https://api.test/v2/contacts/7",
		  "category":"https://api.test/v2/categories/285","total_value":"99.00"}`)

	data, _ := exportTo(t, db, accountID, Options{Family: "bills", Flat: true})

	for _, want := range []string{"Acme Ltd", "Software", "contact_name", "category_name"} {
		if !strings.Contains(data, want) {
			t.Errorf("flat export is missing %q:\n%s", want, data)
		}
	}
	// The URL is replaced, not kept alongside: that is the lossiness, and it
	// is the point.
	if strings.Contains(data, "https://api.test/v2/contacts/7") {
		t.Errorf("the flat export kept the URL it was supposed to resolve:\n%s", data)
	}
}

// A reference to something not archived cannot be resolved, and must stay a
// URL rather than becoming an empty cell that looks like missing data.
func TestFlatExportKeepsUnresolvableReferences(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "bills",
		`{"url":"https://api.test/v2/bills/1","contact":"https://api.test/v2/contacts/404"}`)

	data, _ := exportTo(t, db, accountID, Options{Family: "bills", Flat: true})
	if !strings.Contains(data, "contacts/404") {
		t.Errorf("an unresolvable reference was dropped:\n%s", data)
	}
}

// A person has no organisation name, so both halves are needed.
func TestFlatExportNamesAPerson(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "contacts",
		`{"url":"https://api.test/v2/contacts/9","first_name":"Ada","last_name":"Lovelace"}`)
	archive(t, db, accountID, "bills",
		`{"url":"https://api.test/v2/bills/1","contact":"https://api.test/v2/contacts/9"}`)

	data, _ := exportTo(t, db, accountID, Options{Family: "bills", Flat: true})
	if !strings.Contains(data, "Ada Lovelace") {
		t.Errorf("the person's name was not assembled:\n%s", data)
	}
}

func TestExportJSON(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/1","total_value":"10.00"}`,
		`{"url":"https://api.test/v2/invoices/2","total_value":"20.00"}`)

	data, result := exportTo(t, db, accountID,
		Options{Family: "invoices", Format: FormatJSON})
	if result.Records != 2 {
		t.Fatalf("exported %d records, want 2", result.Records)
	}

	var rows []map[string]any
	if err := json.Unmarshal([]byte(data), &rows); err != nil {
		t.Fatalf("the JSON export does not parse: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("parsed %d rows, want 2", len(rows))
	}
	if rows[0]["total_value"] != "10.00" {
		t.Errorf("total_value = %v, want the exact string 10.00", rows[0]["total_value"])
	}
}

func TestExportJSONL(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/1"}`,
		`{"url":"https://api.test/v2/invoices/2"}`)

	data, _ := exportTo(t, db, accountID, Options{Family: "invoices", Format: FormatJSONL})
	lines := strings.Split(strings.TrimSpace(data), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want one per record", len(lines))
	}
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Errorf("line %d does not parse: %v", i, err)
		}
	}
}

// The usual question is "what do I have now", so a soft-deleted record is left
// out unless asked for.
func TestExportExcludesDeletedByDefault(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/1"}`,
		`{"url":"https://api.test/v2/invoices/2"}`)

	runID, err := db.StartRun(t.Context(), accountID, store.ModeReconcile, store.RunWindow{})
	if err != nil {
		t.Fatal(err)
	}
	// Sweep with a start in the future, so both look unseen.
	if _, err := db.SoftDeleteUnseen(t.Context(), accountID, "invoices",
		time.Now().Add(time.Second), runID); err != nil {
		t.Fatal(err)
	}

	_, result := exportTo(t, db, accountID, Options{Family: "invoices"})
	if result.Records != 0 {
		t.Errorf("exported %d deleted records by default, want 0", result.Records)
	}

	_, withDeleted := exportTo(t, db, accountID,
		Options{Family: "invoices", IncludeDeleted: true})
	if withDeleted.Records != 2 {
		t.Errorf("exported %d with IncludeDeleted, want 2", withDeleted.Records)
	}
}

func TestExportWindow(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "bills",
		`{"url":"https://api.test/v2/bills/1","dated_on":"2026-01-15"}`,
		`{"url":"https://api.test/v2/bills/2","dated_on":"2026-06-15"}`)

	_, result := exportTo(t, db, accountID, Options{
		Family: "bills", FromDate: "2026-01-01", ToDate: "2026-03-31",
	})
	if result.Records != 1 {
		t.Errorf("exported %d records in the first quarter, want 1", result.Records)
	}
}

func TestExportNeedsAFamily(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)

	var out bytes.Buffer
	if _, err := Write(t.Context(), db, accountID, &out, Options{}); err == nil {
		t.Fatal("an export with no family was accepted")
	}
}

func TestExportOfNothing(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)

	data, result := exportTo(t, db, accountID, Options{Family: "invoices"})
	if result.Records != 0 {
		t.Errorf("exported %d records from an empty archive", result.Records)
	}
	// A header-only CSV is still valid; an empty file would not be.
	if strings.TrimSpace(data) != "" && len(readCSV(t, data)) > 1 {
		t.Errorf("unexpected rows:\n%s", data)
	}
}

// A JSON export must round-trip. Rendering a nested object as a string would
// make it parse but no longer be the payload, which defeats the one guarantee
// the faithful shape offers.
func TestJSONExportKeepsStructure(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "invoices",
		`{"url":"https://api.test/v2/invoices/1",
		  "invoice_items":[{"description":"Consultancy","price":"125.50"}],
		  "total_value":"125.50"}`)

	data, _ := exportTo(t, db, accountID,
		Options{Family: "invoices", Format: FormatJSON})

	var rows []map[string]any
	if err := json.Unmarshal([]byte(data), &rows); err != nil {
		t.Fatal(err)
	}

	items, ok := rows[0]["invoice_items"].([]any)
	if !ok {
		t.Fatalf("invoice_items is %T, want an array", rows[0]["invoice_items"])
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("the item is %T, want an object", items[0])
	}
	if first["price"] != "125.50" {
		t.Errorf("price = %v, want the exact string 125.50", first["price"])
	}
}

// The flat shape still has to produce valid JSON, with the resolved name as a
// string rather than a raw fragment.
func TestFlatJSONExportEncodesResolvedNames(t *testing.T) {
	t.Parallel()
	db, accountID := newDB(t)
	archive(t, db, accountID, "contacts",
		`{"url":"https://api.test/v2/contacts/7","organisation_name":"Acme, Ltd \"Trading\""}`)
	archive(t, db, accountID, "bills",
		`{"url":"https://api.test/v2/bills/1","contact":"https://api.test/v2/contacts/7"}`)

	data, _ := exportTo(t, db, accountID,
		Options{Family: "bills", Format: FormatJSON, Flat: true})

	var rows []map[string]any
	if err := json.Unmarshal([]byte(data), &rows); err != nil {
		t.Fatalf("the flat JSON export does not parse: %v\n%s", err, data)
	}
	// A name with a comma and quotes in it must survive both encodings.
	if rows[0]["contact_name"] != `Acme, Ltd "Trading"` {
		t.Errorf("contact_name = %v, want the name intact", rows[0]["contact_name"])
	}
}
