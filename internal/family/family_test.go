package family

import (
	"testing"

	"github.com/alekc/freeagent"
)

// The classes have to match what the API actually requires, since every one of
// these was a live failure.
func TestClassificationMatchesTheAPI(t *testing.T) {
	t.Parallel()
	tests := map[string]Class{
		"notes":              ClassParentScoped,
		"income_tax_returns": ClassUserScoped,
		"bank_transactions":  ClassBankScoped,
		"payroll":            ClassYearScoped,
		"company":            ClassSingleton,
		"trial_balance":      ClassReport,
		"invoices":           ClassCollection,
		"categories":         ClassGrouped,
		"attachments":        ClassChildOnly,
	}
	for name, want := range tests {
		meta, ok := freeagent.Resources[name]
		if !ok {
			t.Errorf("the SDK has no %s entry", name)
			continue
		}
		if got := Classify(meta); got != want {
			t.Errorf("Classify(%s) = %s, want %s", name, got, want)
		}
	}
}

// A class with no name would reach logs and tool descriptions as a bare
// integer, so every constant is checked rather than only the ones a test
// happens to construct.
func TestEveryClassIsNamed(t *testing.T) {
	t.Parallel()
	for c := ClassCollection; c <= ClassChildOnly; c++ {
		if got := c.String(); got == "unknown" || got == "" {
			t.Errorf("class %d has no name", int(c))
		}
	}
	if got := Class(-1).String(); got != "unknown" {
		t.Errorf("out-of-range class = %q, want unknown", got)
	}
}
