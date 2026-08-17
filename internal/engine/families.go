// Package engine drives the archive: which families to read, in what order,
// how far back, and what to do with what comes off the wire.
package engine

import (
	"slices"
	"strings"

	"github.com/alekc/freeagent"
)

// Class is how a family has to be read. It is derived from the SDK's own
// registry rather than from a list maintained here, so a family added
// upstream is classified automatically instead of being silently dropped.
type Class int

const (
	// ClassCollection is a plain paged collection: the common case.
	ClassCollection Class = iota
	// ClassGrouped splits its records across several envelope keys.
	ClassGrouped
	// ClassBankScoped rejects a request without a bank_account filter.
	ClassBankScoped
	// ClassSingleton has no id segment and returns one document.
	ClassSingleton
	// ClassReport is a derived, point-in-time answer rather than a record.
	ClassReport
	// ClassYearScoped is addressed by tax year, with no endpoint listing which
	// years exist.
	ClassYearScoped
	// ClassParentScoped rejects a request without a contact or project.
	ClassParentScoped
	// ClassUserScoped is nested under a user, so its own path does not exist.
	ClassUserScoped
	// ClassCustomEnvelope answers with a shape of its own.
	ClassCustomEnvelope
	// ClassChildOnly is reached through a parent record, never enumerated.
	ClassChildOnly
)

// String names a class for logs and status output.
func (c Class) String() string {
	switch c {
	case ClassCollection:
		return "collection"
	case ClassGrouped:
		return "grouped"
	case ClassBankScoped:
		return "bank-scoped"
	case ClassSingleton:
		return "singleton"
	case ClassReport:
		return "report"
	case ClassYearScoped:
		return "year-scoped"
	case ClassParentScoped:
		return "parent-scoped"
	case ClassUserScoped:
		return "user-scoped"
	case ClassCustomEnvelope:
		return "custom-envelope"
	case ClassChildOnly:
		return "child-only"
	}
	return "unknown"
}

// reportFamilies are the derived reports. The registry marks them as
// read-only singletons, which does not distinguish them from company, so the
// list is explicit: a report is snapshotted with the window it was taken for,
// not upserted as though it were a record.
var reportFamilies = map[string]bool{
	"trial_balance":   true,
	"profit_and_loss": true,
	"balance_sheet":   true,
	"cashflow":        true,
}

// parentScopedFamilies reject a request without a contact or project. Notes is
// the only one, and the API answers 400 without the parameter rather than
// returning everything.
var parentScopedFamilies = map[string]bool{
	"notes": true,
}

// userScopedFamilies are nested under a user. Their registry Path is the
// suffix, not a usable path: /v2/self_assessment_returns does not exist, only
// /v2/users/:id/self_assessment_returns does.
var userScopedFamilies = map[string]bool{
	"income_tax_returns": true,
}

// yearScopedFamilies are addressed by tax year. Neither has an endpoint that
// lists the years a company actually has data for, so the range is derived and
// each year is attempted.
var yearScopedFamilies = map[string]bool{
	"payroll":          true,
	"payroll_profiles": true,
}

// Classify decides how a family is read. Order matters: the flags are not
// mutually exclusive, and the most restrictive one wins.
func Classify(meta freeagent.ResourceMeta) Class {
	switch {
	case meta.NoList:
		return ClassChildOnly
	case yearScopedFamilies[meta.Name]:
		return ClassYearScoped
	case parentScopedFamilies[meta.Name]:
		return ClassParentScoped
	case userScopedFamilies[meta.Name]:
		return ClassUserScoped
	case reportFamilies[meta.Name]:
		return ClassReport
	case meta.Singleton:
		return ClassSingleton
	case meta.CustomEnvelope:
		return ClassCustomEnvelope
	case meta.RequiresBankAccount:
		return ClassBankScoped
	case meta.Grouped:
		return ClassGrouped
	default:
		return ClassCollection
	}
}

// Archivable reports whether this build can archive a family. The remaining
// classes need their own strategies and are not skipped silently: the engine
// reports them as unsupported so the gap is visible in the run output.
func Archivable(meta freeagent.ResourceMeta) bool {
	switch Classify(meta) {
	case ClassCollection, ClassGrouped, ClassBankScoped,
		ClassParentScoped, ClassUserScoped:
		return meta.Plural != "" || meta.Grouped
	case ClassSingleton, ClassReport, ClassYearScoped:
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
			return nil, &UnsupportedFamilyError{Name: name, Class: Classify(meta)}
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
	switch Classify(meta) {
	case ClassCollection, ClassGrouped, ClassBankScoped,
		ClassParentScoped, ClassUserScoped:
		return Archivable(meta)
	default:
		return false
	}
}

// Deferred lists the families this build cannot archive yet, with the reason,
// so a run can report the gap rather than leaving the user to notice it.
func Deferred() map[string]Class {
	out := make(map[string]Class)
	for name, meta := range freeagent.Resources {
		if !Archivable(meta) {
			out[name] = Classify(meta)
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
	Class Class
}

func (e *UnsupportedFamilyError) Error() string {
	return "engine: " + e.Name + " is a " + e.Class.String() +
		" family, which this build cannot archive yet"
}
