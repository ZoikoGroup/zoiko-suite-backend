package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Populations are drawn from their owning services through the control
// population contract (docs/architecture/control-population-contract.md,
// ZS-CONTROL-001 §9) — never pushed by the caller of StartRun. A definition
// names, per side, which registered source system and population to read; the
// run pins the watermark, declared totals and hash of exactly what was read.

var (
	populationNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	paramKeyRe       = regexp.MustCompile(`^[a-z_]{1,32}$`)
	currencyRe       = regexp.MustCompile(`^[A-Z]{3}$`)
	periodRe         = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)
	uuidRe           = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// reservedParams are set by the population client itself; a definition can
// never override them (that would let it retarget another entity or period).
var reservedParams = map[string]bool{"legal_entity_id": true, "period_id": true, "limit": true, "cursor": true}

const (
	maxParams, maxParamValue = 8, 512
	maxSideLen               = 64
	// MaxAmountScale bounds the fixed-point scale a definition may declare. At
	// scale 12 an int64 still holds about 9.2 million whole units; anything
	// larger is refused by the overflow checks rather than wrapped.
	MaxAmountScale = 12
)

// PopulationSpec names one side's population: the side label used in results,
// the registered source system and population, and optional params. Params of
// the form "${scope.<key>}" are bound from the run's scope at StartRun.
type PopulationSpec struct {
	Side       string            `json:"side"`
	System     string            `json:"system"`
	Population string            `json:"population"`
	Params     map[string]string `json:"params,omitempty"`
	// NoPeriod: the population is not period-scoped and its source rejects
	// period_id, so the run's period is not sent.
	NoPeriod bool `json:"no_period,omitempty"`
}

// Ref is the canonical query/spec identity of the population (the spec's
// "query/spec version" field of a snapshot): system/population?sorted params.
func (s PopulationSpec) Ref() string {
	ref := s.System + "/" + s.Population
	if len(s.Params) == 0 {
		return ref
	}
	keys := make([]string, 0, len(s.Params))
	for k := range s.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + s.Params[k]
	}
	return ref + "?" + strings.Join(parts, "&")
}

func (s PopulationSpec) Validate() error {
	if strings.TrimSpace(s.Side) == "" || len(s.Side) > maxSideLen {
		return fmt.Errorf("%w: population side is required (max %d chars)", ErrInvalidArgument, maxSideLen)
	}
	if strings.TrimSpace(s.System) == "" {
		return fmt.Errorf("%w: population %q needs a source system", ErrInvalidArgument, s.Side)
	}
	if !populationNameRe.MatchString(s.Population) {
		return fmt.Errorf("%w: population %q needs a population name (a-z, 0-9, _ or -)", ErrInvalidArgument, s.Side)
	}
	return ValidateParams(s.Params)
}

// ValidateParams checks population params against the contract's limits.
func ValidateParams(p map[string]string) error {
	if len(p) > maxParams {
		return fmt.Errorf("%w: at most %d population params", ErrInvalidArgument, maxParams)
	}
	for k, v := range p {
		if !paramKeyRe.MatchString(k) || reservedParams[k] {
			return fmt.Errorf("%w: invalid population param name %q", ErrInvalidArgument, k)
		}
		if len(v) > maxParamValue || strings.ContainsAny(v, "\x00\r\n") {
			return fmt.Errorf("%w: invalid value for population param %q", ErrInvalidArgument, k)
		}
	}
	return nil
}

// ValidatePopulationSpecs requires exactly two populations with distinct sides
// (B3: the matcher compares two sides; a third would be frozen but never
// reconciled).
func ValidatePopulationSpecs(specs []PopulationSpec) error {
	if len(specs) != 2 {
		return fmt.Errorf("%w: exactly two populations are required", ErrInvalidArgument)
	}
	if err := specs[0].Validate(); err != nil {
		return err
	}
	if err := specs[1].Validate(); err != nil {
		return err
	}
	if specs[0].Side == specs[1].Side {
		return fmt.Errorf("%w: duplicate population side %q", ErrInvalidArgument, specs[0].Side)
	}
	return nil
}

func ValidUUID(s string) bool { return uuidRe.MatchString(s) }

// PopulationRecord is one record exactly as the source contract delivers it.
// The amount is a decimal STRING — never a float.
type PopulationRecord struct {
	RecordID   string            `json:"record_id"`
	Reference  string            `json:"reference"`
	Amount     string            `json:"amount"`
	Currency   string            `json:"currency"`
	Date       string            `json:"date"` // YYYY-MM-DD
	Attributes map[string]string `json:"attributes,omitempty"`
}

func (r PopulationRecord) ParsedAmount() (*big.Rat, error) {
	v, ok := new(big.Rat).SetString(strings.TrimSpace(r.Amount))
	if !ok {
		return nil, fmt.Errorf("%w: record %q has non-decimal amount %q", ErrInvalidArgument, r.RecordID, r.Amount)
	}
	return v, nil
}

// ValidateRecord rejects a record that cannot be compared exactly. A bad
// record fails the whole population: silently dropping records is exactly how
// a completeness control lies.
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
	_, err := r.ParsedAmount()
	return err
}

// DecimalTotals returns the exact per-currency sum of the records' decimal
// amounts. Used to verify a source's declared_totals before any conversion.
func DecimalTotals(records []PopulationRecord) (map[string]*big.Rat, error) {
	sums := map[string]*big.Rat{}
	for _, r := range records {
		amt, err := r.ParsedAmount()
		if err != nil {
			return nil, err
		}
		if sums[r.Currency] == nil {
			sums[r.Currency] = new(big.Rat)
		}
		sums[r.Currency].Add(sums[r.Currency], amt)
	}
	return sums, nil
}

// ToMinorUnits converts an exact decimal amount to an integer count of units
// at the definition's fixed-point scale (10^-scale). It never rounds: an
// amount with more decimal places than the scale is refused (ZS-SVC-Z-001
// NP-04, "producer changes scale/precision silently" must fail safe), and one
// that does not fit an int64 is refused rather than wrapped.
func ToMinorUnits(amount string, scale int) (int64, error) {
	if scale < 0 || scale > MaxAmountScale {
		return 0, fmt.Errorf("%w: amount scale %d out of range", ErrInvalidArgument, scale)
	}
	v, ok := new(big.Rat).SetString(strings.TrimSpace(amount))
	if !ok {
		return 0, fmt.Errorf("%w: %q is not a decimal amount", ErrInvalidArgument, amount)
	}
	v.Mul(v, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)))
	if !v.IsInt() {
		return 0, fmt.Errorf("%w: %q has more than %d decimal places", ErrAmountNotRepresentable, amount, scale)
	}
	n := v.Num()
	if !n.IsInt64() {
		return 0, fmt.Errorf("%w: %q at scale %d", ErrAmountOverflow, amount, scale)
	}
	return n.Int64(), nil
}

// AddChecked adds two int64 amounts, refusing instead of silently wrapping.
func AddChecked(a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, ErrAmountOverflow
	}
	return a + b, nil
}

// CurrencyTotal is the control total of one currency bucket of a snapshot,
// in units at the run's amount scale.
type CurrencyTotal struct {
	Currency        string `json:"currency"`
	Count           int64  `json:"count"`
	TotalMinorUnits int64  `json:"total_minor_units"`
}

// FrozenPopulation is one side's population as read from its source and
// verified against the source's declared totals, converted to the run's
// amount scale, ready to be frozen by StartRun.
type FrozenPopulation struct {
	Side             string
	SourceSystem     string
	SpecRef          string
	Watermark        string
	DeclaredRowCount int64
	DeclaredTotals   map[string]string
	Items            []PopulationItemInput
}

// NewFrozenPopulation converts verified source records into frozen items at
// the given scale. A repeated record_id fails the population: record_id is the
// source's stable authoritative identity, so a repeat means the source broke
// its contract and the population cannot be told apart from a double-read.
func NewFrozenPopulation(spec PopulationSpec, watermark string, declaredRows int64, declared map[string]string,
	records []PopulationRecord, scale int) (FrozenPopulation, error) {
	fp := FrozenPopulation{Side: spec.Side, SourceSystem: spec.System, SpecRef: spec.Ref(), Watermark: watermark,
		DeclaredRowCount: declaredRows, DeclaredTotals: declared, Items: make([]PopulationItemInput, 0, len(records))}
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		if err := ValidateRecord(r); err != nil {
			return fp, err
		}
		if seen[r.RecordID] {
			return fp, fmt.Errorf("%w: %s delivered record_id %q more than once", ErrInvalidArgument, fp.SpecRef, r.RecordID)
		}
		seen[r.RecordID] = true
		amt, err := ToMinorUnits(r.Amount, scale)
		if err != nil {
			return fp, fmt.Errorf("%s record %q: %w", fp.SpecRef, r.RecordID, err)
		}
		date, _ := time.Parse("2006-01-02", r.Date) // validated above
		fp.Items = append(fp.Items, PopulationItemInput{SourceRecordID: r.RecordID, RefID: r.Reference,
			AmountMinorUnits: amt, Currency: r.Currency, OccurredAt: date.UTC(), Dimensions: r.Attributes})
	}
	return fp, nil
}

// ControlTotals sums a population per currency and in total, with overflow
// checks (A14): a total that does not fit is refused, never wrapped.
func ControlTotals(items []PopulationItemInput) (perCurrency []CurrencyTotal, total int64, err error) {
	byCcy := map[string]*CurrencyTotal{}
	for _, it := range items {
		c := byCcy[it.Currency]
		if c == nil {
			c = &CurrencyTotal{Currency: it.Currency}
			byCcy[it.Currency] = c
		}
		if c.TotalMinorUnits, err = AddChecked(c.TotalMinorUnits, it.AmountMinorUnits); err != nil {
			return nil, 0, fmt.Errorf("%w: %s total of the population", ErrAmountOverflow, it.Currency)
		}
		c.Count++
		if total, err = AddChecked(total, it.AmountMinorUnits); err != nil {
			return nil, 0, fmt.Errorf("%w: total of the population", ErrAmountOverflow)
		}
	}
	perCurrency = make([]CurrencyTotal, 0, len(byCcy))
	for _, c := range byCcy {
		perCurrency = append(perCurrency, *c)
	}
	sort.Slice(perCurrency, func(i, j int) bool { return perCurrency[i].Currency < perCurrency[j].Currency })
	return perCurrency, total, nil
}

// HashPopulation is a deterministic sha256 fingerprint of a frozen population:
// items sorted by source record id, every identity-bearing field covered
// (attributes included, since matching may key on them), so adding, removing
// or changing any record changes the hash.
func HashPopulation(items []PopulationItemInput) string {
	sorted := make([]PopulationItemInput, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SourceRecordID < sorted[j].SourceRecordID })
	h := sha256.New()
	for _, it := range sorted {
		keys := make([]string, 0, len(it.Dimensions))
		for k := range it.Dimensions {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(h, "%q|%q|%d|%s|%s", it.SourceRecordID, it.RefID, it.AmountMinorUnits, it.Currency, it.OccurredAt.UTC().Format("2006-01-02"))
		for _, k := range keys {
			fmt.Fprintf(h, "|%q=%q", k, it.Dimensions[k])
		}
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
