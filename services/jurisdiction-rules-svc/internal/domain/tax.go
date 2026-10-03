package domain

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// ZS-JUR-001 Wave 4 (tax half): rule parameters and the exact-decimal tax
// calculation (s11, s12).
//
// DECISION (recorded in docs/architecture/jurisdiction-pack-decision-rule-parameters.md):
// ZS-JUR-001 is the governing standard, so calculation values live in released,
// immutable, sourced and tested rule modules, not in editable rows (s2) and not
// in the tax services. They are carried in a NEW typed field, `parameters`;
// `rule_payload` stays applicability metadata, so nothing existing changes.
//
// No rule formula language exists (s38), so this is deliberately not one: two
// fixed, closed families that cover the common deterministic cases, validated
// strictly, executed with exact rational arithmetic (never binary floating
// point: JUR-NEG-25) and a single, declared rounding step.
//
//	TAX_RATE   {"family":"TAX_RATE","rate":"0.2000","rounding":{"mode":"HALF_UP","scale":2}}
//	TAX_BANDS  {"family":"TAX_BANDS","bands":[{"up_to":"12570","rate":"0"},{"up_to":"50270","rate":"0.2"},{"up_to":null,"rate":"0.4"}],
//	            "rounding":{"mode":"HALF_UP","scale":2}}   // marginal bands: each rate applies only to the slice of the amount inside its band

const (
	FamilyTaxRate  = "TAX_RATE"
	FamilyTaxBands = "TAX_BANDS"
	// FamilyAmount is a fixed statutory amount (an allowance, a threshold, a statutory
	// pay rate per week): a PARAMETER for a consumer such as payroll, not something
	// this service calculates with.
	FamilyAmount = "AMOUNT"

	RoundHalfUp   = "HALF_UP"
	RoundHalfEven = "HALF_EVEN"
	RoundUp       = "UP"   // toward positive infinity (amounts are never negative here)
	RoundDown     = "DOWN" // toward zero

	// Calculation outcomes (in addition to the resolution outcomes).
	CalcCalculated    = "CALCULATED"
	CalcNoParameters  = "NO_PARAMETERS"
	CalcInvalidFacts  = "INVALID_FACTS"
	CalcParameterOnly = "PARAMETER_ONLY"
	maxAmountDigits   = 15
	maxParameterBands = 50
)

// ErrParametersInvalid is returned for malformed rule parameters.
var ErrParametersInvalid = errorString("rule parameters are invalid")

func paramBad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrParametersInvalid, fmt.Sprintf(format, a...))
}

// Rounding is the one declared rounding step, applied once to the total.
type Rounding struct {
	Mode  string `json:"mode"`
	Scale int    `json:"scale"`
}

// BandSpec is one marginal band as written. UpTo nil means "no upper limit".
type BandSpec struct {
	UpTo *string `json:"up_to"`
	Rate string  `json:"rate"`
}

// RuleParameters is the wire form of a rule's calculation parameters.
type RuleParameters struct {
	Family string `json:"family"`
	// Class is the s17 statutory parameter class (optional in general, REQUIRED for
	// rules in the PAYROLL domain, enforced by the compiler).
	Class    string     `json:"class,omitempty"`
	Rate     *string    `json:"rate,omitempty"`
	Bands    []BandSpec `json:"bands,omitempty"`
	Amount   *string    `json:"amount,omitempty"`
	Unit     string     `json:"unit,omitempty"`
	Rounding Rounding   `json:"rounding"`

	// Wave 6: RETENTION and MAPPING families (records.go).
	RecordClass       string         `json:"record_class,omitempty"`
	Trigger           string         `json:"trigger,omitempty"`
	Minimum           *Duration      `json:"minimum,omitempty"`
	FormatRequirement string         `json:"format_requirement,omitempty"`
	LegalHoldOverride *bool          `json:"legal_hold_override,omitempty"`
	DestructionRule   string         `json:"destruction_rule,omitempty"`
	MappingType       string         `json:"mapping_type,omitempty"`
	Entries           []MappingEntry `json:"entries,omitempty"`

	// Wave 5: EINVOICE_PROFILE and FILING_PROFILE families (submission.go).
	Profile *SubmissionProfile `json:"profile,omitempty"`
}

var decimalRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,14})(\.[0-9]{1,12})?$`)

// parseDecimal reads a non-negative decimal string exactly.
func parseDecimal(s string) (*big.Rat, bool) {
	if !decimalRe.MatchString(s) {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

// ParseRuleParameters strictly decodes and validates parameters.
func ParseRuleParameters(raw []byte) (RuleParameters, error) {
	var p RuleParameters
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, paramBad("%v", err)
	}
	if dec.More() {
		return p, paramBad("trailing data")
	}
	return p, p.Validate()
}

// Validate checks the parameters' shape and values.
func (p RuleParameters) Validate() error {
	if p.Class != "" && !ValidParameterClass(p.Class) {
		return paramBad("class must be one of %s", strings.Join(ParameterClasses, ", "))
	}
	switch p.Family {
	case FamilyAmount:
		return p.validateAmount()
	case FamilyRetention:
		return p.validateRetention()
	case FamilyMapping:
		return p.validateMapping()
	case FamilyEInvoiceProfile:
		return p.validateSubmissionProfile(false)
	case FamilyFilingProfile:
		return p.validateSubmissionProfile(true)
	}
	if p.RecordClass != "" || p.Trigger != "" || p.Minimum != nil || p.MappingType != "" || len(p.Entries) != 0 || p.DestructionRule != "" ||
		p.FormatRequirement != "" || p.LegalHoldOverride != nil || p.Profile != nil {
		return paramBad("retention and mapping fields belong to the RETENTION and MAPPING families")
	}
	if p.Amount != nil || p.Unit != "" {
		return paramBad("amount and unit belong to the AMOUNT family")
	}
	switch p.Rounding.Mode {
	case RoundHalfUp, RoundHalfEven, RoundUp, RoundDown:
	default:
		return paramBad("rounding.mode must be HALF_UP, HALF_EVEN, UP or DOWN")
	}
	if p.Rounding.Scale < 0 || p.Rounding.Scale > 6 {
		return paramBad("rounding.scale must be 0 to 6")
	}
	one := big.NewRat(1, 1)
	checkRate := func(label, s string) error {
		r, ok := parseDecimal(s)
		if !ok {
			return paramBad("%s %q must be a non-negative decimal string such as \"0.2000\" (numbers are refused: JUR-NEG-25)", label, s)
		}
		if r.Cmp(one) > 0 {
			return paramBad("%s %q is above 1 (100%%)", label, s)
		}
		return nil
	}
	switch p.Family {
	case FamilyTaxRate:
		if p.Rate == nil {
			return paramBad("TAX_RATE needs rate")
		}
		if len(p.Bands) != 0 {
			return paramBad("TAX_RATE does not take bands")
		}
		return checkRate("rate", *p.Rate)
	case FamilyTaxBands:
		if p.Rate != nil {
			return paramBad("TAX_BANDS does not take a flat rate")
		}
		if len(p.Bands) < 2 || len(p.Bands) > maxParameterBands {
			return paramBad("TAX_BANDS needs 2 to %d bands (use TAX_RATE for one rate)", maxParameterBands)
		}
		var prev *big.Rat
		for i, b := range p.Bands {
			if err := checkRate(fmt.Sprintf("bands[%d].rate", i), b.Rate); err != nil {
				return err
			}
			last := i == len(p.Bands)-1
			if b.UpTo == nil {
				if !last {
					return paramBad("only the last band may have no upper limit")
				}
				continue
			}
			if last {
				return paramBad("the last band must have up_to null so every amount falls in a band")
			}
			u, ok := parseDecimal(*b.UpTo)
			if !ok || u.Sign() == 0 {
				return paramBad("bands[%d].up_to %q must be a positive decimal string", i, *b.UpTo)
			}
			if prev != nil && u.Cmp(prev) <= 0 {
				return paramBad("band limits must be strictly increasing (bands[%d].up_to)", i)
			}
			prev = u
		}
		return nil
	default:
		return paramBad("family must be TAX_RATE or TAX_BANDS")
	}
}

// TaxResult is the explainable outcome of one calculation.
type TaxResult struct {
	Outcome       string   `json:"outcome"`
	Message       string   `json:"message,omitempty"`
	Family        string   `json:"family,omitempty"`
	TaxableAmount string   `json:"taxable_amount,omitempty"`
	TaxAmount     string   `json:"tax_amount,omitempty"`
	Unrounded     string   `json:"unrounded_tax,omitempty"`
	Rounding      Rounding `json:"rounding,omitempty"`
	Rounded       bool     `json:"rounded"`
	Steps         []string `json:"steps"`
}

// ratString renders a rational as a decimal with at most 12 fractional digits,
// trimmed of trailing zeros (an exact value when it terminates within them).
func ratString(r *big.Rat, scale int) string {
	return r.FloatString(scale)
}

func trimDecimal(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// roundRat rounds a non-negative rational to scale decimal places.
func roundRat(r *big.Rat, scale int, mode string) *big.Rat {
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow))
	num, den := scaled.Num(), scaled.Denom()
	q, rem := new(big.Int).QuoRem(num, den, new(big.Int)) // num, den > 0 here: truncation = floor
	if rem.Sign() != 0 {
		switch mode {
		case RoundUp:
			q.Add(q, big.NewInt(1))
		case RoundDown:
		default:
			twice := new(big.Int).Mul(rem, big.NewInt(2))
			cmp := twice.Cmp(den)
			if cmp > 0 || (cmp == 0 && (mode == RoundHalfUp || q.Bit(0) == 1)) {
				q.Add(q, big.NewInt(1))
			}
		}
	}
	return new(big.Rat).SetFrac(q, pow)
}

// CalculateTax applies parameters to a taxable amount (a non-negative decimal
// string). The arithmetic is exact; rounding happens once, at the end, to the
// declared scale and mode. Negative amounts are refused: a credit must be
// calculated on its own taxable basis, never by guessing a sign convention.
func CalculateTax(p RuleParameters, amount string) TaxResult {
	res := TaxResult{Family: p.Family, Rounding: p.Rounding, TaxableAmount: amount, Steps: []string{}}
	fail := func(outcome, msg string) TaxResult {
		res.Outcome, res.Message = outcome, msg
		res.Steps = append(res.Steps, outcome+": "+msg)
		return res
	}
	if err := p.Validate(); err != nil {
		return fail(CalcInvalidFacts, err.Error())
	}
	if ParameterOnlyFamily(p.Family) {
		return fail(CalcParameterOnly, "this rule is a statutory PARAMETER (a stated value, not a formula); it is read through the parameter interface, not calculated with")
	}
	amt, ok := parseDecimal(amount)
	if !ok {
		return fail(CalcInvalidFacts, "taxable_amount must be a non-negative decimal string with at most 12 decimal places (negative amounts are not supported)")
	}
	step := func(format string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(format, a...)) }

	total := new(big.Rat)
	switch p.Family {
	case FamilyTaxRate:
		rate, _ := parseDecimal(*p.Rate)
		total.Mul(amt, rate)
		step("%s x %s = %s", amount, *p.Rate, trimDecimal(ratString(total, 12)))
	default: // TAX_BANDS: marginal
		lower := new(big.Rat)
		for i, b := range p.Bands {
			rate, _ := parseDecimal(b.Rate)
			var upper *big.Rat
			if b.UpTo != nil {
				upper, _ = parseDecimal(*b.UpTo)
			}
			if amt.Cmp(lower) <= 0 {
				step("band %d (above %s): nothing in this band", i+1, trimDecimal(ratString(lower, 12)))
				break
			}
			top := amt
			if upper != nil && upper.Cmp(amt) < 0 {
				top = upper
			}
			slice := new(big.Rat).Sub(top, lower)
			part := new(big.Rat).Mul(slice, rate)
			total.Add(total, part)
			limit := "no limit"
			if b.UpTo != nil {
				limit = *b.UpTo
			}
			step("band %d (above %s up to %s): %s x %s = %s", i+1, trimDecimal(ratString(lower, 12)), limit,
				trimDecimal(ratString(slice, 12)), b.Rate, trimDecimal(ratString(part, 12)))
			if upper == nil || amt.Cmp(upper) <= 0 {
				break
			}
			lower = upper
		}
	}
	rounded := roundRat(total, p.Rounding.Scale, p.Rounding.Mode)
	res.Unrounded = trimDecimal(ratString(total, 12))
	res.TaxAmount = ratString(rounded, p.Rounding.Scale)
	res.Rounded = rounded.Cmp(total) != 0
	step("rounded once to %d decimal place(s), %s: %s", p.Rounding.Scale, p.Rounding.Mode, res.TaxAmount)
	res.Outcome = CalcCalculated
	return res
}

// ParameterClasses are the ZS-JUR-001 s17 payroll statutory parameter classes.
var ParameterClasses = []string{
	"INCOME_WITHHOLDING",     // income tax / withholding bands, thresholds, periodization inputs
	"EMPLOYEE_CONTRIBUTION",  // employee social security, insurance, pension statutory rates and caps
	"EMPLOYER_CONTRIBUTION",  // employer statutory rates, caps and levies
	"ALLOWANCE_OR_THRESHOLD", // allowances and thresholds
	"STATUTORY_PAY_LEAVE",    // statutory pay and leave eligibility and rate parameters
}

// ValidParameterClass reports whether c is a known s17 class.
func ValidParameterClass(c string) bool {
	for _, k := range ParameterClasses {
		if k == c {
			return true
		}
	}
	return false
}

// Units a fixed statutory amount can be expressed in. Periodization (turning an
// annual allowance into a pay-period figure) belongs to the payroll engine; the
// unit tells it what the number means.
var amountUnits = map[string]bool{"PER_DAY": true, "PER_WEEK": true, "PER_MONTH": true, "PER_YEAR": true, "PER_PAY_PERIOD": true, "LUMP_SUM": true}

func (p RuleParameters) validateAmount() error {
	if p.Rate != nil || len(p.Bands) != 0 || p.Profile != nil {
		return paramBad("AMOUNT does not take rate or bands")
	}
	if p.Rounding.Mode != "" || p.Rounding.Scale != 0 {
		return paramBad("AMOUNT carries no rounding: it is a stated value, not a calculation")
	}
	if p.Amount == nil {
		return paramBad("AMOUNT needs amount")
	}
	if _, ok := parseDecimal(*p.Amount); !ok {
		return paramBad("amount %q must be a non-negative decimal string (numbers are refused: JUR-NEG-25)", *p.Amount)
	}
	if !amountUnits[p.Unit] {
		return paramBad("unit must be PER_DAY, PER_WEEK, PER_MONTH, PER_YEAR, PER_PAY_PERIOD or LUMP_SUM")
	}
	return nil
}

// PayrollDomain is the rule_domain whose parameters the payroll interface serves
// (ZS-JUR-001 s17). Payroll CALCULATION stays with the payroll product; this
// service supplies versioned statutory parameters only.
const PayrollDomain = "PAYROLL"

// OutcomeParametersResolved is the outcome of a payroll statutory parameter
// set resolution that found at least one parameter and no conflict.
const OutcomeParametersResolved = "PARAMETERS_RESOLVED"
