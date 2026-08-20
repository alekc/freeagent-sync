package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
)

// writeVerbs are refused before the query reaches SQLite. The archive is the
// only copy of records the far end may have deleted, so an ad-hoc query is not
// a place to discover a typo in a DELETE.
var writeVerbs = []string{
	"insert", "update", "delete", "drop", "alter", "create", "replace",
	"truncate", "attach", "detach", "vacuum", "reindex", "pragma", "begin",
	"commit", "rollback",
}

// cmdSQL runs one read-only query. The archive stores bodies as JSON, so
// SQLite's json_extract reaches any field, modelled or not:
//
//	select json_extract(body, '$.reference'), json_extract(body, '$.total_value')
//	from records where family = 'bills' and deleted_at is null
func cmdSQL(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("sql", e)
	e.g.register(fs)
	limit := fs.Int("limit", 200, "maximum rows to print (0: no limit)")
	format := fs.String("format", "table",
		"output format: table, tsv or csv (csv and tsv are RFC 4180 quoted)")
	positional, err := e.parse(fs, args)
	if err != nil {
		return e.fail(err)
	}
	if len(positional) != 1 {
		fprintln(e.err, `Usage: fasync sql "select ... from records"`)
		return exitConfig
	}

	query := positional[0]
	if err := refuseWrites(query); err != nil {
		return e.fail(err)
	}
	if *format != "table" && *format != "tsv" && *format != "csv" {
		return e.fail(fmt.Errorf("unknown format %q, want table, tsv or csv", *format))
	}

	_, db, err := e.openArchive(ctx)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = db.Close() }()

	// Running the caller's own SQL against their own archive is the entire
	// purpose of this command, and refuseWrites above is what keeps a mistake
	// from being destructive.
	//nolint:gosec // G701: the query is the command's input by design
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return e.fail(err)
	}
	defer func() { _ = rows.Close() }()

	return e.printRows(rows, *format, *limit)
}

// refuseWrites is a guard, not a parser. It cannot be defeated by a determined
// caller, and does not need to be: the point is that an ordinary mistake
// cannot damage an archive whose whole value is being the only remaining copy.
func refuseWrites(query string) error {
	for _, statement := range strings.Split(query, ";") {
		trimmed := strings.TrimSpace(strings.ToLower(statement))
		if trimmed == "" {
			continue
		}
		verb, _, _ := strings.Cut(trimmed, " ")
		for _, banned := range writeVerbs {
			if verb == banned {
				return fmt.Errorf(
					"%s is not allowed here; sql is read-only", strings.ToUpper(verb))
			}
		}
	}
	return nil
}

func (e *env) printRows(rows *sql.Rows, format string, limit int) int {
	columns, err := rows.Columns()
	if err != nil {
		return e.fail(err)
	}

	var t table.Writer
	var delimited *csv.Writer
	if format == "table" {
		t = newTable(e)
		t.AppendHeader(toRow(columns))
	} else {
		delimited, err = newDelimitedWriter(e.out, format)
		if err != nil {
			return e.fail(err)
		}
		if err := delimited.Write(columns); err != nil {
			return e.fail(fmt.Errorf("writing the header: %w", err))
		}
	}

	// csv.Writer buffers, so an error partway through has to flush what it
	// already took or stdout ends mid-field: whole records only (issue #3).
	failFlushed := func(err error) int {
		if delimited != nil {
			delimited.Flush()
		}
		return e.fail(err)
	}

	var printed int
	for rows.Next() {
		if limit > 0 && printed >= limit {
			break
		}
		values, err := scanRow(rows, len(columns))
		if err != nil {
			return failFlushed(err)
		}
		if t != nil {
			t.AppendRow(toRow(values))
		} else if err := delimited.Write(values); err != nil {
			return failFlushed(fmt.Errorf("writing a row: %w", err))
		}
		printed++
	}
	if err := rows.Err(); err != nil {
		return failFlushed(err)
	}

	if t != nil {
		t.Render()
	} else {
		delimited.Flush()
		if err := delimited.Error(); err != nil {
			return e.fail(fmt.Errorf("writing the output: %w", err))
		}
	}
	if limit > 0 && printed == limit {
		// Silence here would read as "that is all there is", which is the one
		// thing a truncated result must not imply.
		fprintf(e.err, "\nstopped at the --limit of %d rows\n", limit)
	}
	return exitOK
}

// scanRow reads a row as strings, which is what printing needs and what keeps
// exact decimals exact: routing money through float64 to display it would
// undo the care taken to store it.
func scanRow(rows *sql.Rows, columns int) ([]string, error) {
	raw := make([]any, columns)
	pointers := make([]any, columns)
	for i := range raw {
		pointers[i] = &raw[i]
	}
	if err := rows.Scan(pointers...); err != nil {
		return nil, fmt.Errorf("fasync: reading a row: %w", err)
	}

	out := make([]string, columns)
	for i, value := range raw {
		switch typed := value.(type) {
		case nil:
			out[i] = ""
		case []byte:
			out[i] = string(typed)
		case string:
			out[i] = typed
		default:
			out[i] = fmt.Sprint(typed)
		}
	}
	return out, nil
}

func toRow(values []string) table.Row {
	row := make(table.Row, len(values))
	for i, v := range values {
		row[i] = v
	}
	return row
}

// newDelimitedWriter quotes any value holding the separator, a double quote or
// a newline. Joining on a bare separator instead shifted every later column,
// and stayed quiet about it (issue #3).
func newDelimitedWriter(out io.Writer, format string) (*csv.Writer, error) {
	w := csv.NewWriter(out)
	switch format {
	case "csv":
		w.Comma = ','
	case "tsv":
		w.Comma = '\t'
	default:
		// cmdSQL validates the flag, so reaching here means a new format was
		// added without wiring a separator. Guessing one would emit CSV.
		return nil, fmt.Errorf("no separator for format %q", format)
	}
	return w, nil
}
