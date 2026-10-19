package domain

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Matching and comparison (ZS-CONTROL-001 §10, §11; ZS-SVC-Z-001 DQC-03).
//
// Items are paired on the definition's declared match key (A3), then each
// pair is compared under the run's pinned tolerance policy: currencies must
// agree (no implicit cross-currency comparison, A2), the amount difference
// must be within that currency's tolerance, and — when the policy declares one
// — the dates must be within the date tolerance (A5). After item matching,
// side totals are compared per currency for the whole population and for each
// declared balance dimension, so an aggregate cannot conceal a sub-dimension
// that is out of balance (DQC-03, INV-08, NP-21).

const (
	KeyRefID      = "ref_id"
	KeyCurrency   = "currency"
	KeyOccurredAt = "occurred_at"
	KeyAmount     = "amount_minor_units"
	keyDimPrefix  = "dimensions."

	maxKeyFields        = 8
	maxBalanceDims      = 8
	maxDateToleranceDay = 366
	bpsDenominator      = 10000
)

var dimensionNameRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ValidateMatchKeyFields accepts the fields an item actually has. A field the
// matcher cannot read would make the definition describe a rule that is not
// applied — the defect A3 records.
func ValidateMatchKeyFields(fields []string) error {
	if len(fields) == 0 || len(fields) > maxKeyFields {
		return fmt.Errorf("%w: match_key_fields needs 1 to %d fields", ErrInvalidArgument, maxKeyFields)
	}
	seen := map[string]bool{}
	for _, f := range fields {
		switch {
		case f == KeyRefID, f == KeyCurrency, f == KeyOccurredAt, f == KeyAmount:
		case strings.HasPrefix(f, keyDimPrefix) && dimensionNameRe.MatchString(strings.TrimPrefix(f, keyDimPrefix)):
		default:
			return fmt.Errorf("%w: unknown match key field %q (use ref_id, currency, occurred_at, amount_minor_units or dimensions.<name>)", ErrInvalidArgument, f)
		}
		if seen[f] {
			return fmt.Errorf("%w: match key field %q repeated", ErrInvalidArgument, f)
		}
		seen[f] = true
	}
	return nil
}

func ValidateBalanceDimensions(dims []string) error {
	if len(dims) > maxBalanceDims {
		return fmt.Errorf("%w: at most %d balance_dimensions", ErrInvalidArgument, maxBalanceDims)
	}
	seen := map[string]bool{}
	for _, d := range dims {
		if !dimensionNameRe.MatchString(d) || seen[d] {
			return fmt.Errorf("%w: invalid or repeated balance dimension %q", ErrInvalidArgument, d)
		}
		seen[d] = true
	}
	return nil
}

// CurrencyTolerance is ONE kind of tolerance for one currency: an absolute
// amount (units at the run's amount scale) or a percentage of an explicit base
// in basis points. ZS-CONTROL-001 §11 does not say how an absolute and a
// percentage tolerance combine (either? both?), so a currency declares one.
type CurrencyTolerance struct {
	AbsoluteMinorUnits *int64 `json:"absolute_minor_units,omitempty"`
	PercentageBps      *int64 `json:"percentage_bps,omitempty"`
}

// TolerancePolicy is the versioned tolerance of a definition, pinned on every
// run. A currency without an entry must match exactly: missing configuration
// can only make the comparison stricter, never hide a difference.
type TolerancePolicy struct {
	Currencies map[string]CurrencyTolerance `json:"currencies,omitempty"`
	// PercentageBaseSide is the side whose amount a percentage tolerance is a
	// percentage of (§11: "base must be explicit"). Required when any currency
	// uses a percentage.
	PercentageBaseSide string `json:"percentage_base_side,omitempty"`
	// DateDays is the calendar-day tolerance between paired items' dates. Nil
	// means dates are not part of the comparison — stated on the pinned policy,
	// not implied.
	DateDays *int `json:"date_days,omitempty"`
}

func (p TolerancePolicy) Validate(sides []string) error {
	usesPct := false
	for ccy, t := range p.Currencies {
		if !currencyRe.MatchString(ccy) {
			return fmt.Errorf("%w: tolerance currency %q is not an ISO-4217 code", ErrInvalidArgument, ccy)
		}
		switch {
		case (t.AbsoluteMinorUnits == nil) == (t.PercentageBps == nil):
			return fmt.Errorf("%w: tolerance for %s must declare exactly one of absolute_minor_units or percentage_bps", ErrInvalidArgument, ccy)
		case t.AbsoluteMinorUnits != nil && *t.AbsoluteMinorUnits < 0:
			return fmt.Errorf("%w: %s absolute tolerance cannot be negative", ErrInvalidArgument, ccy)
		case t.PercentageBps != nil && (*t.PercentageBps < 0 || *t.PercentageBps > bpsDenominator):
			return fmt.Errorf("%w: %s percentage_bps must be 0..%d", ErrInvalidArgument, ccy, bpsDenominator)
		}
		if t.PercentageBps != nil {
			usesPct = true
		}
	}
	if usesPct {
		ok := false
		for _, s := range sides {
			ok = ok || s == p.PercentageBaseSide
		}
		if !ok {
			return fmt.Errorf("%w: percentage tolerance needs percentage_base_side naming one of the populations", ErrInvalidArgument)
		}
	} else if p.PercentageBaseSide != "" {
		return fmt.Errorf("%w: percentage_base_side is set but no currency uses a percentage tolerance", ErrInvalidArgument)
	}
	if p.DateDays != nil && (*p.DateDays < 0 || *p.DateDays > maxDateToleranceDay) {
		return fmt.Errorf("%w: date_days must be 0..%d", ErrInvalidArgument, maxDateToleranceDay)
	}
	return nil
}

// Allowed is the amount difference tolerated in ccy, given the base side's
// amount. A percentage is floored: the tolerance never rounds in the
// preparer's favour.
func (p TolerancePolicy) Allowed(ccy string, baseAmount int64) int64 {
	t, ok := p.Currencies[ccy]
	if !ok {
		return 0
	}
	if t.AbsoluteMinorUnits != nil {
		return *t.AbsoluteMinorUnits
	}
	v := new(big.Int).Abs(big.NewInt(baseAmount))
	v.Mul(v, big.NewInt(*t.PercentageBps))
	v.Quo(v, big.NewInt(bpsDenominator))
	return v.Int64() // ≤ |baseAmount|, so it fits
}

// MatchItem is a frozen item as the matcher sees it.
type MatchItem struct {
	ItemID         string
	Side           string
	SourceRecordID string
	RefID          string
	Amount         int64
	Currency       string
	OccurredAt     time.Time
	Dimensions     map[string]string
}

// ExceptionDetail is the observed evidence and the threshold behind an
// exception (ZS-SVC-Z-001 TC-03: "every rule result stores expected
// predicate/threshold and observed evidence").
type ExceptionDetail struct {
	Rule               string           `json:"rule"`
	CounterpartItemID  string           `json:"counterpart_item_id,omitempty"`
	Currency           string           `json:"currency,omitempty"`
	CounterpartCcy     string           `json:"counterpart_currency,omitempty"`
	DifferenceMinor    *int64           `json:"difference_minor_units,omitempty"`
	AllowedMinor       *int64           `json:"allowed_minor_units,omitempty"`
	DateDifferenceDays *int64           `json:"date_difference_days,omitempty"`
	AllowedDays        *int             `json:"allowed_days,omitempty"`
	MissingKeyField    string           `json:"missing_key_field,omitempty"`
	SideTotals         map[string]int64 `json:"side_totals_minor_units,omitempty"`
}

const (
	RuleUnmatched        = "UNMATCHED"
	RuleMissingKey       = "MISSING_KEY_FIELD"
	RuleCurrencyMismatch = "CURRENCY_MISMATCH"
	RuleAmountTolerance  = "AMOUNT_OUTSIDE_TOLERANCE"
	RuleDateTolerance    = "DATE_OUTSIDE_TOLERANCE"
	RuleBalance          = "BALANCE_OUTSIDE_TOLERANCE"
)

// Compare checks one candidate pair under the policy. It returns nil when the
// pair reconciles, or the reason and evidence it does not. Currency is checked
// first and alone: amounts in different currencies are never compared.
func Compare(a, b MatchItem, p TolerancePolicy) (string, *ExceptionDetail) {
	if a.Currency != b.Currency {
		return fmt.Sprintf("currency mismatch: %s on %s, %s on %s", a.Currency, a.Side, b.Currency, b.Side),
			&ExceptionDetail{Rule: RuleCurrencyMismatch, Currency: a.Currency, CounterpartCcy: b.Currency}
	}
	base := a.Amount
	if p.PercentageBaseSide == b.Side {
		base = b.Amount
	}
	allowed := p.Allowed(a.Currency, base)
	diff, overflow := absDiff(a.Amount, b.Amount)
	if overflow {
		return fmt.Sprintf("amount difference exceeds the representable range (%s)", a.Currency),
			&ExceptionDetail{Rule: RuleAmountTolerance, Currency: a.Currency, AllowedMinor: &allowed}
	}
	if diff > allowed {
		return fmt.Sprintf("amount difference %d exceeds %s tolerance %d", diff, a.Currency, allowed),
			&ExceptionDetail{Rule: RuleAmountTolerance, Currency: a.Currency, DifferenceMinor: &diff, AllowedMinor: &allowed}
	}
	if p.DateDays != nil {
		days := int64(a.OccurredAt.UTC().Truncate(24*time.Hour).Sub(b.OccurredAt.UTC().Truncate(24*time.Hour)) / (24 * time.Hour))
		if days < 0 {
			days = -days
		}
		if days > int64(*p.DateDays) {
			return fmt.Sprintf("timing difference of %d days exceeds date tolerance %d", days, *p.DateDays),
				&ExceptionDetail{Rule: RuleDateTolerance, Currency: a.Currency, DateDifferenceDays: &days, AllowedDays: p.DateDays}
		}
	}
	return "", nil
}

// signedDiff returns a-b and absDiff |a-b|, each reporting overflow (ok=false)
// instead of wrapping.
func signedDiff(a, b int64) (int64, bool) {
	d := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	return d.Int64(), d.IsInt64()
}

func absDiff(a, b int64) (int64, bool) {
	d := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
	d.Abs(d)
	return d.Int64(), !d.IsInt64()
}

// MatchKey builds an item's composite key from the declared fields. It returns
// the first field the item lacks; such an item is never auto-matched — an
// absent value must not pair with another absent value.
func MatchKey(it MatchItem, fields []string) (key, missing string) {
	parts := make([]string, len(fields))
	for i, f := range fields {
		var v string
		switch {
		case f == KeyRefID:
			v = it.RefID
		case f == KeyCurrency:
			v = it.Currency
		case f == KeyOccurredAt:
			v = it.OccurredAt.UTC().Format("2006-01-02")
		case f == KeyAmount:
			v = fmt.Sprint(it.Amount)
		case strings.HasPrefix(f, keyDimPrefix):
			v = it.Dimensions[strings.TrimPrefix(f, keyDimPrefix)]
		}
		if v == "" {
			return "", f
		}
		parts[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(parts, "|"), ""
}

type MatchedPair struct{ A, B MatchItem }

type ItemException struct {
	Item   MatchItem
	Reason string
	Detail ExceptionDetail
}

// AutoMatch pairs side A against side B on the composite key, deterministically
// (items in source record id order; for each A item the first B candidate with
// the same key that reconciles, otherwise the first candidate, whose pair then
// becomes two exceptions). Items without a counterpart are exceptions.
func AutoMatch(sideA, sideB string, itemsA, itemsB []MatchItem, keyFields []string, p TolerancePolicy) ([]MatchedPair, []ItemException) {
	sortItems(itemsA)
	sortItems(itemsB)
	var pairs []MatchedPair
	var exc []ItemException
	missingKey := func(it MatchItem, f string) {
		exc = append(exc, ItemException{Item: it, Reason: fmt.Sprintf("item has no value for match key field %s", f),
			Detail: ExceptionDetail{Rule: RuleMissingKey, MissingKeyField: f, Currency: it.Currency}})
	}

	candidates := map[string][]MatchItem{}
	var keyOrder []string
	for _, b := range itemsB {
		k, miss := MatchKey(b, keyFields)
		if miss != "" {
			missingKey(b, miss)
			continue
		}
		if _, ok := candidates[k]; !ok {
			keyOrder = append(keyOrder, k)
		}
		candidates[k] = append(candidates[k], b)
	}

	for _, a := range itemsA {
		k, miss := MatchKey(a, keyFields)
		if miss != "" {
			missingKey(a, miss)
			continue
		}
		pool := candidates[k]
		if len(pool) == 0 {
			exc = append(exc, ItemException{Item: a, Reason: fmt.Sprintf("no matching key in %s", sideB),
				Detail: ExceptionDetail{Rule: RuleUnmatched, Currency: a.Currency}})
			continue
		}
		pick := -1
		for i, b := range pool {
			if r, _ := Compare(a, b, p); r == "" {
				pick = i
				break
			}
		}
		if pick >= 0 {
			pairs = append(pairs, MatchedPair{A: a, B: pool[pick]})
			candidates[k] = append(pool[:pick:pick], pool[pick+1:]...)
			continue
		}
		b := pool[0]
		candidates[k] = pool[1:]
		reason, detail := Compare(a, b, p)
		da, db := *detail, *detail
		da.CounterpartItemID, db.CounterpartItemID = b.ItemID, a.ItemID
		if detail.Rule == RuleCurrencyMismatch {
			db.Currency, db.CounterpartCcy = b.Currency, a.Currency
		}
		exc = append(exc, ItemException{Item: a, Reason: reason, Detail: da}, ItemException{Item: b, Reason: reason, Detail: db})
	}
	for _, k := range keyOrder {
		for _, b := range candidates[k] {
			exc = append(exc, ItemException{Item: b, Reason: fmt.Sprintf("no matching key in %s", sideA),
				Detail: ExceptionDetail{Rule: RuleUnmatched, Currency: b.Currency}})
		}
	}
	return pairs, exc
}

func sortItems(items []MatchItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].SourceRecordID != items[j].SourceRecordID {
			return items[i].SourceRecordID < items[j].SourceRecordID
		}
		return items[i].ItemID < items[j].ItemID
	})
}

// BalanceVariance is a per-currency total that does not balance between the
// sides. Dimension "" is the whole population; otherwise the bucket is one
// value of a declared balance dimension ("" when items lack the attribute).
type BalanceVariance struct {
	Dimension      string
	DimensionValue string
	Currency       string
	TotalA, TotalB int64
	Difference     int64
	Allowed        int64
}

// CheckBalances compares side totals per currency, for the whole population
// and for every value of every declared balance dimension, under the
// currency's tolerance (the percentage base is the base side's total). A
// balanced aggregate therefore never hides an unbalanced sub-dimension.
// Totals are overflow-checked.
func CheckBalances(sideA, sideB string, itemsA, itemsB []MatchItem, dims []string, p TolerancePolicy) ([]BalanceVariance, error) {
	type bucket struct{ dim, val, ccy string }
	totals := map[bucket]*[2]int64{}
	add := func(side int, items []MatchItem) error {
		for _, it := range items {
			keys := []bucket{{"", "", it.Currency}}
			for _, d := range dims {
				keys = append(keys, bucket{d, it.Dimensions[d], it.Currency})
			}
			for _, k := range keys {
				t := totals[k]
				if t == nil {
					t = &[2]int64{}
					totals[k] = t
				}
				var err error
				if t[side], err = AddChecked(t[side], it.Amount); err != nil {
					return fmt.Errorf("%w: %s total for %s", ErrAmountOverflow, it.Currency, it.Side)
				}
			}
		}
		return nil
	}
	if err := add(0, itemsA); err != nil {
		return nil, err
	}
	if err := add(1, itemsB); err != nil {
		return nil, err
	}

	var out []BalanceVariance
	for k, t := range totals {
		base := t[0]
		if p.PercentageBaseSide == sideB {
			base = t[1]
		}
		diff, ok := signedDiff(t[0], t[1])
		allowed := p.Allowed(k.ccy, base)
		abs, overflow := absDiff(t[0], t[1])
		if !ok || overflow {
			return nil, fmt.Errorf("%w: %s difference between sides", ErrAmountOverflow, k.ccy)
		}
		if abs > allowed {
			out = append(out, BalanceVariance{Dimension: k.dim, DimensionValue: k.val, Currency: k.ccy,
				TotalA: t[0], TotalB: t[1], Difference: diff, Allowed: allowed})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Dimension != b.Dimension {
			return a.Dimension < b.Dimension
		}
		if a.DimensionValue != b.DimensionValue {
			return a.DimensionValue < b.DimensionValue
		}
		return a.Currency < b.Currency
	})
	return out, nil
}
