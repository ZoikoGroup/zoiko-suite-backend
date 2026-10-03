package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// Side identifies which side of a comparison a population is.
type Side string

const (
	SideA Side = "A" // source / authoritative subledger side
	SideB Side = "B" // target / downstream side (e.g. GL)
)

// PopulationRecord is one normalised record of a frozen population. The
// contract every source service implements yields exactly these fields; the
// amount is a decimal STRING (never float64 — ZS-DATA-001 D06).
type PopulationRecord struct {
	RecordID   string            `json:"record_id"`
	Reference  string            `json:"reference"`
	Amount     string            `json:"amount"`
	Currency   string            `json:"currency"`
	Date       string            `json:"date"` // YYYY-MM-DD
	Attributes map[string]string `json:"attributes,omitempty"`
}

// ParsedAmount returns the exact decimal value of the record's amount.
func (r PopulationRecord) ParsedAmount() (*big.Rat, error) {
	v, ok := new(big.Rat).SetString(strings.TrimSpace(r.Amount))
	if !ok {
		return nil, fmt.Errorf("%w: record %q has non-decimal amount %q", ErrInvalidArgument, r.RecordID, r.Amount)
	}
	return v, nil
}

// ValidateRecord rejects a record the engine cannot compare exactly. A bad
// record fails the whole population (fail closed): it is never skipped, since
// silently dropping records is exactly how a completeness control lies.
func ValidateRecord(r PopulationRecord) error {
	if strings.TrimSpace(r.RecordID) == "" {
		return fmt.Errorf("%w: record without record_id", ErrInvalidArgument)
	}
	if strings.TrimSpace(r.Reference) == "" {
		return fmt.Errorf("%w: record %q has no reference", ErrInvalidArgument, r.RecordID)
	}
	if !currencyRe.MatchString(r.Currency) {
		return fmt.Errorf("%w: record %q has invalid currency %q", ErrInvalidArgument, r.RecordID, r.Currency)
	}
	if _, err := time.Parse("2006-01-02", r.Date); err != nil {
		return fmt.Errorf("%w: record %q has invalid date %q", ErrInvalidArgument, r.RecordID, r.Date)
	}
	if _, err := r.ParsedAmount(); err != nil {
		return err
	}
	return nil
}

// CanonicalAmount renders a decimal with no trailing zeros ("10.50" and "10.5"
// and "10.500000" all hash identically).
func CanonicalAmount(r *big.Rat) string {
	s := r.FloatString(12)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	if s == "" || s == "-" {
		return "0"
	}
	if s == "-0" {
		return "0"
	}
	return s
}

// CurrencyTotal is the control total for one currency bucket.
type CurrencyTotal struct {
	Currency string `json:"currency"`
	Count    int    `json:"count"`
	Total    string `json:"total"`
}

// PopulationSnapshot is the frozen, reproducible record of what was tested
// (§9): the exact identity set, control totals and an integrity fingerprint.
type PopulationSnapshot struct {
	PopulationID   string          `json:"population_id"`
	TenantID       string          `json:"tenant_id"`
	RunID          string          `json:"run_id"`
	Side           Side            `json:"side"`
	SourceSystem   string          `json:"source_system"`
	SpecRef        string          `json:"spec_ref"`
	RowCount       int             `json:"row_count"`
	Totals         []CurrencyTotal `json:"control_totals"`
	PopulationHash string          `json:"population_hash"`
	Watermark      string          `json:"source_watermark"`
	FrozenAt       time.Time       `json:"frozen_at"`
	Exclusions     []Exclusion     `json:"exclusions"`
}

// Exclusion is an explicit, documented exclusion (§9). Exclusions are never
// silent: reason, authority and count/value impact are recorded on the snapshot.
type Exclusion struct {
	Reason    string `json:"reason"`
	Authority string `json:"authority"`
	Count     int    `json:"count"`
	Value     string `json:"value"`
	Currency  string `json:"currency"`
	Digest    string `json:"excluded_ids_digest"`
}

// ComputeTotals returns per-currency counts and exact sums, sorted by currency.
func ComputeTotals(records []PopulationRecord) ([]CurrencyTotal, error) {
	sums := map[string]*big.Rat{}
	counts := map[string]int{}
	for _, r := range records {
		amt, err := r.ParsedAmount()
		if err != nil {
			return nil, err
		}
		if sums[r.Currency] == nil {
			sums[r.Currency] = new(big.Rat)
		}
		sums[r.Currency].Add(sums[r.Currency], amt)
		counts[r.Currency]++
	}
	out := make([]CurrencyTotal, 0, len(sums))
	for c, s := range sums {
		out = append(out, CurrencyTotal{Currency: c, Count: counts[c], Total: CanonicalAmount(s)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out, nil
}

// HashPopulation returns a deterministic sha256 fingerprint of the population.
// Records are sorted by record_id first, so the hash is independent of the
// order the source paged them in, and every identity-bearing field is covered:
// changing any record, adding one or removing one changes the hash.
func HashPopulation(records []PopulationRecord) (string, error) {
	sorted := make([]PopulationRecord, len(records))
	copy(sorted, records)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RecordID < sorted[j].RecordID })
	h := sha256.New()
	for _, r := range sorted {
		amt, err := r.ParsedAmount()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s|%s|%s|%s|%s\n", r.RecordID, r.Reference, CanonicalAmount(amt), r.Currency, r.Date)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// DuplicateGroup is a set of records the engine will not let count twice.
type DuplicateGroup struct {
	Kind  string             // "RECORD_ID" (same id delivered twice) or "CONTENT" (distinct ids, identical content)
	Keep  PopulationRecord   // the first record (lowest record_id) stays in the matching population
	Dupes []PopulationRecord // the rest are excluded from matching and become DUPLICATE exceptions
}

// SplitDuplicates detects duplicate source/downstream records BEFORE ordinary
// matching (§10): a duplicate must never be silently netted against a single
// counterpart, and never allowed to make a population look complete.
//
//   - RECORD_ID: the same record_id appears more than once.
//   - CONTENT: different record_ids carry the same (reference, currency,
//     amount, date) — the classic double-posted invoice.
func SplitDuplicates(records []PopulationRecord) (clean []PopulationRecord, groups []DuplicateGroup, err error) {
	return SplitDuplicatesOpt(records, false)
}

// SplitDuplicatesOpt is SplitDuplicates with content-duplicate detection
// optional. Repeated record_ids are ALWAYS duplicates; identical-content
// detection can be switched off for group controls where equal legitimate
// postings exist (a real double posting then surfaces as a group difference).
func SplitDuplicatesOpt(records []PopulationRecord, skipContent bool) (clean []PopulationRecord, groups []DuplicateGroup, err error) {
	sorted := make([]PopulationRecord, len(records))
	copy(sorted, records)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].RecordID < sorted[j].RecordID })

	seenID := map[string]bool{}
	idGroups := map[string]*DuplicateGroup{}
	var uniqueByID []PopulationRecord
	for _, r := range sorted {
		if seenID[r.RecordID] {
			g := idGroups[r.RecordID]
			g.Dupes = append(g.Dupes, r)
			continue
		}
		seenID[r.RecordID] = true
		idGroups[r.RecordID] = &DuplicateGroup{Kind: "RECORD_ID", Keep: r}
		uniqueByID = append(uniqueByID, r)
	}
	var idKeys []string
	for id, g := range idGroups {
		if len(g.Dupes) > 0 {
			idKeys = append(idKeys, id)
		}
	}
	sort.Strings(idKeys)
	for _, id := range idKeys {
		groups = append(groups, *idGroups[id])
	}

	if skipContent {
		return uniqueByID, groups, nil
	}
	contentKey := func(r PopulationRecord) (string, error) {
		a, e := r.ParsedAmount()
		if e != nil {
			return "", e
		}
		return r.Reference + "|" + r.Currency + "|" + CanonicalAmount(a) + "|" + r.Date, nil
	}
	byContent := map[string]*DuplicateGroup{}
	var order []string
	for _, r := range uniqueByID {
		k, e := contentKey(r)
		if e != nil {
			return nil, nil, e
		}
		if g, ok := byContent[k]; ok {
			g.Dupes = append(g.Dupes, r)
			continue
		}
		byContent[k] = &DuplicateGroup{Kind: "CONTENT", Keep: r}
		order = append(order, k)
		clean = append(clean, r)
	}
	for _, k := range order {
		if g := byContent[k]; len(g.Dupes) > 0 {
			groups = append(groups, *g)
		}
	}
	return clean, groups, nil
}
