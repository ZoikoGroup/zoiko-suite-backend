package domain

// REF-09 Units of Measure (ZS-SVC-I-001 s4.19).
//
// A quantity dimension has one base unit, and every other unit converts to it by an exact rational factor, versioned and
// effective-dated. Dimensionality is therefore checked before any arithmetic: a unit has a factor to its own dimension's base
// and to nothing else. Arithmetic is exact (math/big); a float never touches a quantity, because quantity-to-money
// calculations must be reproducible to the last digit. A conversion that is not a plain proportion, an offset or a context
// rule, is out of scope here and needs a specialised rule.

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

var (
	unitCodeShape      = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,19}$`)
	dimensionCodeShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,29}$`)
	decimalShape       = regexp.MustCompile(`^[+-]?(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	integerShape       = regexp.MustCompile(`^[0-9]+$`)
)

// Unit statuses: Draft (no conversion yet), Active, Retired.
const (
	UnitDraft   = "DRAFT"
	UnitActive  = "ACTIVE"
	UnitRetired = "RETIRED"
)

// Conversion source classes. STANDARD is a defined constant; the other two are non-standard and need independent approval.
const (
	ConvStandard     = "STANDARD"
	ConvOrganisation = "ORGANISATION"
	ConvOverride     = "MANUAL_OVERRIDE"
)

// Conversion version statuses.
const (
	ConvPending    = "PENDING"
	ConvActive     = "ACTIVE"
	ConvSuperseded = "SUPERSEDED"
)

// DefaultUnitDecimals is how many decimals a unit's quantities are kept to unless it says otherwise.
const DefaultUnitDecimals = 6

// MaxFactorDigits bounds a factor's numerator and denominator, so a conversion stays cheap and storable.
const MaxFactorDigits = 60

// MaxQuantityDigits bounds a quantity to convert.
const MaxQuantityDigits = 40

// NonStandard reports whether a source class needs independent approval.
func NonStandard(sourceClass string) bool { return sourceClass != ConvStandard }

// ValidConversionSource reports whether c is a known conversion source class.
func ValidConversionSource(c string) bool {
	return c == ConvStandard || c == ConvOrganisation || c == ConvOverride
}

// ParseFactor reads a conversion factor: a positive decimal ("1.609344") or fraction ("1609344/1000000"), exactly.
func ParseFactor(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	r := new(big.Rat)
	if strings.Contains(s, "/") {
		parts := strings.Split(s, "/")
		if len(parts) != 2 || !integerShape.MatchString(parts[0]) || !integerShape.MatchString(parts[1]) {
			return nil, fmt.Errorf("a fraction is two whole numbers, numerator/denominator")
		}
		if len(strings.TrimLeft(parts[0], "0")) > MaxFactorDigits || len(strings.TrimLeft(parts[1], "0")) > MaxFactorDigits {
			return nil, fmt.Errorf("a factor's numerator and denominator are at most %d digits", MaxFactorDigits)
		}
		if _, ok := r.SetString(s); !ok {
			return nil, fmt.Errorf("factor is not a valid fraction")
		}
	} else {
		if !decimalShape.MatchString(s) || len(s) > MaxFactorDigits+1 {
			return nil, fmt.Errorf("factor must be a decimal or a fraction of whole numbers")
		}
		if _, ok := r.SetString(s); !ok {
			return nil, fmt.Errorf("factor is not a valid decimal")
		}
	}
	if r.Sign() <= 0 {
		return nil, fmt.Errorf("a conversion factor is greater than zero")
	}
	if len(r.Num().String()) > MaxFactorDigits || len(r.Denom().String()) > MaxFactorDigits {
		return nil, fmt.Errorf("a factor reduces to at most %d digits in numerator and denominator", MaxFactorDigits)
	}
	return r, nil
}

// ParseQuantity reads an exact decimal quantity, which may be negative (an adjustment).
func ParseQuantity(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if !decimalShape.MatchString(s) {
		return nil, fmt.Errorf("quantity must be a decimal number")
	}
	if len(s) > MaxQuantityDigits {
		return nil, fmt.Errorf("quantity is too long")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("quantity is not a valid decimal")
	}
	return r, nil
}

// RoundHalfUp renders r to the given decimals, rounding half away from zero.
func RoundHalfUp(r *big.Rat, decimals int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	neg := r.Sign() < 0
	abs := new(big.Rat).Abs(r)
	n := new(big.Int).Mul(abs.Num(), scale)
	d := abs.Denom()
	n.Mul(n, big.NewInt(2))
	n.Add(n, d)
	n.Quo(n, new(big.Int).Mul(d, big.NewInt(2)))
	s := n.String()
	for len(s) <= decimals {
		s = "0" + s
	}
	whole, frac := s[:len(s)-decimals], s[len(s)-decimals:]
	out := whole
	if decimals > 0 {
		out += "." + frac
	}
	if neg && strings.Trim(out, "0.") != "" {
		out = "-" + out
	}
	return out
}

// ConvertExact is quantity x from / to, where from and to are factors to the base unit. Exact, never rounded.
func ConvertExact(quantity, from, to *big.Rat) *big.Rat {
	out := new(big.Rat).Mul(quantity, from)
	return out.Quo(out, to)
}

// QuantityDimension is a kind of measurable quantity: length, mass, volume, count.
type QuantityDimension struct {
	DimensionID   string    `json:"dimension_id"`
	TenantID      string    `json:"tenant_id"`
	DimensionCode string    `json:"dimension_code"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	BaseUnitCode  string    `json:"base_unit_code,omitempty"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// UnitFields are the attributes of a unit at creation.
type UnitFields struct {
	UnitCode       string `json:"unit_code"`
	Name           string `json:"name"`
	Symbol         string `json:"symbol"`
	Decimals       *int   `json:"decimals,omitempty"`
	SourceStandard string `json:"source_standard,omitempty"`
}

func (u UnitFields) validate() error {
	switch {
	case !unitCodeShape.MatchString(u.UnitCode):
		return fmt.Errorf("unit_code must be upper case letters, digits and underscore, starting with a letter (1 to 20 characters)")
	case strings.TrimSpace(u.Name) == "" || len(u.Name) > 200:
		return fmt.Errorf("name is required (1 to 200 characters)")
	case strings.TrimSpace(u.Symbol) == "" || len(u.Symbol) > 20:
		return fmt.Errorf("symbol is required (1 to 20 characters)")
	case u.Decimals != nil && (*u.Decimals < 0 || *u.Decimals > 18):
		return fmt.Errorf("decimals must be from 0 to 18")
	case len(u.SourceStandard) > 200:
		return fmt.Errorf("source_standard is too long")
	}
	return nil
}

// DecimalsOrDefault is the unit's rounding precision.
func (u UnitFields) DecimalsOrDefault() int {
	if u.Decimals == nil {
		return DefaultUnitDecimals
	}
	return *u.Decimals
}

// CreateQuantityDimensionRequest creates a dimension together with its base unit.
type CreateQuantityDimensionRequest struct {
	DimensionCode string     `json:"dimension_code"`
	Name          string     `json:"name"`
	Description   string     `json:"description,omitempty"`
	BaseUnit      UnitFields `json:"base_unit"`
	CorrelationID string     `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r CreateQuantityDimensionRequest) Validate() error {
	switch {
	case !dimensionCodeShape.MatchString(r.DimensionCode):
		return fmt.Errorf("dimension_code must be upper case letters, digits and underscore, starting with a letter (2 to 30 characters)")
	case strings.TrimSpace(r.Name) == "" || len(r.Name) > 200:
		return fmt.Errorf("name is required (1 to 200 characters)")
	case len(r.Description) > 1000:
		return fmt.Errorf("description is too long")
	}
	if err := r.BaseUnit.validate(); err != nil {
		return fmt.Errorf("base_unit: %v", err)
	}
	return nil
}

// CreateUnitRequest is CreateUnit: a non-base unit of an existing dimension. It starts DRAFT until its conversion is approved.
type CreateUnitRequest struct {
	DimensionCode string `json:"dimension_code"`
	UnitFields
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r CreateUnitRequest) Validate() error {
	if !dimensionCodeShape.MatchString(r.DimensionCode) {
		return fmt.Errorf("dimension_code names a quantity dimension")
	}
	return r.UnitFields.validate()
}

// Unit is a unit of measure.
type Unit struct {
	UnitID         string    `json:"unit_id"`
	TenantID       string    `json:"tenant_id"`
	DimensionCode  string    `json:"dimension_code"`
	UnitCode       string    `json:"unit_code"`
	Name           string    `json:"name"`
	Symbol         string    `json:"symbol"`
	IsBase         bool      `json:"is_base"`
	Status         string    `json:"status"`
	Decimals       int       `json:"decimals"`
	SourceStandard string    `json:"source_standard,omitempty"`
	RecordVersion  int64     `json:"record_version"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	RetiredOn      string    `json:"retired_on,omitempty"`
	RetireReason   string    `json:"retire_reason,omitempty"`
	RetiredBy      string    `json:"retired_by,omitempty"`
}

// ConversionVersion is one version of the factor taking a unit to its dimension's base unit.
type ConversionVersion struct {
	VersionNo         int        `json:"version_no"`
	Factor            string     `json:"factor"` // the exact reduced fraction, numerator/denominator
	FactorDecimal     string     `json:"factor_decimal"`
	SourceClass       string     `json:"source_class"`
	SourceRef         string     `json:"source_ref"`
	Status            string     `json:"status"`
	ValidFrom         string     `json:"valid_from"`
	ValidTo           string     `json:"valid_to,omitempty"`
	SupersedesVersion int        `json:"supersedes_version,omitempty"`
	Reason            string     `json:"reason,omitempty"`
	EvidenceRef       string     `json:"evidence_ref"`
	ProposedBy        string     `json:"proposed_by"`
	ProposedAt        time.Time  `json:"proposed_at"`
	ApprovedBy        string     `json:"approved_by,omitempty"`
	ApprovedAt        *time.Time `json:"approved_at,omitempty"`
}

// UnitView is a unit as it was on a date: the conversion in force then and whether it could be converted.
type UnitView struct {
	Unit       Unit               `json:"unit"`
	AsOf       string             `json:"as_of"`
	Conversion *ConversionVersion `json:"conversion"`
	// Usable says whether a conversion involving the unit is possible on the date; Reason says why not.
	Usable bool   `json:"usable"`
	Reason string `json:"reason,omitempty"`
	// Pending is a proposed conversion awaiting approval.
	Pending  *ConversionVersion  `json:"pending_conversion,omitempty"`
	Versions []ConversionVersion `json:"conversion_versions,omitempty"`
}

// ProposeConversionRequest is a proposed conversion version: for the first conversion of a unit, or (via supersede) the
// replacement of the active one.
type ProposeConversionRequest struct {
	// Factor is how many base units one of this unit is: a decimal or a fraction, exact.
	ExpectedVersion int64  `json:"expected_version"`
	Factor          string `json:"factor"`
	SourceClass     string `json:"source_class"`
	SourceRef       string `json:"source_ref"`
	ValidFrom       string `json:"valid_from,omitempty"`
	Reason          string `json:"reason,omitempty"`
	EvidenceRef     string `json:"evidence_ref"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r ProposeConversionRequest) Validate() error {
	if _, err := ParseFactor(r.Factor); err != nil {
		return err
	}
	switch {
	case !ValidConversionSource(r.SourceClass):
		return fmt.Errorf("source_class must be STANDARD, ORGANISATION or MANUAL_OVERRIDE")
	case strings.TrimSpace(r.SourceRef) == "" || len(r.SourceRef) > 300:
		return fmt.Errorf("source_ref is required: name the standard or the document behind the factor")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	case r.SourceClass == ConvOverride && strings.TrimSpace(r.Reason) == "":
		return fmt.Errorf("a manual override states why")
	}
	if r.ValidFrom != "" {
		if _, err := ParseDate(r.ValidFrom); err != nil {
			return fmt.Errorf("valid_from %v", err)
		}
	}
	return nil
}

// ApproveConversionRequest approves a pending conversion version.
type ApproveConversionRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	EvidenceRef     string `json:"evidence_ref"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r ApproveConversionRequest) Validate() error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	return nil
}

// RetireUnitRequest is RetireUnit.
type RetireUnitRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason"`
	EvidenceRef     string `json:"evidence_ref"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r RetireUnitRequest) Validate() error {
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	return nil
}

// ConvertQuantityQuery is ConvertQuantity.
type ConvertQuantityQuery struct {
	Quantity string
	From     string
	To       string
	AsOf     time.Time
}

// Validate checks the query.
func (q ConvertQuantityQuery) Validate() error {
	if _, err := ParseQuantity(q.Quantity); err != nil {
		return err
	}
	if !unitCodeShape.MatchString(q.From) || !unitCodeShape.MatchString(q.To) {
		return fmt.Errorf("from and to are unit codes")
	}
	return nil
}

// ConversionPin names the conversion version a result applied, so the calling domain can retain it with the business fact:
// if a conversion changes over time, the applied version is what reproduces the number.
type ConversionPin struct {
	UnitCode      string `json:"unit_code"`
	IsBase        bool   `json:"is_base"`
	VersionNo     int    `json:"version_no,omitempty"`
	Factor        string `json:"factor"`
	FactorDecimal string `json:"factor_decimal"`
	SourceClass   string `json:"source_class,omitempty"`
	SourceRef     string `json:"source_ref,omitempty"`
	ValidFrom     string `json:"valid_from,omitempty"`
}

// ConversionResult is a conversion with its exact value, its rounded value and the versions it applied.
type ConversionResult struct {
	Dimension string        `json:"dimension"`
	AsOf      string        `json:"as_of"`
	Input     string        `json:"input_quantity"`
	FromUnit  string        `json:"from_unit"`
	ToUnit    string        `json:"to_unit"`
	Value     string        `json:"value"`          // rounded to the target unit's decimals
	Exact     string        `json:"exact_fraction"` // unrounded, a reduced numerator/denominator
	Decimals  int           `json:"decimals"`
	Rounding  string        `json:"rounding"`
	From      ConversionPin `json:"from"`
	To        ConversionPin `json:"to"`
}

// UnitRounding states how a result is rounded to the target unit's precision.
const UnitRounding = "HALF_AWAY_FROM_ZERO at the target unit's decimals"
