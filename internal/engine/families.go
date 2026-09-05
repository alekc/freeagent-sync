// Package engine drives the archive: which families to read, in what order,
// how far back, and what to do with what comes off the wire.
package engine

import (
	"slices"
	"strings"

	"github.com/alekc/freeagent"

	"github.com/alekc/freeagent-sync/internal/family"
)

// Archivable reports whether this build can archive a family. The remaining
// classes need their own strategies and are not skipped silently: the engine
// reports them as unsupported so the gap is visible in the run output.
func Archivable(meta freeagent.ResourceMeta) bool {
	switch family.Classify(meta) {
	case family.ClassCollection, family.ClassGrouped, family.ClassBankScoped,
		family.ClassParentScoped, family.ClassUserScoped:
		return meta.Plural != "" || meta.Grouped
	case family.ClassSingleton, family.ClassReport, family.ClassYearScoped:
		// A document endpoint needs no envelope key: the whole response is
		// archived as one body.
		return true
	default:
		return false
	}
}

// pullOrder is the dependency order: parents before the records that
// reference them. The archive does not need it, since bodies are stored
// verbatim, but the projections do and the write path will depend on it
// absolutely, so the ordering is established here once.
//
// A family missing from this list still syncs; it just goes last.
var pullOrder = []string{
	"company",
	"users",
	"categories",
	"bank_accounts",
	"contacts",
	"projects",
	"tasks",
	"price_list_items",
	"stock_items",
	"capital_asset_types",
	"capital_assets",
	"hire_purchases",
	"properties",
	"invoices",
	"recurring_invoices",
	"estimates",
	"credit_notes",
	"credit_note_reconciliations",
	"bills",
	"expenses",
	"timeslips",
	"journal_sets",
	"transactions",
	"bank_transactions",
	"bank_transaction_explanations",
	"bank_feeds",
	"notes",
	"email_addresses",
	"cis_bands",
	"sales_tax_periods",
	"final_accounts_reports",
	"vat_returns",
	"corporation_tax_returns",
	"income_tax_returns",
	"payroll",
	"payroll_profiles",
}

// Archivable families, in pull order. Anything the SDK knows about that this
// list does not mention is appended alphabetically rather than dropped.
func archivableFamilies() []freeagent.ResourceMeta {
	var out []freeagent.ResourceMeta
	for _, meta := range freeagent.Resources {
		if Archivable(meta) {
			out = append(out, meta)
		}
	}
	slices.SortFunc(out, byPullOrder)
	return out
}

func byPullOrder(a, b freeagent.ResourceMeta) int {
	ai, bi := slices.Index(pullOrder, a.Name), slices.Index(pullOrder, b.Name)
	switch {
	case ai < 0 && bi < 0:
		return strings.Compare(a.Name, b.Name)
	case ai < 0:
		return 1
	case bi < 0:
		return -1
	default:
		return ai - bi
	}
}

// SelectFamilies resolves the caller's --family list against what this build
// can archive. An unknown or unsupported name is an error naming the reason,
// never a silent omission from the run.
func SelectFamilies(names []string) ([]freeagent.ResourceMeta, error) {
	if len(names) == 0 {
		return archivableFamilies(), nil
	}

	var out []freeagent.ResourceMeta
	for _, name := range names {
		meta, ok := freeagent.Resources[name]
		if !ok {
			return nil, &UnknownFamilyError{Name: name}
		}
		if !Archivable(meta) {
			return nil, &UnsupportedFamilyError{Name: name, Class: family.Classify(meta)}
		}
		out = append(out, meta)
	}
	slices.SortFunc(out, byPullOrder)
	return out, nil
}

// Probeable reports whether asking about updated_since means anything for a
// family. A singleton has nothing to filter, a report is recomputed on every
// request, and a year-addressed endpoint is not a collection at all. Probing
// them produced failures that said nothing about the API.
func Probeable(meta freeagent.ResourceMeta) bool {
	switch family.Classify(meta) {
	case family.ClassCollection, family.ClassGrouped, family.ClassBankScoped,
		family.ClassParentScoped, family.ClassUserScoped:
		return Archivable(meta)
	default:
		return false
	}
}

// Deferred lists the families this build cannot archive yet, with the reason,
// so a run can report the gap rather than leaving the user to notice it.
func Deferred() map[string]family.Class {
	out := make(map[string]family.Class)
	for name, meta := range freeagent.Resources {
		if !Archivable(meta) {
			out[name] = family.Classify(meta)
		}
	}
	return out
}

// UnknownFamilyError names a family the SDK has no entry for.
type UnknownFamilyError struct{ Name string }

func (e *UnknownFamilyError) Error() string {
	return "engine: unknown family " + e.Name +
		"; run `fasync families` to list what is available"
}

// UnsupportedFamilyError names a family that exists but needs a strategy this
// build does not have yet.
type UnsupportedFamilyError struct {
	Name  string
	Class family.Class
}

func (e *UnsupportedFamilyError) Error() string {
	return "engine: " + e.Name + " is a " + e.Class.String() +
		" family, which this build cannot archive yet"
}
