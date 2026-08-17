package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrNoSuchAccount is returned when a slug does not name a stored account.
var ErrNoSuchAccount = errors.New("store: no such account")

// ErrAccountExists is returned when a slug is already taken.
var ErrAccountExists = errors.New("store: account already exists")

// slugPattern keeps a slug usable as a token-store key, a path segment and a
// command-line argument without any escaping anywhere.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Account is one FreeAgent company plus the credentials that reach it.
type Account struct {
	ID          int64
	Slug        string
	Name        string
	Environment string
	CompanyURL  string
	CreatedAt   time.Time
	DisabledAt  time.Time
}

// Disabled reports an account excluded from routine runs.
func (a Account) Disabled() bool { return !a.DisabledAt.IsZero() }

// ValidateSlug checks a slug before it reaches the database, so the error
// names the rule rather than surfacing as a constraint violation.
func ValidateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf(
			"store: %q is not a valid account slug; use lowercase letters, "+
				"digits and hyphens, starting with a letter or digit", slug)
	}
	return nil
}

// AddAccount stores a new account. The environment string is validated by the
// caller against the SDK, which owns the list of deployments.
func (d *DB) AddAccount(ctx context.Context, slug, name, environment string) (*Account, error) {
	if err := ValidateSlug(slug); err != nil {
		return nil, err
	}
	if environment == "" {
		return nil, errors.New("store: AddAccount requires an environment")
	}

	now := time.Now()
	res, err := d.ExecContext(ctx,
		`INSERT INTO accounts (slug, name, environment, created_at)
		 VALUES (?, ?, ?, ?)`,
		slug, name, environment, FormatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: %s", ErrAccountExists, slug)
		}
		return nil, fmt.Errorf("store: adding account %q: %w", slug, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("store: adding account %q: %w", slug, err)
	}
	return &Account{
		ID: id, Slug: slug, Name: name,
		Environment: environment, CreatedAt: now,
	}, nil
}

// Accounts lists every stored account, disabled ones included, in slug order.
func (d *DB) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, slug, name, environment, company_url, created_at, disabled_at
		 FROM accounts ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("store: listing accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing accounts: %w", err)
	}
	return out, nil
}

// AccountBySlug looks one account up.
func (d *DB) AccountBySlug(ctx context.Context, slug string) (*Account, error) {
	row := d.QueryRowContext(ctx,
		`SELECT id, slug, name, environment, company_url, created_at, disabled_at
		 FROM accounts WHERE slug = ?`, slug)

	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchAccount, slug)
	}
	return a, err
}

// OnlyAccount returns the single stored account, so a one-account setup never
// has to pass --account. It is an error when there is a choice to make.
func (d *DB) OnlyAccount(ctx context.Context) (*Account, error) {
	all, err := d.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	switch len(all) {
	case 0:
		return nil, ErrNoSuchAccount
	case 1:
		return &all[0], nil
	}
	return nil, fmt.Errorf(
		"store: %d accounts are configured, choose one with --account", len(all))
}

// RemoveAccount deletes an account that has no archived records. Records are
// never deleted implicitly: an archive is the point of this tool, so removing
// one has to be a deliberate, separate act.
func (d *DB) RemoveAccount(ctx context.Context, slug string) error {
	account, err := d.AccountBySlug(ctx, slug)
	if err != nil {
		return err
	}

	var records int
	if err := d.QueryRowContext(ctx,
		"SELECT count(*) FROM records WHERE account_id = ?", account.ID).Scan(&records); err != nil {
		return fmt.Errorf("store: counting records for %q: %w", slug, err)
	}
	if records > 0 {
		return fmt.Errorf(
			"store: account %q still holds %d archived records; "+
				"they are not deleted automatically", slug, records)
	}

	if _, err := d.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", account.ID); err != nil {
		return fmt.Errorf("store: removing account %q: %w", slug, err)
	}
	return nil
}

// SetAccountCompanyURL records the company a token actually reaches, learned
// on the first successful call rather than asked for at setup.
func (d *DB) SetAccountCompanyURL(ctx context.Context, id int64, companyURL string) error {
	if _, err := d.ExecContext(ctx,
		"UPDATE accounts SET company_url = ? WHERE id = ?", companyURL, id); err != nil {
		return fmt.Errorf("store: recording the company URL: %w", err)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanAccount(s scanner) (*Account, error) {
	var (
		a          Account
		created    string
		companyURL sql.NullString
		disabled   sql.NullString
	)
	if err := s.Scan(&a.ID, &a.Slug, &a.Name, &a.Environment,
		&companyURL, &created, &disabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("store: reading an account: %w", err)
	}

	var err error
	if a.CreatedAt, err = ParseTime(created); err != nil {
		return nil, err
	}
	if disabled.Valid {
		if a.DisabledAt, err = ParseTime(disabled.String); err != nil {
			return nil, err
		}
	}
	a.CompanyURL = companyURL.String
	return &a, nil
}

// isUniqueViolation recognises a duplicate slug without depending on the
// driver's error type, which is not exported in a usable form.
func isUniqueViolation(err error) bool {
	return err != nil &&
		strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}
