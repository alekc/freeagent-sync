package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/store"
)

// bankAccountsFamily is read before any bank-scoped family, because the
// scopes are its records.
const bankAccountsFamily = "bank_accounts"

// kind is how a job reads its family.
type kind int

const (
	// kindCollection walks pages of records.
	kindCollection kind = iota
	// kindDocument fetches one document and archives the whole envelope.
	kindDocument
	// kindReport fetches a derived report and snapshots it with its window.
	kindReport
	// kindPayrollYear fetches one tax year and then each of its periods.
	kindPayrollYear
)

// job is one unit of reading: a family, optionally narrowed to a scope.
//
// Most families are one job. The bank-scoped ones are one job per bank
// account, because the API rejects a request without a bank_account filter,
// and each account keeps its own cursor.
type job struct {
	kind kind
	meta freeagent.ResourceMeta
	// path overrides the family's own path, for the year-addressed families.
	path string
	// tolerate404 marks a job whose absence is a normal answer, such as a tax
	// year the company had no payroll in.
	tolerate404 bool

	// scope is empty for a plain family and the bank account URL otherwise.
	// It is the scope column in sync_state.
	scope string
	// label is what the progress display and the result table show.
	label string
	// extra is merged into the query and is what makes the scope real.
	extra url.Values
}

func (j job) key() string {
	if j.scope == "" {
		return j.meta.Name
	}
	return j.meta.Name + " [" + j.label + "]"
}

// requestPath is where this job reads from: its own override, or the family's.
func (j job) requestPath() string {
	if j.path != "" {
		return j.path
	}
	return j.meta.Path
}

// plan expands the selected families into jobs, fetching the bank accounts
// first when a bank-scoped family needs them.
func (e *Engine) plan(
	ctx context.Context, families []freeagent.ResourceMeta, now time.Time,
) ([]job, error) {
	scopes, err := e.scopesFor(ctx, families)
	if err != nil {
		return nil, err
	}

	var jobs []job
	for _, meta := range families {
		switch Classify(meta) {
		case ClassYearScoped:
			jobs = append(jobs, e.payrollJobs(ctx, meta, now)...)

		case ClassSingleton:
			jobs = append(jobs, job{kind: kindDocument, meta: meta, label: meta.Name})

		case ClassReport:
			jobs = append(jobs, job{kind: kindReport, meta: meta, label: meta.Name})

		case ClassUserScoped:
			narrowed, err := e.userScopes(ctx, meta)
			if err != nil {
				return nil, err
			}
			for _, one := range narrowed {
				jobs = append(jobs, job{
					kind:  kindCollection,
					meta:  meta,
					scope: one.url,
					label: one.label,
					path:  one.path,
				})
			}

		case ClassBankScoped, ClassParentScoped:
			for _, narrowed := range scopes[Classify(meta)] {
				jobs = append(jobs, job{
					kind:  kindCollection,
					meta:  meta,
					scope: narrowed.url,
					label: narrowed.label,
					extra: narrowed.query,
					path:  narrowed.path,
				})
			}

		default:
			jobs = append(jobs, job{kind: kindCollection, meta: meta, label: meta.Name})
		}
	}
	return jobs, nil
}

// scopesFor resolves the scopes the selected families need, once each: a
// bank-scoped and a parent-scoped family in the same run should not each pay
// for their own lookup.
func (e *Engine) scopesFor(
	ctx context.Context, families []freeagent.ResourceMeta,
) (map[Class][]scope, error) {
	needed := map[Class]bool{}
	for _, meta := range families {
		switch class := Classify(meta); class {
		case ClassBankScoped, ClassParentScoped, ClassUserScoped:
			needed[class] = true
		}
	}

	out := map[Class][]scope{}
	for class := range needed {
		var (
			resolved []scope
			err      error
		)
		switch class {
		case ClassBankScoped:
			resolved, err = e.bankScopes(ctx)
		case ClassParentScoped:
			resolved, err = e.parentScopes(ctx)
		case ClassUserScoped:
			// User-scoped paths differ per family, so they are built per
			// family rather than shared.
			continue
		}
		if err != nil {
			return nil, err
		}
		out[class] = resolved
	}
	return out, nil
}

// bankScopes are the bank accounts the bank-scoped families fan out over.
func (e *Engine) bankScopes(ctx context.Context) ([]scope, error) {
	accounts, err := e.bankAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]scope, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, scope{
			url:   account.URL,
			label: account.Label(),
			query: url.Values{"bank_account": {account.URL}},
		})
	}
	return out, nil
}

// bankAccount is the little of a bank account this needs: its URL, to filter
// by, and a name to put in the progress display.
type bankAccount struct {
	URL  string
	Name string
	Type string
}

// Label is a short human name for the account.
func (a bankAccount) Label() string {
	if a.Name != "" {
		return a.Name
	}
	return store.IDFromURL(a.URL)
}

// bankAccounts reads the scopes from the archive, falling back to one live
// request when they are not there yet. The pull order puts bank_accounts
// first, so the fallback only fires for a run that named a bank-scoped family
// on its own.
func (e *Engine) bankAccounts(ctx context.Context) ([]bankAccount, error) {
	accounts, err := e.archivedBankAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if len(accounts) > 0 {
		return accounts, nil
	}

	meta, ok := freeagent.Resources[bankAccountsFamily]
	if !ok {
		return nil, fmt.Errorf("engine: the SDK has no %s entry", bankAccountsFamily)
	}
	e.report.Logf("reading bank accounts: none archived yet")

	for page, err := range e.client.Pages(ctx, meta, nil) {
		if err != nil {
			return nil, fmt.Errorf("engine: listing bank accounts: %w", err)
		}
		records := make([]store.Record, 0, len(page.Records))
		for _, raw := range page.Records {
			rec, err := store.NewRecord(bankAccountsFamily, raw)
			if err != nil {
				return nil, err
			}
			records = append(records, rec)
		}
		if _, err := e.db.UpsertRecords(ctx, e.account.ID, records); err != nil {
			return nil, err
		}
	}
	return e.archivedBankAccounts(ctx)
}

func (e *Engine) archivedBankAccounts(ctx context.Context) ([]bankAccount, error) {
	bodies, err := e.db.LiveRecordBodies(ctx, e.account.ID, bankAccountsFamily)
	if err != nil {
		return nil, err
	}

	accounts := make([]bankAccount, 0, len(bodies))
	for _, body := range bodies {
		var parsed struct {
			URL  string `json:"url"`
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("engine: reading an archived bank account: %w", err)
		}
		if parsed.URL == "" {
			continue
		}
		accounts = append(accounts, bankAccount{
			URL: parsed.URL, Name: parsed.Name, Type: parsed.Type,
		})
	}
	return accounts, nil
}
