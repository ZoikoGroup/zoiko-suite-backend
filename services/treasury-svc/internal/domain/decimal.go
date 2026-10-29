package domain

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// Decimal is an exact monetary or rate value carried as decimal text.
//
// Binary floating point is prohibited for money (ZS-DATA-001 D06): every
// amount in this service is parsed from the caller's decimal digits into a
// big.Rat and never routed through float64. On the wire a Decimal stays a
// bare JSON number, exactly as the float64 fields it replaces did, so an
// existing caller decoding `amount` into a number keeps working; a quoted
// string is also accepted on input so callers can move to the string form
// D06 prefers without a coordinated release.
//
// The same type serves both column scales in this service. Money columns
// are NUMERIC(20,4) and rate columns are NUMERIC(20,8); the validators
// below carry the scale and bound for each, rather than pretending one
// scale fits both.
type Decimal string

const (
	// MoneyScale is the scale of every NUMERIC(20,4) column in this
	// service: cash_balances, liquidity_thresholds, treasury_transfers,
	// cash_position_snapshots and fx_exposure_snapshots' amount columns.
	MoneyScale = 4
	// MoneyIntegerDigits is NUMERIC(20,4)'s integer-digit capacity.
	MoneyIntegerDigits = 16
	// RateScale is the scale of fx_rates.rate and
	// fx_exposure_snapshots.rate_used: NUMERIC(20,8).
	RateScale = 8
	// RateIntegerDigits is NUMERIC(20,8)'s integer-digit capacity.
	RateIntegerDigits = 12
)

var (
	// ErrAmountPrecision is returned when a money value has more fractional
	// digits, or more integer digits, than a NUMERIC(20,4) column stores.
	// The value is refused, never rounded.
	ErrAmountPrecision = errorString("amount has more decimal places or integer digits than the NUMERIC(20,4) column stores")
	// ErrRatePrecision is the NUMERIC(20,8) counterpart for FX rates.
	ErrRatePrecision = errorString("fx rate has more decimal places or integer digits than the NUMERIC(20,8) column stores")

	// moneyScaleFactor is 10^4: a money value must be an integer multiple
	// of 0.0001 or Postgres would round it on insert.
	moneyScaleFactor = new(big.Rat).SetInt(pow10(MoneyScale))
	// moneyLimit is 10^16: NUMERIC(20,4)'s exclusive upper bound.
	moneyLimit = new(big.Rat).SetInt(pow10(MoneyIntegerDigits))
	// rateScaleFactor is 10^8, rateLimit is 10^12.
	rateScaleFactor = new(big.Rat).SetInt(pow10(RateScale))
	rateLimit       = new(big.Rat).SetInt(pow10(RateIntegerDigits))
)

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// ParseAmount parses an exact decimal string using the JSON-number grammar.
// big.Rat.SetString is more permissive — it accepts fractions ("1/3") and
// other forms that are not decimal amounts — so the grammar check keeps every
// Decimal a terminating decimal, which is what the columns, hashes and JSON
// wire shape promise.
func ParseAmount(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("amount is empty")
	}
	if len(s) > maxDecimalTextLen {
		return nil, fmt.Errorf("amount is longer than %d characters", maxDecimalTextLen)
	}
	if !jsonNumberRe.MatchString(s) {
		return nil, fmt.Errorf("%q is not a decimal number", s)
	}
	// A short string such as "1e999999" would otherwise make big.Rat
	// allocate an enormous integer. Every legitimate money/rate value is
	// many orders of magnitude below this bound.
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(s[i+1:])
		if err != nil || exp > maxDecimalExponent || exp < -maxDecimalExponent {
			return nil, fmt.Errorf("decimal exponent is outside ±%d", maxDecimalExponent)
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal number", s)
	}
	// The input can be short but expand hugely (for example a long mantissa
	// with an exponent). Enforce the bound on the canonical plain-decimal
	// form too, so Rat/MarshalJSON can always parse the stored Decimal again.
	if len(CanonicalAmount(r)) > maxDecimalTextLen {
		return nil, fmt.Errorf("decimal is longer than %d characters after canonicalization", maxDecimalTextLen)
	}
	return r, nil
}

const (
	// A NUMERIC(20,8) value needs at most about 30 characters; these limits
	// are deliberately far above any valid input and exist to bound parser
	// work on hostile JSON. The exponent bound also keeps the canonical
	// plain-decimal form well inside maxDecimalTextLen.
	maxDecimalTextLen  = 128
	maxDecimalExponent = 30
)

// CanonicalAmount renders r as a plain decimal string (never scientific
// notation), trimming trailing fractional zeros so "7.00" and "7" compare
// and print identically. It renders the exact number of decimal places the
// rational requires, so a value with more digits than a column allows is
// still visible to the validator instead of being rounded away first.
func CanonicalAmount(r *big.Rat) string {
	if r.IsInt() {
		return r.RatString()
	}
	scale, exact := terminatingScale(r.Denom())
	if !exact {
		// ParseAmount refuses fractions, so this is reachable only if a
		// caller passes a non-terminating big.Rat directly. Twelve places
		// is the largest scale this service's columns or conversions
		// produce.
		scale = 12
	}
	s := r.FloatString(scale)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}

// terminatingScale returns the number of fractional digits needed to write
// denom's reciprocal as a terminating decimal, and whether it terminates.
// Decimal inputs reduce to 2^a * 5^b, so this is exact for every value the
// wire grammar admits; it is also exact for products and sums of decimals.
func terminatingScale(denom *big.Int) (int, bool) {
	n := new(big.Int).Set(denom)
	twos, fives := 0, 0
	two := big.NewInt(2)
	five := big.NewInt(5)
	for {
		q, rem := new(big.Int).QuoRem(n, two, new(big.Int))
		if rem.Sign() != 0 {
			break
		}
		n, twos = q, twos+1
	}
	for {
		q, rem := new(big.Int).QuoRem(n, five, new(big.Int))
		if rem.Sign() != 0 {
			break
		}
		n, fives = q, fives+1
	}
	if n.Cmp(big.NewInt(1)) != 0 {
		return 0, false
	}
	if twos > fives {
		return twos, true
	}
	return fives, true
}

// jsonNumberRe is the JSON number grammar (RFC 8259 §6). big.Rat.SetString
// also accepts fractions, which are not amounts; this rejects them.
var jsonNumberRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// Rat parses d for exact arithmetic.
func (d Decimal) Rat() (*big.Rat, error) { return ParseAmount(string(d)) }

// IsZero reports whether d is unset or numerically zero. An unset Decimal
// is the zero value of an optional request field.
func (d Decimal) IsZero() bool {
	if d == "" {
		return true
	}
	r, err := d.Rat()
	return err == nil && r.Sign() == 0
}

// OrZero maps an unset Decimal to "0" for storage of an optional figure
// whose column is NOT NULL. It never changes a set value, and it is not a
// validator: a malformed non-empty value is still rejected by the scanner.
func (d Decimal) OrZero() Decimal {
	if d == "" {
		return "0"
	}
	return d
}

// MarshalJSON writes d as a JSON number literal of exactly its digits. An
// unset Decimal is written as null, matching the optional-field behaviour
// of the float64 fields this replaces.
func (d Decimal) MarshalJSON() ([]byte, error) {
	if d == "" {
		return []byte("null"), nil
	}
	r, err := d.Rat()
	if err != nil {
		return nil, fmt.Errorf("marshal decimal: %w", err)
	}
	return []byte(CanonicalAmount(r)), nil
}

// UnmarshalJSON accepts a JSON number or a string holding one, and keeps
// its canonical decimal form. null leaves d unset.
func (d *Decimal) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "null" {
		*d = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
	}
	if !jsonNumberRe.MatchString(s) {
		return fmt.Errorf("amount %q is not a decimal number", s)
	}
	r, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*d = Decimal(CanonicalAmount(r))
	return nil
}

// Scan reads a NUMERIC selected as text (the stores always select
// amount::text / rate::text, so pgx never materialises a float on the way).
// The value is canonicalised so a row read back as "10.5000" compares and
// prints the same as the "10.5" that was written.
func (d *Decimal) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case nil:
		*d = ""
		return nil
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("cannot scan %T into Decimal", src)
	}
	r, err := ParseAmount(s)
	if err != nil {
		return err
	}
	*d = Decimal(CanonicalAmount(r))
	return nil
}

// Add returns d + o exactly.
func (d Decimal) Add(o Decimal) (Decimal, error) {
	a, b, err := d.ratPair(o)
	if err != nil {
		return "", err
	}
	return Decimal(CanonicalAmount(new(big.Rat).Add(a, b))), nil
}

// Sub returns d - o exactly.
func (d Decimal) Sub(o Decimal) (Decimal, error) {
	a, b, err := d.ratPair(o)
	if err != nil {
		return "", err
	}
	return Decimal(CanonicalAmount(new(big.Rat).Sub(a, b))), nil
}

// Mul returns d * o exactly. The caller decides where the product must be
// rounded to a column's scale; this never rounds on its own.
func (d Decimal) Mul(o Decimal) (Decimal, error) {
	a, b, err := d.ratPair(o)
	if err != nil {
		return "", err
	}
	return Decimal(CanonicalAmount(new(big.Rat).Mul(a, b))), nil
}

// Cmp compares d and o numerically, returning -1, 0 or +1.
func (d Decimal) Cmp(o Decimal) (int, error) {
	a, b, err := d.ratPair(o)
	if err != nil {
		return 0, err
	}
	return a.Cmp(b), nil
}

// Abs returns |d| exactly.
func (d Decimal) Abs() (Decimal, error) {
	r, err := d.Rat()
	if err != nil {
		return "", err
	}
	return Decimal(CanonicalAmount(new(big.Rat).Abs(r))), nil
}

// Neg returns -d exactly.
func (d Decimal) Neg() (Decimal, error) {
	r, err := d.Rat()
	if err != nil {
		return "", err
	}
	return Decimal(CanonicalAmount(new(big.Rat).Neg(r))), nil
}

// Rounded returns d rounded to scale fractional digits. It is used only
// where a column's scale is a deliberate output policy (FX conversion),
// never to make an input fit a column: an input that would be rounded is
// refused by the validators below.
func (d Decimal) Rounded(scale int) (Decimal, error) {
	r, err := d.Rat()
	if err != nil {
		return "", err
	}
	rounded, ok := new(big.Rat).SetString(r.FloatString(scale))
	if !ok {
		return "", fmt.Errorf("round decimal: %q", r.FloatString(scale))
	}
	return Decimal(CanonicalAmount(rounded)), nil
}

func (d Decimal) ratPair(o Decimal) (*big.Rat, *big.Rat, error) {
	a, err := d.Rat()
	if err != nil {
		return nil, nil, err
	}
	b, err := o.Rat()
	if err != nil {
		return nil, nil, err
	}
	return a, b, nil
}

// SumDecimals adds a list exactly. It is the loop-friendly form of Add and
// parses each value once.
func SumDecimals(values ...Decimal) (Decimal, error) {
	sum := new(big.Rat)
	for _, v := range values {
		r, err := v.Rat()
		if err != nil {
			return "", err
		}
		sum.Add(sum, r)
	}
	return Decimal(CanonicalAmount(sum)), nil
}

// NetDecimals returns base minus every subtraction, exactly. It is the
// one-pass form used by the cash-position and forecast arithmetic.
func NetDecimals(base Decimal, subs ...Decimal) (Decimal, error) {
	sum, err := base.Rat()
	if err != nil {
		return "", err
	}
	for _, v := range subs {
		r, err := v.Rat()
		if err != nil {
			return "", err
		}
		sum.Sub(sum, r)
	}
	return Decimal(CanonicalAmount(sum)), nil
}

// ValidateMoney checks that d is a decimal the NUMERIC(20,4) money columns
// store exactly, with no rounding and no overflow. It allows negative
// values (a bank balance can be overdrawn); command inputs that must be
// positive use ValidatePositiveMoney or ValidateNonNegativeMoney.
func ValidateMoney(d Decimal) (*big.Rat, error) {
	r, err := d.Rat()
	if err != nil {
		return nil, ErrInvalidAmount
	}
	if err := checkScaleAndBound(r, moneyScaleFactor, moneyLimit); err != nil {
		return nil, ErrAmountPrecision
	}
	return r, nil
}

// ValidatePositiveMoney is ValidateMoney plus the > 0 rule every transfer
// amount and FX rate needs.
func ValidatePositiveMoney(d Decimal) (*big.Rat, error) {
	r, err := ValidateMoney(d)
	if err != nil {
		return nil, err
	}
	if r.Sign() <= 0 {
		return nil, ErrInvalidAmount
	}
	return r, nil
}

// ValidateNonNegativeMoney is ValidateMoney plus a >= 0 rule for
// caller-supplied figures that are exclusions, not movements.
func ValidateNonNegativeMoney(d Decimal) (*big.Rat, error) {
	r, err := ValidateMoney(d)
	if err != nil {
		return nil, err
	}
	if r.Sign() < 0 {
		return nil, ErrInvalidAmount
	}
	return r, nil
}

// ValidateRate checks that d is a positive rate the NUMERIC(20,8) columns
// store exactly. A rate is not money and has a different scale and bound,
// so it does not share the money validators.
func ValidateRate(d Decimal) (*big.Rat, error) {
	r, err := d.Rat()
	if err != nil {
		return nil, ErrInvalidFXRate
	}
	if r.Sign() <= 0 {
		return nil, ErrInvalidFXRate
	}
	if err := checkScaleAndBound(r, rateScaleFactor, rateLimit); err != nil {
		return nil, ErrRatePrecision
	}
	return r, nil
}

func checkScaleAndBound(r *big.Rat, scaleFactor, limit *big.Rat) error {
	if !new(big.Rat).Mul(r, scaleFactor).IsInt() {
		return ErrAmountPrecision
	}
	if new(big.Rat).Abs(r).Cmp(limit) >= 0 {
		return ErrAmountPrecision
	}
	return nil
}

// MoneyColumnText renders a validated money value the way a NUMERIC(20,4)
// column holds it and ::text reads it back. New hashes use this exact
// four-place text, not a float64.
//
// It is not byte-identical to the old fmt.Sprintf("%.4f", float64) for every
// column value: the nearest float64 to a large 4-place decimal can round to
// the neighbouring 0.0001. The store keeps a verification-only legacy
// renderer for rows hashed before this pass; see protectedFieldHashMatches.
func MoneyColumnText(r *big.Rat) string { return r.FloatString(MoneyScale) }

// MoneyText is MoneyColumnText for a Decimal. A malformed value is
// rendered as its raw text (never as zero), so a corrupted row produces a
// mismatching hash and fails closed rather than hashing like a valid 0.
func (d Decimal) MoneyText() string {
	r, err := d.Rat()
	if err != nil {
		return string(d)
	}
	return MoneyColumnText(r)
}

// RateColumnText is MoneyColumnText's NUMERIC(20,8) counterpart.
func RateColumnText(r *big.Rat) string { return r.FloatString(RateScale) }
