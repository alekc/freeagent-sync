package ui

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// capture records what a reporter logged, so assertions read the events
// rather than a formatted string.
type capture struct {
	mu     sync.Mutex
	events []event
}

type event struct {
	level string
	msg   string
	args  []any
}

func (c *capture) add(level, msg string, args []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event{level: level, msg: msg, args: args})
}

func (c *capture) Info(msg string, args ...any)  { c.add("info", msg, args) }
func (c *capture) Warn(msg string, args ...any)  { c.add("warn", msg, args) }
func (c *capture) Error(msg string, args ...any) { c.add("error", msg, args) }

func (c *capture) find(level, msg string) (event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.level == level && e.msg == msg {
			return e, true
		}
	}
	return event{}, false
}

func argValue(e event, key string) (any, bool) {
	for i := 0; i+1 < len(e.args); i += 2 {
		if k, ok := e.args[i].(string); ok && k == key {
			return e.args[i+1], true
		}
	}
	return nil, false
}

// syncBuffer is safe for the render goroutine to write to while a test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestParseMode(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"auto", "always", "never"} {
		got, err := ParseMode(in)
		if err != nil {
			t.Errorf("ParseMode(%q) returned %v", in, err)
		}
		if string(got) != in {
			t.Errorf("ParseMode(%q) = %q", in, got)
		}
	}
	_, err := ParseMode("yes")
	if err == nil {
		t.Fatal("ParseMode(\"yes\") succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "auto, always or never") {
		t.Errorf("error = %q, want it to list the alternatives", err)
	}
}

// A buffer is not a terminal, so auto must fall back to log lines. This is
// the path every cron run takes.
func TestAutoFallsBackToLogsOffTerminal(t *testing.T) {
	t.Parallel()
	r, err := New(ModeAuto, &syncBuffer{}, &capture{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, ok := r.(*logReporter); !ok {
		t.Errorf("auto picked %T off a terminal, want the log reporter", r)
	}
}

func TestModeNeverAlwaysLogs(t *testing.T) {
	t.Parallel()
	r, err := New(ModeNever, os.Stdout, &capture{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if _, ok := r.(*logReporter); !ok {
		t.Errorf("never picked %T, want the log reporter", r)
	}
}

func TestModeAlwaysDrawsBars(t *testing.T) {
	t.Parallel()
	out := &syncBuffer{}
	r, err := New(ModeAlways, out, &capture{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(*barReporter); !ok {
		t.Fatalf("always picked %T, want the bar reporter", r)
	}

	tr := r.Track("bills", 10, UnitsCount)
	tr.Add(10)
	tr.Done()
	r.Close()

	if out.String() == "" {
		t.Error("the bar reporter drew nothing")
	}
}

func TestNewRejectsAnUnknownMode(t *testing.T) {
	t.Parallel()
	if _, err := New(Mode("loud"), &syncBuffer{}, &capture{}); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

func TestLogTrackerReportsOnDone(t *testing.T) {
	t.Parallel()
	log := &capture{}
	r, err := New(ModeNever, &syncBuffer{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tr := r.Track("bills", 0, UnitsCount)
	tr.SetTotal(120)
	tr.Add(50)
	tr.Add(70)
	tr.Message("3 pages")
	tr.Done()

	e, ok := log.find("info", "finished")
	if !ok {
		t.Fatal("no finished line was logged")
	}
	if v, _ := argValue(e, "task"); v != "bills" {
		t.Errorf("task = %v, want bills", v)
	}
	if v, _ := argValue(e, "count"); v != int64(120) {
		t.Errorf("count = %v, want 120", v)
	}
	if v, _ := argValue(e, "expected"); v != int64(120) {
		t.Errorf("expected = %v, want 120", v)
	}
	if v, _ := argValue(e, "detail"); v != "3 pages" {
		t.Errorf("detail = %v, want the message", v)
	}
}

func TestLogTrackerReportsBytes(t *testing.T) {
	t.Parallel()
	log := &capture{}
	r, err := New(ModeNever, &syncBuffer{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tr := r.Track("attachments", 0, UnitsBytes)
	tr.Add(2048)
	tr.Done()

	e, ok := log.find("info", "finished")
	if !ok {
		t.Fatal("no finished line was logged")
	}
	if v, _ := argValue(e, "bytes"); v != int64(2048) {
		t.Errorf("bytes = %v, want 2048", v)
	}
	if _, ok := argValue(e, "count"); ok {
		t.Error("a byte tracker should not also report a count")
	}
}

func TestLogTrackerReportsFailure(t *testing.T) {
	t.Parallel()
	log := &capture{}
	r, err := New(ModeNever, &syncBuffer{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	want := errors.New("rate limited")
	tr := r.Track("invoices", 0, UnitsCount)
	tr.Add(3)
	tr.Fail(want)

	e, ok := log.find("error", "failed")
	if !ok {
		t.Fatal("no failure line was logged")
	}
	v, ok := argValue(e, "error")
	if !ok {
		t.Fatal("the failure line carried no error")
	}
	got, isErr := v.(error)
	if !isErr || !errors.Is(got, want) {
		t.Errorf("error = %v, want %v", v, want)
	}
}

// A tracker that finished must stay finished, so a deferred Done after an
// explicit Fail cannot overwrite the failure with a success.
func TestLogTrackerFinishesOnce(t *testing.T) {
	t.Parallel()
	log := &capture{}
	r, err := New(ModeNever, &syncBuffer{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tr := r.Track("bills", 0, UnitsCount)
	tr.Fail(errors.New("boom"))
	tr.Done()
	tr.Done()

	if _, ok := log.find("info", "finished"); ok {
		t.Error("a failed tracker also reported success")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.events) != 1 {
		t.Errorf("logged %d events, want exactly 1", len(log.events))
	}
}

func TestLogfIsSurfaced(t *testing.T) {
	t.Parallel()
	log := &capture{}
	r, err := New(ModeNever, &syncBuffer{}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	r.Logf("skipped %d families", 3)
	if _, ok := log.find("info", "skipped 3 families"); !ok {
		t.Error("Logf did not reach the logger")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, mode := range []Mode{ModeNever, ModeAlways} {
		r, err := New(mode, &syncBuffer{}, &capture{})
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		r.Close()
	}
}

// Workers update their own trackers concurrently, so this runs under -race to
// prove neither implementation needs the caller to serialise.
func TestTrackersAreConcurrencySafe(t *testing.T) {
	t.Parallel()
	for _, mode := range []Mode{ModeNever, ModeAlways} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			r, err := New(mode, &syncBuffer{}, &capture{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			var wg sync.WaitGroup
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					tr := r.Track("worker", 100, UnitsCount)
					for range 100 {
						tr.Add(1)
					}
					tr.Message("done")
					if i%2 == 0 {
						tr.Done()
					} else {
						tr.Fail(errors.New("nope"))
					}
					r.Logf("worker %d finished", i)
				}()
			}
			wg.Wait()
		})
	}
}

// The label was capped at 32 characters while bank_transaction_explanations is
// 29 on its own, so six consecutive rows all read
// "bank_transaction_explanations [~" and nothing said which account each was.
// The bracketed scope is the only distinguishing part, so it survives and the
// family gives up the room.
func TestFitLabelKeepsTheScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		label string
		width int
		want  string
	}{
		{
			name:  "fits untouched",
			label: "bank_transactions [Tide]",
			width: 40,
			want:  "bank_transactions [Tide]",
		},
		{
			name:  "family gives up room, scope intact",
			label: "bank_transaction_explanations [Tide Saving Account]",
			width: 40,
			want:  "bank_transaction_… [Tide Saving Account]",
		},
		{
			name:  "an unscoped label truncates normally",
			label: "credit_note_reconciliations",
			width: 20,
			want:  "credit_note_reconci…",
		},
		{
			name:  "no room for both, the scope is the useful half",
			label: "bank_transaction_explanations [Revolut GBP Savings]",
			width: 22,
			want:  " [Revolut GBP Savings]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fitLabel(tc.label, tc.width)
			if got != tc.want {
				t.Errorf("fitLabel(%q, %d) = %q, want %q", tc.label, tc.width, got, tc.want)
			}
			// Measured in runes: the ellipsis is three bytes and one cell, and
			// counting bytes is exactly what made the first attempt overrun.
			if cells := utf8.RuneCountInString(got); cells > tc.width {
				t.Errorf("result is %d cells, over the budget of %d", cells, tc.width)
			}
		})
	}
}

// Whatever the budget, a scoped label must never come back with the scope
// missing entirely: that is the state the fix exists to prevent.
func TestFitLabelNeverLosesTheScopeEntirely(t *testing.T) {
	t.Parallel()
	const label = "bank_transaction_explanations [Tide Saving Account]"
	for width := 12; width <= 60; width++ {
		got := fitLabel(label, width)
		if cells := utf8.RuneCountInString(got); cells > width {
			t.Fatalf("width %d: %q is %d cells", width, got, cells)
		}
		if !strings.Contains(got, "[") {
			t.Errorf("width %d: %q lost the scope", width, got)
		}
	}
}

// The budget comes from the terminal, so a wide one shows the whole label and a
// narrow one still shows something.
func TestMessageLengthFollowsTheTerminal(t *testing.T) {
	t.Parallel()
	// Not a terminal, so the assumed width applies and the result is bounded.
	got := messageLengthFor(&syncBuffer{})
	if got < minMessageLength || got > maxMessageLength {
		t.Errorf("messageLength = %d, want between %d and %d",
			got, minMessageLength, maxMessageLength)
	}
	if got != maxMessageLength {
		t.Errorf("messageLength = %d, want the full budget when width is unknown", got)
	}

	// And at that budget the longest real label keeps its account intact, which
	// is the whole point of the budget being wide enough.
	const longest = "bank_transaction_explanations [Tide Saving Account]"
	if fitted := fitLabel(longest, got); !strings.Contains(fitted, "Tide Saving Account]") {
		t.Errorf("fitted = %q, want the account name whole", fitted)
	}
}

func TestClamp(t *testing.T) {
	t.Parallel()
	tests := []struct{ value, low, high, want int }{
		{5, 1, 10, 5},
		{0, 1, 10, 1},
		{99, 1, 10, 10},
	}
	for _, tc := range tests {
		if got := clamp(tc.value, tc.low, tc.high); got != tc.want {
			t.Errorf("clamp(%d, %d, %d) = %d, want %d",
				tc.value, tc.low, tc.high, got, tc.want)
		}
	}
}

// A family with no records never learns a total, so its bar rendered as ???
// and then stayed that way. Anything finished is complete by definition.
func TestFinishingWithoutATotalStillCompletes(t *testing.T) {
	t.Parallel()
	out := &syncBuffer{}
	r, err := New(ModeAlways, out, &capture{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Total 0 and nothing added: the empty-family case.
	tr := r.Track("timeslips", 0, UnitsCount)
	tr.Done()

	bar, ok := tr.(*barTracker)
	if !ok {
		t.Fatalf("tracker is %T", tr)
	}
	if got := bar.total.Load(); got <= 0 {
		t.Errorf("total = %d after Done, want a positive one so the bar resolves", got)
	}
	if !bar.t.IsDone() {
		t.Error("the tracker did not finish")
	}
	if bar.t.PercentDone() < 100 {
		t.Errorf("percent = %.0f after Done, want 100", bar.t.PercentDone())
	}
}

// A tracker that did learn a total keeps it: the fallback must not overwrite a
// real number.
func TestFinishingKeepsAKnownTotal(t *testing.T) {
	t.Parallel()
	r, err := New(ModeAlways, &syncBuffer{}, &capture{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tr := r.Track("bills", 0, UnitsCount)
	tr.SetTotal(7)
	tr.Add(7)
	tr.Done()

	bar := tr.(*barTracker)
	if got := bar.total.Load(); got != 7 {
		t.Errorf("total = %d, want the 7 that was set", got)
	}
}
