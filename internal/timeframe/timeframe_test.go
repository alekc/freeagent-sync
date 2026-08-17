package timeframe

import (
	"strings"
	"testing"
	"time"
)

// now is a fixed reference so relative offsets are assertable. Deliberately
// mid-month and mid-afternoon, so truncation and clamping bugs show up.
var now = time.Date(2026, 6, 15, 14, 30, 45, 0, time.UTC)

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want time.Time
	}{
		{"", time.Time{}},
		{"  ", time.Time{}},
		{"now", now},
		{"today", time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)},
		{"2026-03-01", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-03-01T14:00:00Z", time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)},
		{"30s", now.Add(-30 * time.Second)},
		{"30m", now.Add(-30 * time.Minute)},
		{"2h", now.Add(-2 * time.Hour)},
		{"3d", time.Date(2026, 6, 12, 14, 30, 45, 0, time.UTC)},
		{"2w", time.Date(2026, 6, 1, 14, 30, 45, 0, time.UTC)},
		{"6mo", time.Date(2025, 12, 15, 14, 30, 45, 0, time.UTC)},
		{"1y", time.Date(2025, 6, 15, 14, 30, 45, 0, time.UTC)},
		{"0d", now},
		// A leading minus is how people habitually write an offset, and it
		// means the same thing as the bare form.
		{"-2w", time.Date(2026, 6, 1, 14, 30, 45, 0, time.UTC)},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.in, now)
			if err != nil {
				t.Fatalf("Parse(%q) returned %v", tc.in, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("Parse(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// The m versus mo split is the one people get wrong, so it is asserted rather
// than left to the table above.
func TestParseMinutesAreNotMonths(t *testing.T) {
	t.Parallel()
	minute, err := Parse("1m", now)
	if err != nil {
		t.Fatal(err)
	}
	month, err := Parse("1mo", now)
	if err != nil {
		t.Fatal(err)
	}
	if !minute.Equal(now.Add(-time.Minute)) {
		t.Errorf("1m = %s, want one minute before now", minute)
	}
	if !month.Equal(time.Date(2026, 5, 15, 14, 30, 45, 0, time.UTC)) {
		t.Errorf("1mo = %s, want one month before now", month)
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in       string
		contains string
	}{
		{"2W", "lowercase"},
		{"6MO", "lowercase"},
		{"tomorrow", "cannot parse"},
		{"2026-13-01", "cannot parse"},
		{"2 weeks", "cannot parse"},
		{"w2", "cannot parse"},
		{"+2w", "cannot parse"},
		{"2f", "cannot parse"},
		{"99999999999999999999y", "out of range"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.in, now)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", tc.in)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("Parse(%q) error = %q, want it to mention %q", tc.in, err, tc.contains)
			}
		})
	}
}

// time.AddDate normalises an overflowing day, so a month before 31 March is 3
// March. Anyone bounding an accounting period reads that as a bug, so the
// parser clamps to the end of the target month instead.
func TestParseClampsMonthEnd(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		from time.Time
		expr string
		want time.Time
	}{
		{
			name: "31 March back one month clamps to 28 February",
			from: time.Date(2026, 3, 31, 9, 0, 0, 0, time.UTC),
			expr: "1mo",
			want: time.Date(2026, 2, 28, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "31 May back one month clamps to 30 April",
			from: time.Date(2026, 5, 31, 9, 0, 0, 0, time.UTC),
			expr: "1mo",
			want: time.Date(2026, 4, 30, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "29 February back one year clamps to 28 February",
			from: time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC),
			expr: "1y",
			want: time.Date(2027, 2, 28, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "31 March back one month into a leap February",
			from: time.Date(2028, 3, 31, 9, 0, 0, 0, time.UTC),
			expr: "1mo",
			want: time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC),
		},
		{
			name: "a day that exists in the target month is untouched",
			from: time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC),
			expr: "1mo",
			want: time.Date(2026, 2, 15, 9, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.expr, tc.from)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("Parse(%q) from %s = %s, want %s", tc.expr, tc.from, got, tc.want)
			}
		})
	}
}

// Day and larger units are calendar arithmetic, so the wall-clock time of day
// survives a daylight-saving boundary. An exact 72h subtraction would land an
// hour earlier and quietly shift every date-bounded query.
func TestParseCrossesDaylightSavingByWallClock(t *testing.T) {
	t.Parallel()
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// British Summer Time begins on 29 March 2026, so this window straddles it.
	start := time.Date(2026, 3, 30, 12, 0, 0, 0, london)
	got, err := Parse("3d", start)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 3, 27, 12, 0, 0, 0, london)
	if !got.Equal(want) {
		t.Fatalf("3d before %s = %s, want %s", start, got, want)
	}
	if h := got.Hour(); h != 12 {
		t.Errorf("wall-clock hour = %d, want 12", h)
	}
}

func TestParseDateTruncates(t *testing.T) {
	t.Parallel()
	got, err := ParseDate("2h", now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseDate(2h) = %s, want %s", got, want)
	}
}

func TestParseDateKeepsUnbounded(t *testing.T) {
	t.Parallel()
	got, err := ParseDate("", now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Errorf("ParseDate(\"\") = %s, want the zero time", got)
	}
}

func TestParseWindow(t *testing.T) {
	t.Parallel()
	w, err := ParseWindow("2w", "now", now)
	if err != nil {
		t.Fatal(err)
	}
	if !w.From.Equal(now.AddDate(0, 0, -14)) || !w.To.Equal(now) {
		t.Errorf("window = %s, want the last fortnight", w)
	}
}

// An inverted window would reach the API and come back empty, which reads as
// "nothing changed" rather than "you typed the bounds the wrong way round".
func TestParseWindowRejectsInverted(t *testing.T) {
	t.Parallel()
	_, err := ParseWindow("2026-06-01", "2026-01-01", now)
	if err == nil {
		t.Fatal("inverted window was accepted")
	}
	if !strings.Contains(err.Error(), "before it starts") {
		t.Errorf("error = %q, want it to explain the inversion", err)
	}
}

func TestParseWindowAllowsHalfOpen(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ from, to string }{
		{"2w", ""},
		{"", "now"},
		{"", ""},
	} {
		if _, err := ParseWindow(tc.from, tc.to, now); err != nil {
			t.Errorf("ParseWindow(%q, %q) returned %v", tc.from, tc.to, err)
		}
	}
}

func TestWindowContains(t *testing.T) {
	t.Parallel()
	w, err := ParseWindow("2026-06-01", "2026-06-30", now)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		when time.Time
		want bool
	}{
		{"inside", time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), true},
		{"on the lower bound", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), true},
		{"on the upper bound", time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), true},
		{"before", time.Date(2026, 5, 31, 23, 59, 59, 0, time.UTC), false},
		{"after", time.Date(2026, 6, 30, 0, 0, 1, 0, time.UTC), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := w.Contains(tc.when); got != tc.want {
				t.Errorf("Contains(%s) = %v, want %v", tc.when, got, tc.want)
			}
		})
	}
}

func TestWindowContainsUnbounded(t *testing.T) {
	t.Parallel()
	var w Window
	if !w.IsZero() {
		t.Fatal("zero window is not reporting itself as unbounded")
	}
	if !w.Contains(now) || !w.Contains(time.Time{}) {
		t.Error("an unbounded window should contain everything")
	}
}

func TestWindowString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		from, to string
		want     string
	}{
		{"", "", "unbounded"},
		{"2026-01-01", "", "from 2026-01-01T00:00:00Z"},
		{"", "2026-01-01", "up to 2026-01-01T00:00:00Z"},
		{"2026-01-01", "2026-02-01", "2026-01-01T00:00:00Z to 2026-02-01T00:00:00Z"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			w, err := ParseWindow(tc.from, tc.to, now)
			if err != nil {
				t.Fatal(err)
			}
			if got := w.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
