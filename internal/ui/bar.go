package ui

import (
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jedib0t/go-pretty/v6/progress"
	"golang.org/x/term"
)

// renderFrequency is how often the display repaints. Fast enough to look
// live, slow enough not to compete with the work for CPU.
const renderFrequency = 100 * time.Millisecond

// stopTimeout bounds the wait for the render goroutine to finish, so a
// display bug can never wedge a cron run at exit.
const stopTimeout = 2 * time.Second

// Layout budget. The message gets whatever the terminal has left after the bar
// and the statistics, within these bounds.
const (
	trackerLength = 25
	// Room for the trailing "... done! [4 in 2.309s; 1/s]" and its padding.
	statsLength = 38
	// A label shorter than this is not worth showing, so a very narrow
	// terminal gets a cramped message rather than none.
	minMessageLength = 28
	// Long enough for the longest real label, which is a bank-scoped family
	// plus an account name, at around fifty characters.
	maxMessageLength = 72
)

// barReporter draws go-pretty progress bars.
type barReporter struct {
	pw  progress.Writer
	log Logger
	// messageLength is how much room a label has, so labels can be fitted
	// before go-pretty truncates them from the right and eats the scope.
	messageLength int

	once sync.Once
	done chan struct{}
}

func newBarReporter(out io.Writer, log Logger) *barReporter {
	messageLength := messageLengthFor(out)

	pw := progress.NewWriter()
	pw.SetOutputWriter(out)
	pw.SetUpdateFrequency(renderFrequency)
	pw.SetTrackerLength(trackerLength)
	pw.SetMessageLength(messageLength)
	pw.SetSortBy(progress.SortByPercentDsc)
	// Without this the display exits the moment the last tracker finishes,
	// which loses the bars during the gaps between phases of a pull.
	pw.SetAutoStop(false)

	pw.SetStyle(progress.StyleBlocks)
	style := pw.Style()
	style.Visibility.TrackerOverall = true
	style.Visibility.ETA = true
	style.Visibility.ETAOverall = true
	style.Visibility.Percentage = true
	style.Visibility.Speed = true
	style.Visibility.Time = true
	style.Visibility.Value = true

	r := &barReporter{
		pw: pw, log: log, messageLength: messageLength, done: make(chan struct{}),
	}
	go func() {
		defer close(r.done)
		pw.Render()
	}()
	return r
}

func (r *barReporter) Track(name string, total int64, units Units) Tracker {
	t := &progress.Tracker{
		Message: fitLabel(name, r.messageLength),
		Total:   total,
		Units:   unitsFor(units),
	}
	r.pw.AppendTracker(t)

	tracker := &barTracker{t: t}
	tracker.total.Store(total)
	return tracker
}

func (r *barReporter) Logf(format string, args ...any) {
	r.pw.Log(format, args...)
}

func (r *barReporter) Close() {
	r.once.Do(func() {
		r.pw.Stop()
		select {
		case <-r.done:
		case <-time.After(stopTimeout):
			r.log.Warn("progress display did not stop cleanly")
		}
	})
}

// messageLengthFor gives the label whatever the terminal has spare. When the
// width cannot be measured there is no line to fit into, so the label gets the
// full budget.
func messageLengthFor(out io.Writer) int {
	f, ok := out.(*os.File)
	if !ok {
		return maxMessageLength
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return maxMessageLength
	}
	return clamp(width-trackerLength-statsLength, minMessageLength, maxMessageLength)
}

// fitLabel shortens a label to width, trimming the family rather than the
// scope.
//
// A run shows six consecutive bank_transaction_explanations rows, and the
// account in brackets is the only thing telling them apart. Truncating from
// the right, which is what the display does by default, removes exactly the
// part that carries the information.
func fitLabel(name string, width int) string {
	// Counted in runes throughout. The display measures cells, and the ellipsis
	// below is three bytes but one cell, so counting bytes overruns the budget.
	if utf8.RuneCountInString(name) <= width {
		return name
	}

	open := strings.LastIndex(name, " [")
	if open < 0 || !strings.HasSuffix(name, "]") {
		return truncate(name, width)
	}

	family, scope := name[:open], name[open:]
	// Keep the scope whole if the family can give up enough room for it.
	if room := width - utf8.RuneCountInString(scope); room >= minFamilyLength {
		return truncate(family, room) + scope
	}
	// Otherwise there is not room for both, and the scope is the useful half.
	return truncate(scope, width)
}

// minFamilyLength is how much of a family name still identifies it.
const minFamilyLength = 8

// truncate shortens with a trailing ellipsis, so a shortened label is visibly
// shortened rather than looking like a different name.
func truncate(s string, width int) string {
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width <= 1 {
		return string(runes[:max(width, 0)])
	}
	return string(runes[:width-1]) + "\u2026"
}

func clamp(value, low, high int) int {
	return min(max(value, low), high)
}

func unitsFor(u Units) progress.Units {
	if u == UnitsBytes {
		return progress.UnitsBytes
	}
	return progress.UnitsDefault
}

// barTracker adapts one go-pretty tracker. go-pretty guards its own state, so
// the only thing needed here is to keep post-Done calls harmless and to
// remember the total, which is read back at Done.
type barTracker struct {
	t *progress.Tracker
	// total mirrors what was set, kept here rather than read off the tracker so
	// Done does not race a concurrent SetTotal.
	total atomic.Int64
}

func (b *barTracker) Add(n int64) {
	if n != 0 && !b.t.IsDone() {
		b.t.Increment(n)
	}
}

func (b *barTracker) SetTotal(n int64) {
	b.total.Store(n)
	b.t.UpdateTotal(n)
}

func (b *barTracker) Message(s string) { b.t.UpdateMessage(s) }

func (b *barTracker) Done() {
	if b.t.IsDone() {
		return
	}
	// A tracker that never learned a total renders as ??? forever, which is
	// what an empty family did: nothing to count, so nothing ever set one.
	// Anything finished is complete by definition, so give it one.
	if b.total.Load() <= 0 {
		done := b.t.Value()
		if done <= 0 {
			done = 1
		}
		b.SetTotal(done)
		b.t.SetValue(done)
	}
	b.t.MarkAsDone()
}

// Fail leaves the bar visible and marked, so a partial run is obvious at a
// glance rather than only in the exit code.
func (b *barTracker) Fail(err error) {
	if b.t.IsDone() {
		return
	}
	if err != nil {
		b.t.UpdateMessage(b.t.Message + ": " + err.Error())
	}
	b.t.MarkAsErrored()
}
