// Package timeframe parses the bound expressions accepted by every time flag:
// an absolute date, an RFC 3339 instant, or a relative offset such as 2w.
package timeframe

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Syntax accepted by every bound flag. Kept in one place because the same
// grammar serves both the business-date and the record-change windows.
const Syntax = `2026-03-01, an RFC 3339 instant, "now", "today", ` +
	`or a relative offset (30m, 2h, 3d, 2w, 6mo, 1y)`

// relative matches an offset such as 2w or -6mo. The unit alternation lists
// mo before m so the two-letter unit is never shadowed by the one-letter one.
var relative = regexp.MustCompile(`^-?(\d+)(mo|s|m|h|d|w|y)$`)

// ErrUnbounded reports a bound expression that resolved to no limit. Callers
// treat a zero time as unbounded, so this is only for explicit checks.
var ErrUnbounded = errors.New("timeframe: unbounded")

// Parse resolves one bound expression to an instant, relative to now, whose
// location is used for date-only forms. An empty string is unbounded and
// yields the zero time.
func Parse(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "":
		return time.Time{}, nil
	case "now":
		return now, nil
	case "today":
		return midnight(now), nil
	}

	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, now.Location()); err == nil {
		return t, nil
	}
	if m := relative.FindStringSubmatch(s); m != nil {
		return applyOffset(now, m[1], m[2])
	}

	// An uppercase unit is the most likely near miss, and the m versus mo
	// split is the one people get wrong. Say both rather than "invalid".
	if relative.MatchString(strings.ToLower(s)) {
		return time.Time{}, fmt.Errorf(
			"timeframe: %q must be lowercase; m is minutes and mo is months", s)
	}
	return time.Time{}, fmt.Errorf("timeframe: cannot parse %q, expected %s", s, Syntax)
}

// ParseDate resolves a bound expression and truncates it to midnight in now's
// location, for the flags that address business dates rather than instants.
func ParseDate(s string, now time.Time) (time.Time, error) {
	t, err := Parse(s, now)
	if err != nil || t.IsZero() {
		return t, err
	}
	return midnight(t.In(now.Location())), nil
}

// applyOffset subtracts a relative offset from now. Sub-day units are exact
// durations; day and larger are calendar arithmetic, so a wall-clock time of
// day survives a daylight-saving boundary.
func applyOffset(now time.Time, digits, unit string) (time.Time, error) {
	n, err := strconv.Atoi(digits)
	if err != nil {
		return time.Time{}, fmt.Errorf("timeframe: offset %q is out of range", digits+unit)
	}
	switch unit {
	case "s":
		return now.Add(-time.Duration(n) * time.Second), nil
	case "m":
		return now.Add(-time.Duration(n) * time.Minute), nil
	case "h":
		return now.Add(-time.Duration(n) * time.Hour), nil
	case "d":
		return now.AddDate(0, 0, -n), nil
	case "w":
		return now.AddDate(0, 0, -7*n), nil
	case "mo":
		return addMonths(now, -n), nil
	case "y":
		return addMonths(now, -12*n), nil
	}
	return time.Time{}, fmt.Errorf("timeframe: unknown unit %q", unit)
}

// addMonths shifts by whole months, clamping to the last day of the target
// month. time.AddDate normalises instead, so a month before 31 March is 3
// March, which reads as a bug to anyone bounding an accounting period.
func addMonths(t time.Time, months int) time.Time {
	year, month, day := t.Date()
	hour, minute, sec := t.Clock()

	target := time.Date(year, month, 1, hour, minute, sec, t.Nanosecond(), t.Location()).
		AddDate(0, months, 0)
	if last := daysIn(target.Year(), target.Month()); day > last {
		day = last
	}
	return time.Date(target.Year(), target.Month(), day,
		hour, minute, sec, t.Nanosecond(), t.Location())
}

// daysIn returns the length of a month, found by stepping back a day from the
// first of the next one.
func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func midnight(t time.Time) time.Time {
	year, month, day := t.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, t.Location())
}

// Window is a resolved pair of bounds. A zero bound is unbounded on that side.
type Window struct {
	From time.Time
	To   time.Time
}

// IsZero reports a window with no bound on either side.
func (w Window) IsZero() bool { return w.From.IsZero() && w.To.IsZero() }

// String renders the window for logs and run records.
func (w Window) String() string {
	switch {
	case w.IsZero():
		return "unbounded"
	case w.From.IsZero():
		return "up to " + w.To.Format(time.RFC3339)
	case w.To.IsZero():
		return "from " + w.From.Format(time.RFC3339)
	}
	return w.From.Format(time.RFC3339) + " to " + w.To.Format(time.RFC3339)
}

// ParseWindow resolves a pair of bound expressions and rejects an inverted
// window locally, rather than letting it reach the API as an empty result the
// caller would read as "nothing changed".
func ParseWindow(from, to string, now time.Time) (Window, error) {
	return parseWindow(from, to, now, Parse)
}

// ParseDateWindow is ParseWindow for the flags addressing business dates.
func ParseDateWindow(from, to string, now time.Time) (Window, error) {
	return parseWindow(from, to, now, ParseDate)
}

func parseWindow(from, to string, now time.Time,
	parse func(string, time.Time) (time.Time, error),
) (Window, error) {
	lower, err := parse(from, now)
	if err != nil {
		return Window{}, err
	}
	upper, err := parse(to, now)
	if err != nil {
		return Window{}, err
	}
	if !lower.IsZero() && !upper.IsZero() && upper.Before(lower) {
		return Window{}, fmt.Errorf("timeframe: window ends %s before it starts %s",
			upper.Format(time.RFC3339), lower.Format(time.RFC3339))
	}
	return Window{From: lower, To: upper}, nil
}

// Contains reports whether t falls inside the window. Both bounds are
// inclusive, matching the API's own updated_since and from_date filters.
func (w Window) Contains(t time.Time) bool {
	if !w.From.IsZero() && t.Before(w.From) {
		return false
	}
	if !w.To.IsZero() && t.After(w.To) {
		return false
	}
	return true
}
