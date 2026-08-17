package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/alekc/freeagent"
	"github.com/shopspring/decimal"

	"github.com/alekc/freeagent-sync/internal/blob"
	"github.com/alekc/freeagent-sync/internal/store"
)

// CheckStatus is how one check came out.
type CheckStatus string

const (
	// CheckPass means the check ran and found nothing wrong.
	CheckPass CheckStatus = "pass"
	// CheckFail means the check found something that should not be true.
	CheckFail CheckStatus = "fail"
	// CheckAdvisory means the check found a difference that may be legitimate.
	// Reported so a human can judge, not treated as a failure.
	CheckAdvisory CheckStatus = "advisory"
	// CheckSkipped means the check could not run: usually missing data.
	CheckSkipped CheckStatus = "skipped"
)

// Check is one verification result.
type Check struct {
	Name    string
	Status  CheckStatus
	Summary string
	Detail  []string
}

// VerifyResult is the whole verification.
type VerifyResult struct {
	Checks []Check
}

// Failed reports whether any check found something that should not be true.
// Advisory findings do not fail: they are differences a human has to judge.
func (r VerifyResult) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			return true
		}
	}
	return false
}

// VerifyOptions configures a verification.
type VerifyOptions struct {
	// Blobs enables re-hashing every stored attachment.
	Blobs *blob.Store
	// FromDate and ToDate bound the reconciliation against the trial balance.
	// Empty means use the newest snapshot's own window.
	FromDate string
	ToDate   string
	// MaxDetail caps how many lines a check reports, so one broken family
	// cannot bury the others.
	MaxDetail int
}

// defaultMaxDetail keeps output readable when something is badly wrong.
const defaultMaxDetail = 20

// Verify checks the archive against itself and against FreeAgent's own
// arithmetic.
//
// The checks are ordered by how much they prove. Internal consistency and
// dangling references are local and unambiguous. The reconciliation against
// the trial balance is advisory, because a trial balance total is cumulative
// from the start of an accounting period and a difference on a balance-sheet
// code can be an opening balance rather than a missing record.
func (e *Engine) Verify(ctx context.Context, opts VerifyOptions) (VerifyResult, error) {
	if opts.MaxDetail <= 0 {
		opts.MaxDetail = defaultMaxDetail
	}
	var result VerifyResult

	integrity, err := e.checkReferences(ctx, opts)
	if err != nil {
		return result, err
	}
	result.Checks = append(result.Checks, integrity)

	balance, entries := e.checkTrialBalanceSumsToZero(ctx)
	result.Checks = append(result.Checks, balance)

	result.Checks = append(result.Checks, e.checkNominalCoverage(ctx, entries, opts))
	result.Checks = append(result.Checks, e.checkTotalsAgainstTrialBalance(ctx, entries, opts))

	if opts.Blobs != nil {
		blobs, err := e.checkBlobs(ctx, opts)
		if err != nil {
			return result, err
		}
		result.Checks = append(result.Checks, blobs)
	}
	return result, nil
}

// checkReferences looks for cross-references pointing at records the archive
// does not hold.
//
// This is the strongest check available, and it needs no network: every
// reference in a FreeAgent payload is a URL, so a reference to a family this
// tool archives that is not in the archive means a gap. References into a
// family this build does not archive are counted separately, because those are
// expected rather than wrong.
func (e *Engine) checkReferences(ctx context.Context, opts VerifyOptions) (Check, error) {
	check := Check{Name: "cross-references resolve"}

	archived, err := e.db.ArchivedURLs(ctx, e.account.ID)
	if err != nil {
		return check, err
	}
	// Attachments are archived, just not as records: they live in their own
	// table with their bytes in the blob store. Counting a reference to one as
	// unarchived would understate what the mirror actually holds.
	attachments, err := e.db.StoredAttachments(ctx, e.account.ID)
	if err != nil {
		return check, err
	}
	for _, att := range attachments {
		archived[att.URL] = "attachments"
	}

	base := archiveBase(archived)
	if base == "" {
		check.Status = CheckSkipped
		check.Summary = "nothing archived yet; run: fasync pull"
		return check, nil
	}

	dangling := map[string]int{}
	unarchived := map[string]int{}
	err = e.db.EachRecord(ctx, e.account.ID, func(rec store.RecordRow) error {
		if rec.Deleted {
			return nil
		}
		for ref := range referencesIn(rec.Body, base) {
			if _, ok := archived[ref]; ok {
				continue
			}
			family := familyOfURL(ref, base)
			meta, known := freeagent.Resources[family]
			if known && Archivable(meta) {
				dangling[family+" <- "+rec.Family]++
				continue
			}
			unarchived[family]++
		}
		return ctx.Err()
	})
	if err != nil {
		return check, err
	}

	if len(dangling) == 0 {
		check.Status = CheckPass
		check.Summary = fmt.Sprintf(
			"every reference into an archived family resolves; %d point at families this build does not archive",
			total(unarchived))
		check.Detail = topCounts(unarchived, opts.MaxDetail)
		return check, nil
	}

	check.Status = CheckFail
	check.Summary = fmt.Sprintf(
		"%d references point at records that should be archived but are not",
		total(dangling))
	check.Detail = topCounts(dangling, opts.MaxDetail)
	return check, nil
}

// checkTrialBalanceSumsToZero is double-entry's own invariant. It proves the
// snapshot decoded correctly and that FreeAgent's own books balance.
func (e *Engine) checkTrialBalanceSumsToZero(
	ctx context.Context,
) (Check, []trialBalanceEntry) {
	check := Check{Name: "trial balance sums to zero"}

	entries, snap, problem := e.loadTrialBalance(ctx)
	if problem != nil {
		check.Status, check.Summary = problem.Status, problem.Summary
		return check, nil
	}

	sum := decimal.Zero
	for _, entry := range entries {
		sum = sum.Add(entry.Total)
	}
	if sum.IsZero() {
		check.Status = CheckPass
		check.Summary = fmt.Sprintf("%d entries, taken %s, sum exactly zero",
			len(entries), snap.TakenAt.Format("2006-01-02 15:04"))
		return check, entries
	}

	check.Status = CheckFail
	check.Summary = fmt.Sprintf("%d entries sum to %s, not zero", len(entries), sum)
	return check, entries
}

// loadTrialBalance reads the newest snapshot, returning a Check instead of an
// error when it cannot: a check that cannot run is a reported outcome, not a
// reason to abandon the rest of the verification.
func (e *Engine) loadTrialBalance(
	ctx context.Context,
) ([]trialBalanceEntry, *store.ReportSnapshot, *Check) {
	snap, err := e.db.LatestReportSnapshot(ctx, e.account.ID, "trial_balance")
	if err != nil {
		return nil, nil, &Check{
			Status:  CheckSkipped,
			Summary: "no trial balance snapshot; run: fasync pull --family trial_balance",
		}
	}

	entries, decodeErr := decodeTrialBalance(snap.Body)
	if decodeErr != nil {
		return nil, nil, &Check{Status: CheckFail, Summary: decodeErr.Error()}
	}
	if len(entries) == 0 {
		return nil, nil, &Check{
			Status: CheckSkipped, Summary: "the trial balance snapshot is empty",
		}
	}
	return entries, snap, nil
}

// checkNominalCoverage looks for a nominal code the books use that the
// archived transactions never mention, which means a whole category of entries
// never arrived.
func (e *Engine) checkNominalCoverage(
	ctx context.Context, entries []trialBalanceEntry, opts VerifyOptions,
) Check {
	check := Check{Name: "nominal codes covered"}
	if len(entries) == 0 {
		check.Status = CheckSkipped
		check.Summary = "no trial balance to compare against"
		return check
	}

	local, err := e.localTotals(ctx, opts)
	if err != nil {
		check.Status = CheckSkipped
		check.Summary = err.Error()
		return check
	}
	if len(local) == 0 {
		check.Status = CheckSkipped
		check.Summary = "no transactions archived; run: fasync pull --family transactions"
		return check
	}

	var missing []string
	for _, entry := range entries {
		if entry.NominalCode == "" || entry.Total.IsZero() {
			continue
		}
		if _, ok := local[entry.NominalCode]; !ok {
			missing = append(missing,
				fmt.Sprintf("%s %s (trial balance %s)",
					entry.NominalCode, entry.Name, entry.Total))
		}
	}

	if len(missing) == 0 {
		check.Status = CheckPass
		check.Summary = fmt.Sprintf(
			"all %d nominal codes with a balance appear in the archived transactions",
			len(entries))
		return check
	}

	check.Status = CheckFail
	check.Summary = fmt.Sprintf("%d nominal codes have a balance but no archived transactions",
		len(missing))
	check.Detail = capLines(missing, opts.MaxDetail)
	return check
}

// checkTotalsAgainstTrialBalance compares the sum of archived transactions per
// nominal code against FreeAgent's own figure.
//
// Advisory by design. A trial balance total runs from the start of an
// accounting period, so a balance-sheet code can legitimately differ by its
// opening balance. A difference is worth a human's attention, not an
// automatic failure.
func (e *Engine) checkTotalsAgainstTrialBalance(
	ctx context.Context, entries []trialBalanceEntry, opts VerifyOptions,
) Check {
	check := Check{Name: "totals match the trial balance"}
	if len(entries) == 0 {
		check.Status = CheckSkipped
		check.Summary = "no trial balance to compare against"
		return check
	}

	local, err := e.localTotals(ctx, opts)
	if err != nil {
		check.Status = CheckSkipped
		check.Summary = err.Error()
		return check
	}
	if len(local) == 0 {
		check.Status = CheckSkipped
		check.Summary = "no transactions archived to compare"
		return check
	}

	var differences []string
	var matched int
	for _, entry := range entries {
		if entry.NominalCode == "" {
			continue
		}
		mine, ok := local[entry.NominalCode]
		if !ok {
			continue
		}
		if mine.Equal(entry.Total) {
			matched++
			continue
		}
		differences = append(differences, fmt.Sprintf(
			"%s %s: archive %s, trial balance %s, difference %s",
			entry.NominalCode, entry.Name, mine, entry.Total, mine.Sub(entry.Total)))
	}

	if len(differences) == 0 {
		check.Status = CheckPass
		check.Summary = fmt.Sprintf("%d nominal codes agree exactly", matched)
		return check
	}

	check.Status = CheckAdvisory
	check.Summary = fmt.Sprintf(
		"%d codes agree, %d differ; a trial balance runs from the accounting period start, "+
			"so an opening balance can explain a difference on a balance-sheet code",
		matched, len(differences))
	check.Detail = capLines(differences, opts.MaxDetail)
	return check
}

// checkBlobs re-hashes every stored attachment. Re-hashing rather than
// comparing against something stored alongside it, which would only prove two
// copies of a possibly-wrong value agree.
func (e *Engine) checkBlobs(ctx context.Context, opts VerifyOptions) (Check, error) {
	check := Check{Name: "attachment bytes intact"}

	stored, err := e.db.StoredAttachments(ctx, e.account.ID)
	if err != nil {
		return check, err
	}
	if len(stored) == 0 {
		check.Status = CheckSkipped
		check.Summary = "no attachments stored"
		return check, nil
	}

	seen := map[string]bool{}
	var checked int
	var bad []string
	for _, att := range stored {
		if seen[att.SHA256] {
			continue
		}
		seen[att.SHA256] = true
		checked++
		if err := opts.Blobs.Verify(att.SHA256); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", att.FileName, err))
		}
	}

	if len(bad) == 0 {
		check.Status = CheckPass
		check.Summary = fmt.Sprintf("%d distinct blobs re-hashed and matched", checked)
		return check, nil
	}
	check.Status = CheckFail
	check.Summary = fmt.Sprintf("%d of %d blobs failed verification", len(bad), checked)
	check.Detail = capLines(bad, opts.MaxDetail)
	return check, nil
}

// localTotals sums archived transactions per nominal code, over the same
// window the trial balance snapshot covers.
func (e *Engine) localTotals(
	ctx context.Context, opts VerifyOptions,
) (map[string]decimal.Decimal, error) {
	from, to := opts.FromDate, opts.ToDate
	if from == "" && to == "" {
		if snap, err := e.db.LatestReportSnapshot(
			ctx, e.account.ID, "trial_balance"); err == nil {
			from, to = snap.FromDate, snap.ToDate
		}
	}

	bodies, err := e.db.LiveRecordBodiesInWindow(
		ctx, e.account.ID, "transactions", from, to)
	if err != nil {
		return nil, err
	}

	totals := map[string]decimal.Decimal{}
	for _, body := range bodies {
		var txn struct {
			NominalCode string `json:"nominal_code"`
			DebitValue  string `json:"debit_value"`
		}
		if json.Unmarshal(body, &txn) != nil || txn.NominalCode == "" {
			continue
		}
		value, err := decimal.NewFromString(txn.DebitValue)
		if err != nil {
			continue
		}
		totals[txn.NominalCode] = totals[txn.NominalCode].Add(value)
	}
	return totals, nil
}

// trialBalanceEntry is the part of a trial balance line this needs.
type trialBalanceEntry struct {
	NominalCode string
	Name        string
	Total       decimal.Decimal
}

func decodeTrialBalance(body []byte) ([]trialBalanceEntry, error) {
	var envelope struct {
		Entries []struct {
			NominalCode string `json:"nominal_code"`
			Name        string `json:"name"`
			Total       any    `json:"total"`
		} `json:"trial_balance_summary"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("the trial balance snapshot does not decode: %w", err)
	}

	out := make([]trialBalanceEntry, 0, len(envelope.Entries))
	for _, raw := range envelope.Entries {
		// FreeAgent sends money as a quoted string in most places and as a
		// bare number in some reports, so both are accepted here.
		total, err := decimalFrom(raw.Total)
		if err != nil {
			return nil, fmt.Errorf("nominal code %s has an unreadable total: %w",
				raw.NominalCode, err)
		}
		out = append(out, trialBalanceEntry{
			NominalCode: raw.NominalCode, Name: raw.Name, Total: total,
		})
	}
	return out, nil
}

func decimalFrom(value any) (decimal.Decimal, error) {
	switch typed := value.(type) {
	case nil:
		return decimal.Zero, nil
	case string:
		if typed == "" {
			return decimal.Zero, nil
		}
		return decimal.NewFromString(typed)
	case json.Number:
		return decimal.NewFromString(typed.String())
	case float64:
		// Reached only when a report sends a bare number. Formatted back
		// through the shortest exact representation rather than scaled, so no
		// precision is invented.
		return decimal.NewFromString(fmt.Sprintf("%v", typed))
	}
	return decimal.Zero, fmt.Errorf("%v is not a number", value)
}

// referencesIn yields every FreeAgent resource URL in a payload.
func referencesIn(body []byte, base string) map[string]struct{} {
	out := map[string]struct{}{}
	if base == "" {
		return out
	}

	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		return out
	}
	walkStrings(parsed, func(s string) {
		if strings.HasPrefix(s, base+"/") {
			out[s] = struct{}{}
		}
	})
	return out
}

func walkStrings(node any, visit func(string)) {
	switch typed := node.(type) {
	case string:
		visit(typed)
	case map[string]any:
		for key, child := range typed {
			// A record's own url is not a reference to anything else.
			if key == "url" {
				continue
			}
			walkStrings(child, visit)
		}
	case []any:
		for _, child := range typed {
			walkStrings(child, visit)
		}
	}
}

// archiveBase derives the API host from the archive itself, rather than from a
// client this check does not otherwise need. Every record in an account came
// from one host, and a reference is only resolvable if it is on that host, so
// the archived URLs are the authoritative source for what counts as one.
func archiveBase(archived map[string]string) string {
	for raw := range archived {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		return parsed.Scheme + "://" + parsed.Host
	}
	return ""
}

// familyOfURL reads the family out of a resource URL: the segment after the
// version prefix.
func familyOfURL(url, base string) string {
	rest := strings.TrimPrefix(strings.TrimPrefix(url, base), "/")
	rest = strings.TrimPrefix(rest, "v2/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func total(counts map[string]int) int {
	var sum int
	for _, n := range counts {
		sum += n
	}
	return sum
}

// topCounts renders a count map as lines, largest first.
func topCounts(counts map[string]int, limit int) []string {
	keys := slices.Sorted(maps.Keys(counts))
	slices.SortStableFunc(keys, func(a, b string) int { return counts[b] - counts[a] })

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, fmt.Sprintf("%-40s %d", key, counts[key]))
	}
	return capLines(out, limit)
}

func capLines(lines []string, limit int) []string {
	if len(lines) <= limit {
		return lines
	}
	out := append([]string(nil), lines[:limit]...)
	return append(out, fmt.Sprintf("... and %d more", len(lines)-limit))
}
