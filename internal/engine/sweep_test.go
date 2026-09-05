package engine

import (
	"net/http"
	"testing"
	"time"

	"github.com/alekc/freeagent-sync/internal/store"
)

// records builds n fake bills, which is the cheapest way to get a family big
// enough for a fraction to mean something.
func records(n int) []fakeRecord {
	return recordsFrom(1, n)
}

// recordsFrom builds n fake bills with ids the archive has not seen, for the
// case where the far end answers with a different set rather than a subset.
func recordsFrom(first, n int) []fakeRecord {
	out := make([]fakeRecord, 0, n)
	for i := first; i < first+n; i++ {
		out = append(out, fakeRecord{ID: i, UpdatedAt: march})
	}
	return out
}

// A sweep either side of the bound. The interesting case is the boundary: a
// guard that refuses everything is as useless as one that refuses nothing.
func TestSweepBoundRefusesOnlyWhatIsOverIt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		remaining int
		want      SweepState
	}{
		// 40 live, so 4 unseen is exactly the 10% bound and survives it.
		{"at the bound", 36, SweepDone},
		{"over the bound", 35, SweepRefused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.fake.set("bills", records(40)...)
			h.pull(Options{Mode: store.ModeFull})

			h.fake.set("bills", records(tc.remaining)...)
			result := h.pull(Options{Mode: store.ModeFull, Reconcile: true})

			bills := h.familyResult(result, "bills")
			if bills.Sweep != tc.want {
				t.Errorf("sweep = %q, want %q", bills.Sweep, tc.want)
			}
			if got := int(bills.Deleted); got != 40-tc.remaining {
				t.Errorf("sweep set = %d, want %d", got, 40-tc.remaining)
			}
		})
	}
}

// The incident this bound exists for: a read that completes and reports itself
// full while the endpoint quietly answered a fraction of the family. Nothing
// distinguishes it from a genuine mass deletion except its size.
func TestAShortWalkIsNotSweptToNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(40)...)
	h.pull(Options{Mode: store.ModeFull})

	// The far end answers four of forty and calls it a complete walk.
	h.fake.set("bills", records(4)...)
	result := h.pull(Options{Mode: store.ModeFull, Reconcile: true})

	if got := h.liveCount("bills"); got != 40 {
		t.Errorf("live count = %d, want all 40 intact", got)
	}
	bills := h.familyResult(result, "bills")
	if bills.Sweep != SweepRefused {
		t.Errorf("sweep = %q, want the sweep refused", bills.Sweep)
	}
	// The run must fail loudly: a cron job reads the exit code, not the table.
	if result.Outcome != store.OutcomePartial {
		t.Errorf("outcome = %s, want partial so the run is noticed", result.Outcome)
	}
	if bills.Err == nil {
		t.Error("a refused sweep did not name itself as a failure")
	}
}

// A bound measured after the read can be diluted by the read itself: forty
// records replaced by four hundred different ones is the whole family going,
// but only 9% of what the archive holds once the new ones are in.
func TestSweepBoundIsMeasuredBeforeTheRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(40)...)
	h.pull(Options{Mode: store.ModeFull})

	// A different forty-times-larger set, sharing not one id with the archive.
	h.fake.set("bills", recordsFrom(1000, 400)...)
	result := h.pull(Options{Mode: store.ModeFull, Reconcile: true})

	if got := h.familyResult(result, "bills").Sweep; got != SweepRefused {
		t.Errorf("sweep = %q, want all forty of the old records defended", got)
	}
	// The new ones are archived either way: what the bound withholds is the
	// delete, so the family holds both sets until the run is understood.
	if got := h.liveCount("bills"); got != 440 {
		t.Errorf("live count = %d, want 440", got)
	}
}

// A small family is all boundary: three records losing one is a third of it.
// Bounding those would make them permanently unsweepable.
func TestSweepBoundDoesNotApplyBelowTheFloor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(5)...)
	h.pull(Options{Mode: store.ModeFull})

	h.fake.set("bills", records(1)...)
	result := h.pull(Options{Mode: store.ModeFull, Reconcile: true})

	if got := h.familyResult(result, "bills").Sweep; got != SweepDone {
		t.Errorf("sweep = %q, want a small family swept without a bound", got)
	}
	if got := h.liveCount("bills"); got != 1 {
		t.Errorf("live count = %d, want 1", got)
	}
}

// Removing the bound has to be possible, or a genuine mass deletion can never
// be recorded. Allowing all of it says so without a sentinel.
func TestMaxSweepFractionOfOneRemovesTheBound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(40)...)
	h.pull(Options{Mode: store.ModeFull})

	h.fake.set("bills", records(1)...)
	result := h.pull(Options{
		Mode: store.ModeFull, Reconcile: true, MaxSweepFraction: 1,
	})

	if got := h.familyResult(result, "bills").Sweep; got != SweepDone {
		t.Errorf("sweep = %q, want the bound removed", got)
	}
	if got := h.liveCount("bills"); got != 1 {
		t.Errorf("live count = %d, want 1", got)
	}
}

// The point of a preview: the count is real and the archive is untouched.
func TestDryRunReportsTheSweepWithoutDeleting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(40)...)
	h.pull(Options{Mode: store.ModeFull})

	h.fake.set("bills", records(38)...)
	result := h.pull(Options{Mode: store.ModeFull, Reconcile: true, DryRun: true})

	bills := h.familyResult(result, "bills")
	if bills.Sweep != SweepPreview {
		t.Errorf("sweep = %q, want a preview", bills.Sweep)
	}
	if bills.Deleted != 2 {
		t.Errorf("preview said %d would go, want 2", bills.Deleted)
	}
	if got := h.liveCount("bills"); got != 40 {
		t.Errorf("live count = %d, want all 40 still live after a dry run", got)
	}
	// A preview is not a deletion, so the run must not report one.
	if result.Deleted != 0 {
		t.Errorf("run reported %d deleted on a dry run", result.Deleted)
	}
}

// A dry run that recorded a reconcile would silence --reconcile-if-due for a
// whole interval while having changed nothing, which is the worst of both.
func TestDryRunDoesNotRecordTheSweep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(40)...)
	h.pull(Options{Mode: store.ModeFull})

	h.fake.set("bills", records(38)...)
	h.pull(Options{Mode: store.ModeFull, Reconcile: true, DryRun: true})

	state, err := h.db.FamilyState(t.Context(), h.account.ID, "bills", "")
	if err != nil {
		t.Fatal(err)
	}
	if !state.LastFullReconcile.IsZero() {
		t.Errorf("a dry run recorded a reconcile at %s", state.LastFullReconcile)
	}
}

// A preview must answer what a real sweep would do, not something more
// permissive, or it reassures about exactly the run that would have failed.
func TestDryRunIsBoundedTheSameWay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(40)...)
	h.pull(Options{Mode: store.ModeFull})

	h.fake.set("bills", records(4)...)
	result := h.pull(Options{Mode: store.ModeFull, Reconcile: true, DryRun: true})

	if got := h.familyResult(result, "bills").Sweep; got != SweepRefused {
		t.Errorf("sweep = %q, want a dry run refused by the same bound", got)
	}
}

// The ambiguity the whole issue is about: a family nothing checked reported
// the same zero as one that was checked and found clean.
func TestASkippedSweepIsDistinctFromACleanOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(3)...)
	h.pull(Options{Mode: store.ModeFull})

	// Incremental: the read sees only what changed, so it cannot tell absence
	// from unchanged and the sweep is refused.
	skipped := h.familyResult(
		h.pull(Options{Mode: store.ModeIncremental, Reconcile: true}), "bills")
	// Full: the read covered the family and found nothing gone.
	clean := h.familyResult(
		h.pull(Options{Mode: store.ModeFull, Reconcile: true}), "bills")

	if skipped.Sweep != SweepSkipped {
		t.Errorf("unswept family reported %q, want skipped", skipped.Sweep)
	}
	if clean.Sweep != SweepDone {
		t.Errorf("fully read family reported %q, want swept", clean.Sweep)
	}
	if skipped.Deleted != 0 || clean.Deleted != 0 {
		t.Fatalf("both counts should be zero, got %d and %d",
			skipped.Deleted, clean.Deleted)
	}
	if skipped.Sweep == clean.Sweep {
		t.Error("a skipped sweep is indistinguishable from a clean one")
	}
}

// Not asking for a sweep is a third thing again, and must not read as either
// of the other two.
func TestAFamilyNobodyAskedToSweepReportsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(3)...)

	result := h.pull(Options{Mode: store.ModeFull})

	if got := h.familyResult(result, "bills").Sweep; got != SweepNone {
		t.Errorf("sweep = %q, want no sweep state at all", got)
	}
}

// A feature the company does not have is not a family the read failed to
// cover. Counting it as one would put every absent feature in the "not fully
// read" tally of every single run.
func TestAnUnavailableFamilyIsNotReportedAsSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.setRaw("users", `{"url":"`+h.apiURL+`/v2/users/2"}`)
	h.fake.failWith("users/2/self_assessment_returns", http.StatusForbidden)

	result := h.pull(Options{
		Mode:      store.ModeFull,
		Reconcile: true,
		Families:  []string{"users", "income_tax_returns"},
	})

	for _, f := range result.Families {
		if f.Family == "income_tax_returns" && f.Sweep != SweepNone {
			t.Errorf("an unavailable family reported sweep %q, want none", f.Sweep)
		}
	}
}

// --reconcile-if-due inside its interval is cadence, not a refusal, so it must
// not be reported as a family the read failed to cover.
func TestSweepNotDueReportsNoState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", records(3)...)

	first := h.pull(Options{
		Mode: store.ModeFull, ReconcileIfDue: true, ReconcileInterval: time.Hour,
	})
	if got := h.familyResult(first, "bills").Sweep; got != SweepDone {
		t.Fatalf("first sweep = %q, want a never-swept family swept", got)
	}

	second := h.pull(Options{
		Mode: store.ModeFull, ReconcileIfDue: true, ReconcileInterval: time.Hour,
	})
	if got := h.familyResult(second, "bills").Sweep; got != SweepNone {
		t.Errorf("sweep = %q inside the interval, want no state", got)
	}
}
