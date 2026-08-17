package engine

import (
	"strings"
	"testing"
)

func probeResultFor(t *testing.T, results []ProbeResult, family string) ProbeResult {
	t.Helper()
	for _, r := range results {
		if r.Family == family {
			return r
		}
	}
	t.Fatalf("no probe result for %s", family)
	return ProbeResult{}
}

func TestProbeDetectsAHonouredFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	results, err := h.engine.Probe(t.Context(), []string{"bills"})
	if err != nil {
		t.Fatal(err)
	}

	got := probeResultFor(t, results, "bills")
	if got.Result != ProbeHonoured {
		t.Errorf("result = %s (%s), want honoured", got.Result, got.Detail)
	}

	state, err := h.db.FamilyState(t.Context(), h.account.ID, "bills", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.SupportsUpdatedSince == nil || !*state.SupportsUpdatedSince {
		t.Error("a honoured probe did not record support")
	}
}

// A family that ignores the filter returns everything, which looks exactly
// like success. This is the case the whole probe exists for.
func TestProbeDetectsAnIgnoredFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.ignoreUpdatedSince("bills")
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	results, err := h.engine.Probe(t.Context(), []string{"bills"})
	if err != nil {
		t.Fatal(err)
	}

	got := probeResultFor(t, results, "bills")
	if got.Result != ProbeIgnored {
		t.Fatalf("result = %s, want ignored", got.Result)
	}
	if !strings.Contains(got.Detail, "not applied") {
		t.Errorf("detail = %q, want it to explain what was observed", got.Detail)
	}

	state, err := h.db.FamilyState(t.Context(), h.account.ID, "bills", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.SupportsUpdatedSince == nil || *state.SupportsUpdatedSince {
		t.Error("an ignored probe did not record the lack of support")
	}
}

// An empty family proves nothing either way, so the capability must stay
// unknown rather than being recorded as working.
func TestProbeOnAnEmptyFamilyConcludesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills")

	results, err := h.engine.Probe(t.Context(), []string{"bills"})
	if err != nil {
		t.Fatal(err)
	}

	if got := probeResultFor(t, results, "bills"); got.Result != ProbeNoData {
		t.Errorf("result = %s, want no-data", got.Result)
	}

	state, err := h.db.FamilyState(t.Context(), h.account.ID, "bills", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.SupportsUpdatedSince != nil {
		t.Errorf("an untestable family recorded support = %v, want unknown",
			*state.SupportsUpdatedSince)
	}
}

func TestProbeRecordsAFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.failWith("bills", 500)

	results, err := h.engine.Probe(t.Context(), []string{"bills"})
	if err != nil {
		t.Fatal(err)
	}

	got := probeResultFor(t, results, "bills")
	if got.Result != ProbeFailed || got.Err == nil {
		t.Errorf("result = %+v, want a recorded failure", got)
	}

	state, err := h.db.FamilyState(t.Context(), h.account.ID, "bills", "")
	if err != nil {
		t.Fatal(err)
	}
	if state.SupportsUpdatedSince != nil {
		t.Error("a failed probe asserted a capability")
	}
}

// Two requests per family: one to see whether it has records at all, one to
// test the filter. Cheap enough to run across every family.
func TestProbeCostsTwoRequestsPerFamily(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})
	h.fake.set("contacts", fakeRecord{ID: 1, UpdatedAt: march})

	if _, err := h.engine.Probe(t.Context(), []string{"bills", "contacts"}); err != nil {
		t.Fatal(err)
	}
	if got := h.fake.requestCount(); got != 4 {
		t.Errorf("probe made %d requests for two families, want 4", got)
	}
}

func TestProbeRecordsTheRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	if _, err := h.engine.Probe(t.Context(), []string{"bills"}); err != nil {
		t.Fatal(err)
	}

	runs, err := h.db.RecentRuns(t.Context(), h.account.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Mode != "probe" {
		t.Errorf("runs = %+v, want one probe run", runs)
	}
}

// A probe must never move a cursor: it reads one record per family and knows
// nothing about the rest.
func TestProbeDoesNotAdvanceCursors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})

	if _, err := h.engine.Probe(t.Context(), []string{"bills"}); err != nil {
		t.Fatal(err)
	}
	if !h.cursor("bills").IsZero() {
		t.Errorf("cursor = %s after a probe, want zero", h.cursor("bills"))
	}
}
