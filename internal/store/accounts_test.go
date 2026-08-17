package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAddAndReadAccount(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	added, err := db.AddAccount(t.Context(), "acme", "Acme Ltd", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if added.ID == 0 {
		t.Error("AddAccount returned no id")
	}
	if added.CreatedAt.IsZero() {
		t.Error("AddAccount did not stamp CreatedAt")
	}

	got, err := db.AccountBySlug(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "acme" || got.Name != "Acme Ltd" || got.Environment != "sandbox" {
		t.Errorf("read back %+v, want the account as stored", got)
	}
	if got.Disabled() {
		t.Error("a new account should not be disabled")
	}
	// Stored as text, so the round trip through the archive's layout is
	// what is actually being checked here.
	if delta := got.CreatedAt.Sub(added.CreatedAt); delta > time.Second || delta < -time.Second {
		t.Errorf("CreatedAt drifted by %s across the round trip", delta)
	}
}

func TestAddAccountRejectsDuplicates(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	if _, err := db.AddAccount(t.Context(), "acme", "", "sandbox"); err != nil {
		t.Fatal(err)
	}
	_, err := db.AddAccount(t.Context(), "acme", "", "production")
	if !errors.Is(err, ErrAccountExists) {
		t.Fatalf("second add returned %v, want ErrAccountExists", err)
	}
}

func TestAddAccountValidatesTheSlug(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	bad := []string{"", "Acme", "acme ltd", "-acme", "acme/ltd", "acme.ltd",
		strings.Repeat("a", 64)}
	for _, slug := range bad {
		if _, err := db.AddAccount(t.Context(), slug, "", "sandbox"); err == nil {
			t.Errorf("slug %q was accepted", slug)
		}
	}

	good := []string{"a", "acme", "acme-ltd", "acme2", "0acme",
		strings.Repeat("a", 63)}
	for i, slug := range good {
		if _, err := db.AddAccount(t.Context(), slug, "", "sandbox"); err != nil {
			t.Errorf("slug %q (case %d) was rejected: %v", slug, i, err)
		}
	}
}

func TestAddAccountRequiresAnEnvironment(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	if _, err := db.AddAccount(t.Context(), "acme", "", ""); err == nil {
		t.Fatal("an account with no environment was accepted")
	}
}

func TestAccountBySlugReportsMissing(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	_, err := db.AccountBySlug(t.Context(), "nope")
	if !errors.Is(err, ErrNoSuchAccount) {
		t.Fatalf("error = %v, want ErrNoSuchAccount", err)
	}
}

func TestAccountsAreListedInSlugOrder(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	for _, slug := range []string{"zulu", "alpha", "mike"} {
		if _, err := db.AddAccount(t.Context(), slug, "", "sandbox"); err != nil {
			t.Fatal(err)
		}
	}

	all, err := db.Accounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mike", "zulu"}
	if len(all) != len(want) {
		t.Fatalf("listed %d accounts, want %d", len(all), len(want))
	}
	for i, slug := range want {
		if all[i].Slug != slug {
			t.Errorf("account %d = %q, want %q", i, all[i].Slug, slug)
		}
	}
}

// A single-account setup should never have to pass --account, but two
// accounts must not be silently guessed between.
func TestOnlyAccount(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	if _, err := db.OnlyAccount(t.Context()); !errors.Is(err, ErrNoSuchAccount) {
		t.Errorf("with no accounts, error = %v, want ErrNoSuchAccount", err)
	}

	if _, err := db.AddAccount(t.Context(), "acme", "", "sandbox"); err != nil {
		t.Fatal(err)
	}
	only, err := db.OnlyAccount(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if only.Slug != "acme" {
		t.Errorf("OnlyAccount = %q, want acme", only.Slug)
	}

	if _, err := db.AddAccount(t.Context(), "other", "", "sandbox"); err != nil {
		t.Fatal(err)
	}
	_, err = db.OnlyAccount(t.Context())
	if err == nil {
		t.Fatal("OnlyAccount picked one of two accounts")
	}
	if !strings.Contains(err.Error(), "--account") {
		t.Errorf("error = %q, want it to name the flag", err)
	}
}

// The archive is the point of the tool, so dropping an account must not take
// archived records with it as a side effect.
func TestRemoveAccountRefusesWhenRecordsExist(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	account, err := db.AddAccount(t.Context(), "acme", "", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	now := FormatTime(time.Now())
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO records
		 (account_id, family, url, body, body_sha256, first_seen_at, last_seen_at)
		 VALUES (?, 'bills', 'https://example.test/bills/1', '{}', 'abc', ?, ?)`,
		account.ID, now, now); err != nil {
		t.Fatal(err)
	}

	err = db.RemoveAccount(t.Context(), "acme")
	if err == nil {
		t.Fatal("an account with archived records was removed")
	}
	if !strings.Contains(err.Error(), "archived records") {
		t.Errorf("error = %q, want it to explain the refusal", err)
	}
}

func TestRemoveAccountWhenEmpty(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	if _, err := db.AddAccount(t.Context(), "acme", "", "sandbox"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveAccount(t.Context(), "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AccountBySlug(t.Context(), "acme"); !errors.Is(err, ErrNoSuchAccount) {
		t.Errorf("account survived removal: %v", err)
	}
}

func TestRemoveAccountReportsMissing(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	if err := db.RemoveAccount(t.Context(), "nope"); !errors.Is(err, ErrNoSuchAccount) {
		t.Fatalf("error = %v, want ErrNoSuchAccount", err)
	}
}

func TestSetAccountCompanyURL(t *testing.T) {
	t.Parallel()
	db := openTemp(t)

	account, err := db.AddAccount(t.Context(), "acme", "", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://api.sandbox.freeagent.com/v2/company"
	if err := db.SetAccountCompanyURL(t.Context(), account.ID, url); err != nil {
		t.Fatal(err)
	}

	got, err := db.AccountBySlug(t.Context(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.CompanyURL != url {
		t.Errorf("CompanyURL = %q, want %q", got.CompanyURL, url)
	}
}
