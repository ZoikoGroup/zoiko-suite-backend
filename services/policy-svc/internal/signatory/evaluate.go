package signatory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"zoiko.io/identity-envelope/govdecision"
)

// EvaluatorVersion names this evaluator and its contract version; it is stored
// with every decision and test run.
const EvaluatorVersion = "policy-svc/SIGNATORY_MATRIX@1"

// Owners of the required actions a resolution hands out.
const (
	OwnerWorkflow      = "workflow-svc"
	OwnerAuthorization = "authorization-svc"
	OwnerLegal         = "legal-review"
	OwnerTreasury      = "payment-execution-owner"
)

var documentHash = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Commitment is one related agreement or amendment of the same series,
// aggregated so a commitment cannot be split under a band (Doc 01 §5.4).
type Commitment struct {
	ResourceID string `json:"resource_id"`
	Amount     Amount `json:"amount"`
	Currency   string `json:"currency"`
}

// Converted is the commitment's value in the matrix currency, as valued by
// its owner (Spend Controls owns FX: Doc 02 §6.2). It is used only with the
// owner's FX snapshot reference; a missing rate never defaults to 1.0.
type Converted struct {
	Amount        Amount `json:"amount"`
	Currency      string `json:"currency"`
	FXSnapshotRef string `json:"fx_snapshot_ref"`
}

// Input is what a resolution decides on: the evaluator's action context.
type Input struct {
	InstrumentType      string       `json:"instrument_type"`
	Amount              Amount       `json:"amount"`
	Currency            string       `json:"currency"`
	Basis               string       `json:"basis"`
	OptionsAmount       *Amount      `json:"options_amount,omitempty"`
	Converted           *Converted   `json:"converted,omitempty"`
	RelatedCommitments  []Commitment `json:"related_commitments,omitempty"`
	RiskFlags           []string     `json:"risk_flags,omitempty"`
	DocumentHash        string       `json:"document_hash,omitempty"`
	PreparerPrincipalID string       `json:"preparer_principal_id,omitempty"`
}

// Amount is a decimal given as a JSON string or an exact JSON number.
type Amount string

// UnmarshalJSON keeps the decimal's exact text.
func (a *Amount) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*a = Amount(s)
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("amount must be a decimal string or number")
	}
	*a = Amount(n.String())
	return nil
}

// InputError is a problem with the request, naming the field.
type InputError struct {
	Field  string
	Reason string // missing | invalid
	Detail string
}

func (e *InputError) Error() string { return e.Field + ": " + e.Reason + " " + e.Detail }

func missing(field string) error { return &InputError{Field: field, Reason: "missing"} }
func bad(field, detail string) error {
	return &InputError{Field: field, Reason: "invalid", Detail: detail}
}

// ParseInput decodes and checks an action context.
func ParseInput(raw []byte) (*Input, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, missing("action_context.instrument_type")
	}
	var in Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, bad("action_context", err.Error())
	}
	switch {
	case in.InstrumentType == "":
		return nil, missing("action_context.instrument_type")
	case !identifier.MatchString(in.InstrumentType):
		return nil, bad("action_context.instrument_type", "must be an upper-case identifier")
	case in.Amount == "":
		return nil, missing("action_context.amount")
	case in.Currency == "":
		return nil, missing("action_context.currency")
	case !isCurrency(in.Currency):
		return nil, bad("action_context.currency", "must be an ISO 4217 code")
	case in.Basis == "":
		return nil, missing("action_context.basis")
	case in.Basis != govdecision.BasisGross && in.Basis != govdecision.BasisNet:
		return nil, bad("action_context.basis", "must be GROSS or NET")
	case in.DocumentHash != "" && !documentHash.MatchString(in.DocumentHash):
		return nil, bad("action_context.document_hash", "must be sha256:<64 hex>")
	}
	if _, err := Decimal(string(in.Amount)); err != nil {
		return nil, bad("action_context.amount", err.Error())
	}
	if in.OptionsAmount != nil {
		if _, err := Decimal(string(*in.OptionsAmount)); err != nil {
			return nil, bad("action_context.options_amount", err.Error())
		}
	}
	if c := in.Converted; c != nil {
		switch {
		case !isCurrency(c.Currency):
			return nil, bad("action_context.converted.currency", "must be an ISO 4217 code")
		case strings.TrimSpace(c.FXSnapshotRef) == "":
			return nil, missing("action_context.converted.fx_snapshot_ref")
		case in.OptionsAmount != nil:
			return nil, bad("action_context.options_amount", "a converted amount is the total committed value, options included")
		}
		if _, err := Decimal(string(c.Amount)); err != nil {
			return nil, bad("action_context.converted.amount", err.Error())
		}
	}
	for i, rc := range in.RelatedCommitments {
		field := fmt.Sprintf("action_context.related_commitments[%d]", i)
		if strings.TrimSpace(rc.ResourceID) == "" {
			return nil, missing(field + ".resource_id")
		}
		if !isCurrency(rc.Currency) {
			return nil, bad(field+".currency", "must be an ISO 4217 code")
		}
		if _, err := Decimal(string(rc.Amount)); err != nil {
			return nil, bad(field+".amount", err.Error())
		}
	}
	for _, f := range in.RiskFlags {
		if !identifier.MatchString(f) {
			return nil, bad("action_context.risk_flags", fmt.Sprintf("%q is not an upper-case identifier", f))
		}
	}
	return &in, nil
}

// HighRisk reports whether the request needs an instrument-specific matrix.
func (in *Input) HighRisk() bool {
	if HighRiskInstruments[in.InstrumentType] {
		return true
	}
	for _, f := range in.RiskFlags {
		if HighRiskFlags[f] {
			return true
		}
	}
	return false
}

// Detail is the non-authoritative context of a resolution.
type Detail struct {
	BandID           string `json:"band_id,omitempty"`
	ExemptionID      string `json:"exemption_id,omitempty"`
	InstrumentType   string `json:"instrument_type"`
	ComparedAmount   string `json:"compared_amount,omitempty"`
	Currency         string `json:"currency,omitempty"`
	ValuationBasis   string `json:"valuation_basis,omitempty"`
	AggregatedSeries bool   `json:"aggregated_series,omitempty"`
	HighRisk         bool   `json:"high_risk"`
}

// Result is one resolution.
type Result struct {
	Outcome         string
	ReasonCodes     []string
	RuleIDs         []string
	RequiredActions []govdecision.RequiredAction
	UpstreamRefs    []string
	Detail          Detail
}

func notConfigured(versionID, reason string, d Detail) Result {
	return Result{Outcome: govdecision.NotConfigured, ReasonCodes: []string{reason}, RuleIDs: []string{versionID + "#not_configured"}, Detail: d}
}

// Evaluate resolves one request under a matrix. versionID labels the rules.
func Evaluate(versionID string, m *Matrix, in *Input) Result {
	highRisk := in.HighRisk()
	d := Detail{InstrumentType: in.InstrumentType, HighRisk: highRisk, Currency: m.Currency, ValuationBasis: m.ValuationBasis}
	switch {
	case highRisk && !m.HighRisk:
		return notConfigured(versionID, govdecision.ReasonConfigInstrumentMatrixMissing, d)
	case !highRisk && m.HighRisk:
		// A high-risk matrix governs only high-risk requests.
		return notConfigured(versionID, govdecision.ReasonConfigPolicyMissing, d)
	case !m.Covers(in.InstrumentType, highRisk):
		return notConfigured(versionID, govdecision.ReasonConfigPolicyMissing, d)
	case in.Basis != m.ValuationBasis:
		// Never silently mix bases (Doc 01 §4.2).
		return notConfigured(versionID, govdecision.ReasonConfigValuationBasisMismatch, d)
	}

	rules := []string{versionID + "#instrument:" + in.InstrumentType, versionID + "#currency:" + m.Currency}
	var upstream []string
	var exposure *big.Rat
	switch {
	case in.Currency == m.Currency:
		exposure, _ = Decimal(string(in.Amount))
		if in.OptionsAmount != nil {
			opt, _ := Decimal(string(*in.OptionsAmount))
			exposure = new(big.Rat).Add(exposure, opt)
		}
	case in.Converted != nil && in.Converted.Currency == m.Currency:
		exposure, _ = Decimal(string(in.Converted.Amount))
		upstream = append(upstream, "fx:"+in.Converted.FXSnapshotRef)
	default:
		// policy-svc does no FX; the owner's valuation is needed.
		return notConfigured(versionID, govdecision.ReasonConfigCurrencyUnsupported, d)
	}

	reasons := []string{}
	if m.aggregates() && len(in.RelatedCommitments) > 0 {
		for _, rc := range in.RelatedCommitments {
			if rc.Currency != m.Currency {
				return notConfigured(versionID, govdecision.ReasonConfigCurrencyUnsupported, d)
			}
			v, _ := Decimal(string(rc.Amount))
			exposure = new(big.Rat).Add(exposure, v)
			upstream = append(upstream, "series:"+rc.ResourceID)
		}
		d.AggregatedSeries = true
		reasons = append(reasons, govdecision.ReasonSignatureSeriesAggregated)
		rules = append(rules, versionID+"#aggregate_series")
	}
	d.ComparedAmount = exposure.FloatString(scale(exposure))

	for _, e := range m.Exemptions {
		if e.InstrumentType == in.InstrumentType && !highRisk && (e.upTo == nil || exposure.Cmp(e.upTo) <= 0) {
			d.ExemptionID = e.ExemptionID
			return Result{
				Outcome:      govdecision.Permit,
				ReasonCodes:  append([]string{govdecision.ReasonSignatureExempt}, reasons...),
				RuleIDs:      append(rules, versionID+"#exemption:"+e.ExemptionID),
				UpstreamRefs: upstream,
				Detail:       d,
			}
		}
	}

	var band *Band
	for i := range m.Bands {
		b := &m.Bands[i]
		if (b.above == nil || exposure.Cmp(b.above) > 0) && (b.upTo == nil || exposure.Cmp(b.upTo) <= 0) {
			band = b
			break
		}
	}
	if band == nil {
		return notConfigured(versionID, govdecision.ReasonConfigBandMissing, d)
	}
	d.BandID = band.BandID
	rules = append(rules, versionID+"#band:"+band.BandID)

	constraints := []string{
		govdecision.ConstraintDistinctHumans, govdecision.ConstraintPreparerExcluded,
		govdecision.ConstraintAuthorityAtSigning, govdecision.ConstraintDocumentHashBound,
		govdecision.ConstraintLegalEntityScoped, govdecision.ConstraintNoImplicitDelegate,
	}
	if band.DistinctDepartments {
		constraints = append(constraints, govdecision.ConstraintDistinctDepartments)
	}
	offices := make([]govdecision.OfficeRequirement, 0, len(band.RequiredOffices))
	for _, o := range band.RequiredOffices {
		offices = append(offices, govdecision.OfficeRequirement{Office: o.Office, MinCount: o.MinCount})
	}
	actions := []govdecision.RequiredAction{
		{
			Kind: govdecision.ActionKindSignatureQuorum, Owner: OwnerWorkflow,
			MinDistinctSigners: band.MinDistinctSigners, RequiredOffices: offices,
			EligibleOffices: append([]string(nil), band.EligibleOffices...),
			Sequencing:      band.Sequencing, Order: append([]string(nil), band.Order...),
			Constraints: constraints, DocumentHash: in.DocumentHash,
		},
		{
			Kind: govdecision.ActionKindSignerEligibility, Owner: OwnerAuthorization,
			Detail: "each signer's office, legal-entity delegation, risk restrictions and segregation of duties are verified at the moment of signing",
		},
	}
	reasons = append([]string{govdecision.ReasonSignatureQuorumIncomplete}, reasons...)
	if highRisk {
		reasons = append(reasons, govdecision.ReasonSignatureHighRiskInstrument)
	}
	if band.LegalReview || intersects(m.LegalReviewRiskFlags, in.RiskFlags) {
		actions = append(actions, govdecision.RequiredAction{
			Kind: govdecision.ActionKindLegalReview, Owner: OwnerLegal,
			Detail: "legal review is mandatory before signature",
		})
		reasons = append(reasons, govdecision.ReasonSignatureLegalReview)
	}
	if band.SeparateReleaseAuthorization || in.InstrumentType == "PAYMENT_INSTRUCTION" {
		actions = append(actions, govdecision.RequiredAction{
			Kind: govdecision.ActionKindReleaseAuthorization, Owner: OwnerTreasury,
			Constraints: []string{govdecision.ConstraintNotProxyForBankMandate, govdecision.ConstraintNoImplicitDelegate},
			Detail:      "release needs separate treasury authorization and a verified bank mandate; an approval or signature is not a bank mandate",
		})
		reasons = append(reasons, govdecision.ReasonSignatureReleaseMandate)
	}
	return Result{
		Outcome:         govdecision.SignatureRequired,
		ReasonCodes:     reasons,
		RuleIDs:         rules,
		RequiredActions: actions,
		UpstreamRefs:    upstream,
		Detail:          d,
	}
}

// scale is the number of decimal places needed to print r exactly (up to 8).
func scale(r *big.Rat) int {
	for p := 0; p <= 8; p++ {
		x := new(big.Rat).Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(p)), nil)))
		if x.IsInt() {
			if p < 2 {
				return 2
			}
			return p
		}
	}
	return 8
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// CoversPayload reports whether a stored payload governs the request. A
// payload that does not parse is reported as covering, so it is selected and
// its resolution answers NOT_CONFIGURED (CONFIG_POLICY_INVALID) visibly,
// instead of disappearing from the candidate set.
func CoversPayload(payload []byte, instrument string, highRisk bool) bool {
	m, err := ParseMatrix(payload)
	if err != nil {
		return true
	}
	return m.Covers(instrument, highRisk)
}
