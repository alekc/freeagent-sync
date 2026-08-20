package main

import (
	"encoding/csv"
	"slices"
	"strings"
	"testing"
)

// A value holding the separator, a double quote or a newline has to leave as
// one field. Joining on a bare separator shifted every later column, and did
// it silently because the row still looked well formed (issue #3).
func TestSQLQuotesDelimitedOutput(t *testing.T) {
	for _, c := range []struct {
		name   string
		format string
		query  string
		want   string
	}{
		{
			name:   "comma in a csv value",
			format: "csv",
			query:  `SELECT 'a,b' AS one, 'c' AS two`,
			want:   "one,two\n\"a,b\",c\n",
		},
		{
			name:   "double quote in a csv value",
			format: "csv",
			query:  `SELECT 'say "hi"' AS one, 'c' AS two`,
			want:   "one,two\n\"say \"\"hi\"\"\",c\n",
		},
		{
			name:   "newline in a csv value",
			format: "csv",
			query:  `SELECT 'a' || char(10) || 'b' AS one, 'c' AS two`,
			want:   "one,two\n\"a\nb\",c\n",
		},
		{
			name:   "tab in a tsv value",
			format: "tsv",
			query:  `SELECT 'a' || char(9) || 'b' AS one, 'c' AS two`,
			want:   "one\ttwo\n\"a\tb\"\tc\n",
		},
		{
			// An unaliased json_extract names a column containing both a
			// comma and quotes, which is the shape README shows.
			name:   "comma and quotes in a column name",
			format: "csv",
			query:  `SELECT json_extract('{"a":1,"b":2}', '$.a'), 7 AS other`,
			want:   "\"json_extract('{\"\"a\"\":1,\"\"b\"\":2}', '$.a')\",other\n1,7\n",
		},
		{
			name:   "comma in an aliased column name",
			format: "csv",
			query:  `SELECT 1 AS "a,b", 2 AS c`,
			want:   "\"a,b\",c\n1,2\n",
		},
		{
			name:   "every row is quoted, not only the first",
			format: "csv",
			query:  `SELECT 'a,b' AS one UNION ALL SELECT 'c,d' ORDER BY 1`,
			want:   "one\n\"a,b\"\n\"c,d\"\n",
		},
		{
			// Leading and trailing spaces quote too, which widens the change
			// for tsv beyond values holding a tab.
			name:   "leading space in a tsv value",
			format: "tsv",
			query:  `SELECT ' a' AS one, 'b' AS two`,
			want:   "one\ttwo\n\" a\"\tb\n",
		},
		{
			name:   "csv leaves an ordinary value alone",
			format: "csv",
			query:  `SELECT 'a' AS one, 'b' AS two`,
			want:   "one,two\na,b\n",
		},
		{
			name:   "tsv leaves an ordinary value alone",
			format: "tsv",
			query:  `SELECT 'a' AS one, 'b' AS two`,
			want:   "one\ttwo\na\tb\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			if got := h.mustRun("sql", c.query, "-format", c.format); got != c.want {
				t.Errorf("output = %q, want %q", got, c.want)
			}
		})
	}
}

// The property that actually broke: a conforming reader has to recover every
// value as the field it started as. Field counts are checked too, since a
// short row is how the corruption showed up on real data.
func TestSQLDelimitedOutputRoundTrips(t *testing.T) {
	const query = `SELECT 'a,b' AS c1, 'say "hi"' AS c2, ` +
		`'a' || char(10) || 'b' AS c3, 'a' || char(9) || 'b' AS c4`
	want := []string{"a,b", `say "hi"`, "a\nb", "a\tb"}

	for _, c := range []struct {
		format string
		comma  rune
	}{
		{format: "csv", comma: ','},
		{format: "tsv", comma: '\t'},
	} {
		t.Run(c.format, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun("init")
			out := h.mustRun("sql", query, "-format", c.format)

			reader := csv.NewReader(strings.NewReader(out))
			reader.Comma = c.comma
			records, err := reader.ReadAll()
			if err != nil {
				t.Fatalf("reading back %q: %v", out, err)
			}
			if len(records) != 2 {
				t.Fatalf("got %d records from %q, want a header and one row",
					len(records), out)
			}
			if !slices.Equal(records[1], want) {
				t.Errorf("round trip = %q, want %q", records[1], want)
			}
		})
	}
}

// The table renderer draws cell borders, so a comma there was never
// ambiguous and must not acquire quotes.
func TestSQLTableFormatDoesNotQuote(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")
	out := h.mustRun("sql", `SELECT 'a,b' AS one`, "-format", "table")

	if !strings.Contains(out, "a,b") {
		t.Errorf("table output = %q, want the raw value in it", out)
	}
	if strings.Contains(out, `"a,b"`) {
		t.Errorf("table output = %q, want no quoting", out)
	}
}

// A query that fails partway still has to leave whole records on stdout.
// Buffering made the first cut of this fix end output mid-field, which parses
// and so reads as a real value: the very fault issue #3 is about.
func TestSQLFlushesWholeRecordsWhenAQueryFailsPartway(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	// abs() of the most negative integer overflows, and only on the last row,
	// so the failure lands after enough output to fill the write buffer.
	const query = `WITH RECURSIVE t(n) AS (` +
		`SELECT 1 UNION ALL SELECT n+1 FROM t WHERE n < 600) ` +
		`SELECT n AS num, ` +
		`CASE WHEN n = 600 THEN abs(-9223372036854775808) ELSE n * 1000 END AS v ` +
		`FROM t`

	code, stdout, stderr := h.run("sql", query, "-format", "csv", "-limit", "0")
	if code == exitOK {
		t.Fatalf("exited %d, want a failure; stderr: %s", code, stderr)
	}

	records, err := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if err != nil {
		t.Fatalf("stdout does not parse, so a record was cut short: %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("got %d records, want the header and the rows before the error",
			len(records))
	}
	// A truncated tail keeps the right field count, so the count alone proves
	// nothing: v is always num followed by three zeroes, so check that.
	last := records[len(records)-1]
	if last[1] != last[0]+"000" {
		t.Errorf("last record = %q, want v to be num followed by 000", last)
	}
}

// The flush has to stay above the truncation notice, and the notice has to
// stay on stderr so it cannot land among the rows.
func TestSQLLimitKeepsQuotingAndStreamOrder(t *testing.T) {
	h := newHarness(t)
	h.mustRun("init")

	code, stdout, stderr := h.run("sql",
		`SELECT 'a,b' AS one UNION ALL SELECT 'c,d' UNION ALL SELECT 'e,f' ORDER BY 1`,
		"-format", "csv", "-limit", "2")
	if code != exitOK {
		t.Fatalf("exited %d; stderr: %s", code, stderr)
	}
	if want := "one\n\"a,b\"\n\"c,d\"\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "stopped at the --limit of 2 rows") {
		t.Errorf("stderr = %q, want the truncation notice", stderr)
	}
}
