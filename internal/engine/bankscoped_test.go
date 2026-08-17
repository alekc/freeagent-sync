package engine

import (
	"strings"
	"testing"

	"github.com/alekc/freeagent-sync/internal/store"
)

// seedBankAccounts puts two accounts in the stub, which is what the
// bank-scoped families fan out over.
func seedBankAccounts(h *harness) {
	h.fake.set("bank_accounts",
		fakeRecord{ID: 1, UpdatedAt: march, Extra: "Current"},
		fakeRecord{ID: 2, UpdatedAt: march, Extra: "Savings"},
	)
}

func TestBankScopedFanOutPerAccount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedBankAccounts(h)
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/1",
		fakeRecord{ID: 10, UpdatedAt: march})
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/2",
		fakeRecord{ID: 20, UpdatedAt: march}, fakeRecord{ID: 21, UpdatedAt: april})

	result, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeFull,
		Families: []string{"bank_accounts", "bank_transactions"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}

	if got := h.liveCount("bank_transactions"); got != 3 {
		t.Errorf("archived %d transactions, want 3 across both accounts", got)
	}

	// One result per scope, labelled by the account name rather than its id.
	var scoped []string
	for _, f := range result.Families {
		if f.Family == "bank_transactions" {
			scoped = append(scoped, f.Name())
		}
	}
	if len(scoped) != 2 {
		t.Fatalf("got %d scoped results, want 2: %v", len(scoped), scoped)
	}
	for _, want := range []string{"Current", "Savings"} {
		if !strings.Contains(strings.Join(scoped, " "), want) {
			t.Errorf("results %v do not name the %s account", scoped, want)
		}
	}
}

// Every request must carry the bank_account filter: the API rejects one
// without it, so a missing filter would fail the whole family remotely.
func TestBankScopedSendsTheAccountFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedBankAccounts(h)

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeFull,
		Families: []string{"bank_accounts", "bank_transactions"},
	}); err != nil {
		t.Fatal(err)
	}

	seen := h.fake.scopesSeen("bank_transactions")
	if len(seen) != 2 {
		t.Fatalf("saw %d distinct bank_account filters, want 2: %v", len(seen), seen)
	}
	for _, url := range []string{
		"https://api.test/v2/bank_accounts/1",
		"https://api.test/v2/bank_accounts/2",
	} {
		if !seen[url] {
			t.Errorf("no request carried bank_account=%s", url)
		}
	}
}

// Each account keeps its own cursor, so a quiet account is not dragged
// forward by a busy one.
func TestBankScopedCursorsArePerAccount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedBankAccounts(h)
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/1",
		fakeRecord{ID: 10, UpdatedAt: march})
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/2",
		fakeRecord{ID: 20, UpdatedAt: april})

	if _, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeFull,
		Families: []string{"bank_accounts", "bank_transactions"},
	}); err != nil {
		t.Fatal(err)
	}

	first := h.scopedCursor("bank_transactions", "https://api.test/v2/bank_accounts/1")
	second := h.scopedCursor("bank_transactions", "https://api.test/v2/bank_accounts/2")
	if !first.Equal(march) {
		t.Errorf("account 1 cursor = %s, want %s", first, march)
	}
	if !second.Equal(april) {
		t.Errorf("account 2 cursor = %s, want %s", second, april)
	}
}

// The sweep is per family but the read is per account, so a family may only
// be swept once every one of its accounts has been read in full. Sweeping
// after a partial fan-out would delete the accounts not yet reached.
func TestBankScopedSweepWaitsForEveryAccount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedBankAccounts(h)
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/1",
		fakeRecord{ID: 10, UpdatedAt: march})
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/2",
		fakeRecord{ID: 20, UpdatedAt: march})

	pull := func(opts Options) Result {
		t.Helper()
		opts.Families = []string{"bank_accounts", "bank_transactions"}
		result, err := h.engine.Pull(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	pull(Options{Mode: store.ModeFull, Reconcile: true})
	if got := h.liveCount("bank_transactions"); got != 2 {
		t.Fatalf("archived %d transactions, want 2", got)
	}

	// Now the second account errors. Nothing must be swept, even though the
	// first account was read cleanly and its records are all still present.
	h.fake.failScope("bank_transactions", "https://api.test/v2/bank_accounts/2", 500)
	result := pull(Options{Mode: store.ModeFull, Reconcile: true})

	if got := h.liveCount("bank_transactions"); got != 2 {
		t.Errorf("live count = %d after a partial fan-out, want 2 untouched", got)
	}
	for _, f := range result.Families {
		if f.Family == "bank_transactions" && f.Swept {
			t.Error("a family was swept while one of its accounts was failing")
		}
	}
}

// With every account read, a sweep must still work across the fan-out.
func TestBankScopedSweepAcrossAccounts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedBankAccounts(h)
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/1",
		fakeRecord{ID: 10, UpdatedAt: march}, fakeRecord{ID: 11, UpdatedAt: march})
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/2",
		fakeRecord{ID: 20, UpdatedAt: march})

	families := []string{"bank_accounts", "bank_transactions"}
	// Sequential on purpose. Read concurrently, the accounts start within
	// microseconds of each other and the sweep bound barely matters; read one
	// after another, a sweep bounded by the last account's start time would
	// delete everything the first account had already re-seen.
	opts := Options{
		Mode: store.ModeFull, Reconcile: true, Families: families, Concurrency: 1,
	}
	if _, err := h.engine.Pull(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	if got := h.liveCount("bank_transactions"); got != 3 {
		t.Fatalf("archived %d transactions, want 3", got)
	}

	// One transaction disappears from the first account.
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/1",
		fakeRecord{ID: 10, UpdatedAt: march})
	if _, err := h.engine.Pull(t.Context(), opts); err != nil {
		t.Fatal(err)
	}

	if got := h.liveCount("bank_transactions"); got != 2 {
		t.Errorf("live count = %d, want 2 after one was swept", got)
	}
	// The other account's record must survive the family-wide sweep.
	if _, err := h.db.RecordBody(
		t.Context(), h.account.ID, "https://api.test/v2/bank_transactions/20"); err != nil {
		t.Fatal(err)
	}
	live, err := h.db.LiveRecordBodies(t.Context(), h.account.ID, "bank_transactions")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(joinBodies(live)), "bank_transactions/20") {
		t.Error("the second account's transaction was swept along with the first's")
	}
}

// Bank transactions document no sort parameter, so sending one risks a 400 on
// a family that is a primary objective.
func TestBankScopedDoesNotSendASort(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedBankAccounts(h)
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/1",
		fakeRecord{ID: 10, UpdatedAt: march})

	families := []string{"bank_accounts", "bank_transactions"}
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeFull, Families: families,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.Pull(t.Context(), Options{
		Mode: store.ModeIncremental, Families: families,
	}); err != nil {
		t.Fatal(err)
	}

	if got := h.fake.queryFor("bank_transactions").Get("sort"); got != "" {
		t.Errorf("sort = %q was sent to bank_transactions, which documents none", got)
	}
	// A plain collection still gets one, because it is what makes a resumed
	// incremental walk follow the cursor.
	h.fake.set("bills", fakeRecord{ID: 1, UpdatedAt: march})
	h.pull(Options{Mode: store.ModeFull})
	h.pull(Options{Mode: store.ModeIncremental})
	if got := h.fake.queryFor("bills").Get("sort"); got != "updated_at" {
		t.Errorf("sort = %q on bills, want updated_at", got)
	}
}

// A run that names only a bank-scoped family has no archived accounts to fan
// out over, so it fetches them rather than failing.
func TestBankScopedFetchesAccountsWhenNoneArchived(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	seedBankAccounts(h)
	h.fake.setScoped("bank_transactions", "https://api.test/v2/bank_accounts/1",
		fakeRecord{ID: 10, UpdatedAt: march})

	result, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeFull,
		Families: []string{"bank_transactions"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Fatalf("outcome = %s, want ok", result.Outcome)
	}
	if got := h.liveCount("bank_transactions"); got != 1 {
		t.Errorf("archived %d transactions, want 1", got)
	}
	// The accounts it had to fetch are archived too, rather than discarded.
	if got := h.liveCount("bank_accounts"); got != 2 {
		t.Errorf("archived %d bank accounts, want 2", got)
	}
}

func TestBankScopedWithNoAccountsIsNotAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.fake.set("bank_accounts")

	result, err := h.engine.Pull(t.Context(), Options{
		Mode:     store.ModeFull,
		Families: []string{"bank_accounts", "bank_transactions"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != store.OutcomeOK {
		t.Errorf("outcome = %s, want ok for a company with no bank accounts", result.Outcome)
	}
}

func joinBodies(bodies [][]byte) []byte {
	var out []byte
	for _, b := range bodies {
		out = append(out, b...)
	}
	return out
}
