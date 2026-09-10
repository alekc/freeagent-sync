// Package family classifies FreeAgent resource families by how they have to be
// read. The classification is shared: the archive engine uses it to pick a pull
// strategy, and the MCP server uses it to decide which parameters a family's
// tool must require.
package family

import "github.com/alekc/freeagent"

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
