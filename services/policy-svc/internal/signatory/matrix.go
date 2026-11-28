// Package signatory is the SIGNATORY_MATRIX policy family (ZS-GOV-POL-001 §5,
// ZS-GOV-INT-002 §4): the versioned rule that says how many distinct human
// signers, holding which corporate offices, an instrument of a given value
// needs before it may be signed or released.
//
// policy-svc owns the definition and its resolution only. It never decides
// whether a named person may sign: authorization-svc validates each signer's
// office, delegation and segregation of duties at the moment of signing,
// workflow-svc collects the distinct signers and quorum, and the document or
// payment owner binds the signature to the exact document hash (Doc 02 §4.2).
// So the outcome of a resolution is SIGNATURE_REQUIRED with the requirement,
// PERMIT only for an explicit exemption rule, or NOT_CONFIGURED — never PERMIT
// because zero signatures are recorded.
//
// Everything here is a pure function of a version's payload and the request,
// so live resolution, certification test runs and simulation agree.
package signatory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"zoiko.io/identity-envelope/govdecision"
)

// PolicyType is the policy_type of a signatory matrix.
const PolicyType = "SIGNATORY_MATRIX"

// Sequencing of a band's signatures.
const (
	SequencingParallel   = "PARALLEL"
	SequencingSequential = "SEQUENTIAL"
)

// Limits on a matrix.
const (
	DefaultDecisionTTL = 15 * time.Minute
	MinDecisionTTL     = time.Minute
	MaxDecisionTTL     = 24 * time.Hour
	MaxSigners         = 10
	MaxBands           = 32
)

// HighRiskInstruments need an instrument-specific matrix (high_risk: true),
// even below a standard band (Doc 01 §5.4). The set is a system-mandatory
// control: a tenant matrix can add instruments to a high-risk matrix but can
// never take one of these out of the high-risk regime.
var HighRiskInstruments = map[string]bool{
	"GUARANTEE":              true,
	"CREDIT_FACILITY":        true,
	"LENDING":                true,
	"EQUITY":                 true,
	"LITIGATION_SETTLEMENT":  true,
	"RELATED_PARTY_CONTRACT": true,
}

// HighRiskFlags on a request make any instrument high-risk.
var HighRiskFlags = map[string]bool{"RELATED_PARTY": true, "HIGH_RISK": true}

var identifier = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)

// ErrInvalidMatrix wraps every reason a payload is not a valid matrix.
var ErrInvalidMatrix = errors.New("invalid signatory matrix")

// OfficeRequirement: at least MinCount distinct signers holding Office.
type OfficeRequirement struct {
	Office   string `json:"office"`
	MinCount int    `json:"min_count"`
}

// Band is one non-overlapping amount band: above Above (exclusive; null means
// from zero inclusive) up to UpTo (inclusive; null means unbounded).
type Band struct {
	BandID                       string              `json:"band_id"`
	Above                        *string             `json:"above"`
	UpTo                         *string             `json:"up_to"`
	MinDistinctSigners           int                 `json:"min_distinct_signers"`
	RequiredOffices              []OfficeRequirement `json:"required_offices"`
	EligibleOffices              []string            `json:"eligible_offices,omitempty"`
	Sequencing                   string              `json:"sequencing,omitempty"`
	Order                        []string            `json:"order,omitempty"`
	DistinctDepartments          bool                `json:"distinct_departments,omitempty"`
	LegalReview                  bool                `json:"legal_review,omitempty"`
	SeparateReleaseAuthorization bool                `json:"separate_release_authorization,omitempty"`

	above, upTo *big.Rat
}

// Exemption is an explicit rule that an instrument needs no signature (up to
// an optional amount). It is the only way a resolution answers PERMIT.
type Exemption struct {
	ExemptionID    string  `json:"exemption_id"`
	InstrumentType string  `json:"instrument_type"`
	UpTo           *string `json:"up_to,omitempty"`
	Reason         string  `json:"reason"`

	upTo *big.Rat
}

// Matrix is a SIGNATORY_MATRIX version's rule_payload.
type Matrix struct {
	Currency             string      `json:"currency"`
	ValuationBasis       string      `json:"valuation_basis"`
	InstrumentTypes      []string    `json:"instrument_types"`
	HighRisk             bool        `json:"high_risk,omitempty"`
	DecisionTTLSeconds   int         `json:"decision_ttl_seconds,omitempty"`
	AggregateSeries      *bool       `json:"aggregate_series,omitempty"`
	LegalReviewRiskFlags []string    `json:"legal_review_risk_flags,omitempty"`
	Bands                []Band      `json:"bands"`
	Exemptions           []Exemption `json:"exemptions,omitempty"`
}

// TTL is how long a decision under this matrix stays valid.
func (m *Matrix) TTL() time.Duration {
	if m.DecisionTTLSeconds == 0 {
		return DefaultDecisionTTL
	}
	return time.Duration(m.DecisionTTLSeconds) * time.Second
}

func (m *Matrix) aggregates() bool { return m.AggregateSeries == nil || *m.AggregateSeries }

// Overlaps reports the first instrument two matrices would both govern in the
// same risk regime: activating both at one scope and period would make a
// request ambiguous (Doc 01 §3.2), so it is refused at activation.
func (m *Matrix) Overlaps(other *Matrix) (string, bool) {
	if m.HighRisk != other.HighRisk {
		return "", false
	}
	for _, a := range m.InstrumentTypes {
		for _, b := range other.InstrumentTypes {
			if a == b {
				return a, true
			}
		}
	}
	return "", false
}

// Covers reports whether the matrix governs instrument in the request's risk
// regime: a high-risk request only by a high-risk matrix, an ordinary request
// only by an ordinary one. So an instrument-specific high-risk matrix and the
// ordinary matrix can list the same instrument without ever competing.
func (m *Matrix) Covers(instrument string, highRisk bool) bool {
	if m.HighRisk != highRisk {
		return false
	}
	for _, t := range m.InstrumentTypes {
		if t == instrument {
			return true
		}
	}
	return false
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMatrix, fmt.Sprintf(format, args...))
}

// Decimal parses an exact non-negative decimal; floats never touch money.
func Decimal(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "eE/") {
		return nil, fmt.Errorf("%q is not a plain decimal", s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("%q is not a decimal", s)
	}
	if r.Sign() < 0 {
		return nil, fmt.Errorf("%q is negative", s)
	}
	return r, nil
}

func isCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// ParseMatrix decodes and validates a payload: every rule a resolution depends
// on is checked at authoring time, so a version that could never resolve
// cleanly is refused before it can be approved.
func ParseMatrix(payload []byte) (*Matrix, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var m Matrix
	if err := dec.Decode(&m); err != nil {
		return nil, invalid("%v", err)
	}
	if !isCurrency(m.Currency) {
		return nil, invalid("currency must be an ISO 4217 code (three upper-case letters)")
	}
	if m.ValuationBasis != govdecision.BasisGross && m.ValuationBasis != govdecision.BasisNet {
		return nil, invalid("valuation_basis must be GROSS or NET")
	}
	if m.DecisionTTLSeconds != 0 {
		ttl := time.Duration(m.DecisionTTLSeconds) * time.Second
		if ttl < MinDecisionTTL || ttl > MaxDecisionTTL {
			return nil, invalid("decision_ttl_seconds must be between %d and %d", int(MinDecisionTTL.Seconds()), int(MaxDecisionTTL.Seconds()))
		}
	}
	if len(m.InstrumentTypes) == 0 {
		return nil, invalid("instrument_types must list at least one instrument")
	}
	seen := map[string]bool{}
	for _, t := range m.InstrumentTypes {
		if !identifier.MatchString(t) || seen[t] {
			return nil, invalid("instrument_types entry %q is malformed or repeated", t)
		}
		seen[t] = true
		if HighRiskInstruments[t] && !m.HighRisk {
			return nil, invalid("%s is a high-risk instrument: it needs its own matrix with high_risk true", t)
		}
	}
	for _, f := range m.LegalReviewRiskFlags {
		if !identifier.MatchString(f) {
			return nil, invalid("legal_review_risk_flags entry %q is malformed", f)
		}
	}
	if err := m.validateBands(); err != nil {
		return nil, err
	}
	if err := m.validateExemptions(seen); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Matrix) validateBands() error {
	if len(m.Bands) == 0 || len(m.Bands) > MaxBands {
		return invalid("bands must hold between 1 and %d bands", MaxBands)
	}
	ids := map[string]bool{}
	for i := range m.Bands {
		b := &m.Bands[i]
		if strings.TrimSpace(b.BandID) == "" || ids[b.BandID] {
			return invalid("band %d: band_id is missing or repeated", i)
		}
		ids[b.BandID] = true
		var err error
		if b.Above != nil {
			if b.above, err = Decimal(*b.Above); err != nil {
				return invalid("band %s: above %v", b.BandID, err)
			}
		}
		if b.UpTo != nil {
			if b.upTo, err = Decimal(*b.UpTo); err != nil {
				return invalid("band %s: up_to %v", b.BandID, err)
			}
		}
		if b.above != nil && b.upTo != nil && b.upTo.Cmp(b.above) <= 0 {
			return invalid("band %s: up_to must be greater than above", b.BandID)
		}
		if err := b.validateSigners(); err != nil {
			return err
		}
	}
	// Order by lower bound: the band from zero first.
	sort.SliceStable(m.Bands, func(i, j int) bool {
		a, b := m.Bands[i].above, m.Bands[j].above
		switch {
		case a == nil:
			return b != nil
		case b == nil:
			return false
		default:
			return a.Cmp(b) < 0
		}
	})
	if m.Bands[0].above != nil {
		return invalid("the first band must start at zero (above: null)")
	}
	for i := 1; i < len(m.Bands); i++ {
		prev, cur := m.Bands[i-1], m.Bands[i]
		switch {
		case cur.above == nil:
			return invalid("bands %s and %s both start at zero: bands must not overlap", prev.BandID, cur.BandID)
		case prev.upTo == nil:
			return invalid("band %s is unbounded but band %s follows it: only the last band may be unbounded", prev.BandID, cur.BandID)
		case cur.above.Cmp(prev.upTo) < 0:
			return invalid("bands %s and %s overlap", prev.BandID, cur.BandID)
		case cur.above.Cmp(prev.upTo) > 0:
			return invalid("bands %s and %s leave a gap: band %s must start above %s", prev.BandID, cur.BandID, cur.BandID, *prev.UpTo)
		}
		// A higher band never needs fewer signers: no evasion by amount.
		if cur.MinDistinctSigners < prev.MinDistinctSigners {
			return invalid("band %s needs fewer signers than band %s below it", cur.BandID, prev.BandID)
		}
	}
	return nil
}

func (b *Band) validateSigners() error {
	if b.MinDistinctSigners < 1 || b.MinDistinctSigners > MaxSigners {
		return invalid("band %s: min_distinct_signers must be between 1 and %d", b.BandID, MaxSigners)
	}
	required := 0
	offices := map[string]bool{}
	for _, o := range b.RequiredOffices {
		if !identifier.MatchString(o.Office) || offices[o.Office] || o.MinCount < 1 {
			return invalid("band %s: required_offices entry %q is malformed, repeated or has min_count < 1", b.BandID, o.Office)
		}
		offices[o.Office] = true
		required += o.MinCount
	}
	if required > b.MinDistinctSigners {
		return invalid("band %s: required_offices need %d signers but min_distinct_signers is %d", b.BandID, required, b.MinDistinctSigners)
	}
	eligible := map[string]bool{}
	for _, o := range b.EligibleOffices {
		if !identifier.MatchString(o) || eligible[o] {
			return invalid("band %s: eligible_offices entry %q is malformed or repeated", b.BandID, o)
		}
		eligible[o] = true
	}
	for o := range offices {
		if len(b.EligibleOffices) > 0 && !eligible[o] {
			return invalid("band %s: required office %s is not in eligible_offices", b.BandID, o)
		}
	}
	if required < b.MinDistinctSigners && len(b.EligibleOffices) == 0 {
		return invalid("band %s: eligible_offices must name who fills the %d seat(s) no required office covers", b.BandID, b.MinDistinctSigners-required)
	}
	switch b.Sequencing {
	case "":
		b.Sequencing = SequencingParallel
		if len(b.Order) > 0 {
			return invalid("band %s: order is only valid with sequencing SEQUENTIAL", b.BandID)
		}
	case SequencingParallel:
		if len(b.Order) > 0 {
			return invalid("band %s: order is only valid with sequencing SEQUENTIAL", b.BandID)
		}
	case SequencingSequential:
		if len(b.Order) == 0 {
			return invalid("band %s: sequencing SEQUENTIAL needs an order", b.BandID)
		}
		for _, o := range b.Order {
			if !offices[o] && !eligible[o] {
				return invalid("band %s: order names %s, which the band does not require or allow", b.BandID, o)
			}
		}
	default:
		return invalid("band %s: sequencing must be PARALLEL or SEQUENTIAL", b.BandID)
	}
	return nil
}

func (m *Matrix) validateExemptions(instruments map[string]bool) error {
	ids := map[string]bool{}
	for i := range m.Exemptions {
		e := &m.Exemptions[i]
		switch {
		case strings.TrimSpace(e.ExemptionID) == "" || ids[e.ExemptionID]:
			return invalid("exemption %d: exemption_id is missing or repeated", i)
		case !instruments[e.InstrumentType]:
			return invalid("exemption %s: instrument_type %q is not one of the matrix's instrument_types", e.ExemptionID, e.InstrumentType)
		case HighRiskInstruments[e.InstrumentType] || m.HighRisk:
			return invalid("exemption %s: a high-risk instrument can never be exempt from signature", e.ExemptionID)
		case strings.TrimSpace(e.Reason) == "":
			return invalid("exemption %s: reason is required", e.ExemptionID)
		}
		ids[e.ExemptionID] = true
		if e.UpTo != nil {
			v, err := Decimal(*e.UpTo)
			if err != nil {
				return invalid("exemption %s: up_to %v", e.ExemptionID, err)
			}
			e.upTo = v
		}
	}
	return nil
}
