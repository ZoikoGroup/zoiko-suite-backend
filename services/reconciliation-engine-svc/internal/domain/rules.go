package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Rule versions (A11) — the pattern of financial-control-svc's control rule
// versions: a definition's rules are an append-only series of immutable
// versions. A version is created as a draft (rule publication), approved by
// someone other than its creator under a separate permission (DATA-07
// "configuration/rule publication separated from evaluation/release
// approval"; ZS-CONTROL-001 Invariant 4 "rules … versioned and pinned to the
// run"), and governs runs from its effective date. A new version must name
// the version it is based on (DATA-07 "optimistic version checks on …
// rule … changes").

const PrefixRuleVersion = "rrv_"

const (
	ErrNoEffectiveRules    = errorString("the definition has no approved rule version in effect; create and approve one")
	ErrDefinitionExists    = errorString("a definition with this name already exists in the legal entity; publish a new rule version instead")
	ErrRuleVersionConflict = errorString("the definition has a newer rule version than the one this change is based on")
	ErrRuleVersionNotFound = errorString("rule version not found")
	ErrRuleVersionApproved = errorString("the rule version is already approved and immutable")
	ErrSelfApproval        = errorString("the creator of a rule version cannot approve it")
	// ErrBackdatedRuleVersion: a version dated before the version in effect could
	// never take effect (the latest effective date governs), so it is refused.
	ErrBackdatedRuleVersion = errorString("effective_from is before the effective date of the rule version in effect")
)

// RuleSet is one complete, self-contained rule document. Every field a run
// pins comes from here.
type RuleSet struct {
	Populations          []PopulationSpec     `json:"populations"`
	MatchKeyFields       []string             `json:"match_key_fields"`
	BalanceDimensions    []string             `json:"balance_dimensions"`
	AmountScale          int                  `json:"amount_scale"`
	Tolerance            TolerancePolicy      `json:"tolerance"`
	Materiality          MaterialityPolicy    `json:"materiality"`
	ExceptionPolicy      ExceptionPolicy      `json:"exception_policy"`
	CertificationProfile CertificationProfile `json:"certification_profile"`
}

// Digest is the SHA-256 of the rule set's canonical encoding. encoding/json
// writes struct fields in declaration order and map keys sorted, so the same
// rules always produce the same digest — including after a JSONB round trip,
// which the store verifies on every load.
func (r RuleSet) Digest() (string, error) {
	r.normalize()
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// normalize makes absent and empty collections encode alike.
func (r *RuleSet) normalize() {
	if r.BalanceDimensions == nil {
		r.BalanceDimensions = []string{}
	}
	if r.Materiality.Currencies == nil {
		r.Materiality.Currencies = map[string]int64{}
	}
	if r.Tolerance.Currencies == nil {
		r.Tolerance.Currencies = map[string]CurrencyTolerance{}
	}
	for i := range r.Populations {
		if len(r.Populations[i].Params) == 0 {
			r.Populations[i].Params = nil
		}
	}
}

// RulesInput is the rule document as a caller writes it. Amounts (tolerance,
// materiality and every population amount) are integers in units of
// 10^-amount_scale. amount_scale and exception_policy are required, with no
// default, because the scale is part of what "balanced" means (ZS-SVC-Z-001
// INV-09, TC-05) and every exception needs an owner and SLA (A7). Tolerance is
// per currency (A2, A5); a currency without one must match exactly.
// Materiality is per currency (A4); a currency without a threshold is
// UNASSESSED. CertificationProfile defaults to permitting no accepted risk.
type RulesInput struct {
	Populations          []PopulationSpec     `json:"populations"`
	MatchKeyFields       []string             `json:"match_key_fields"`
	BalanceDimensions    []string             `json:"balance_dimensions,omitempty"`
	AmountScale          *int                 `json:"amount_scale"`
	Tolerance            TolerancePolicy      `json:"tolerance"`
	Materiality          MaterialityPolicy    `json:"materiality"`
	ExceptionPolicy      *ExceptionPolicy     `json:"exception_policy"`
	CertificationProfile CertificationProfile `json:"certification_profile"`
}

// RuleSet validates the input and returns it as a rule set.
func (r RulesInput) RuleSet() (RuleSet, error) {
	if err := ValidatePopulationSpecs(r.Populations); err != nil {
		return RuleSet{}, err
	}
	if err := ValidateMatchKeyFields(r.MatchKeyFields); err != nil {
		return RuleSet{}, err
	}
	if err := ValidateBalanceDimensions(r.BalanceDimensions); err != nil {
		return RuleSet{}, err
	}
	if err := r.Tolerance.Validate([]string{r.Populations[0].Side, r.Populations[1].Side}); err != nil {
		return RuleSet{}, err
	}
	if r.AmountScale == nil {
		return RuleSet{}, fmt.Errorf("%w: amount_scale is required", ErrInvalidArgument)
	}
	if *r.AmountScale < 0 || *r.AmountScale > MaxAmountScale {
		return RuleSet{}, fmt.Errorf("%w: amount_scale must be between 0 and %d", ErrInvalidArgument, MaxAmountScale)
	}
	if err := r.Materiality.Validate(); err != nil {
		return RuleSet{}, err
	}
	if r.ExceptionPolicy == nil {
		return RuleSet{}, fmt.Errorf("%w: exception_policy is required", ErrInvalidArgument)
	}
	if err := r.ExceptionPolicy.Validate(); err != nil {
		return RuleSet{}, err
	}
	rs := RuleSet{Populations: r.Populations, MatchKeyFields: r.MatchKeyFields, BalanceDimensions: r.BalanceDimensions,
		AmountScale: *r.AmountScale, Tolerance: r.Tolerance, Materiality: r.Materiality, ExceptionPolicy: *r.ExceptionPolicy,
		CertificationProfile: r.CertificationProfile}
	rs.normalize()
	return rs, nil
}

// ValidateEffectiveFrom: empty means "from the day it is created" (the
// database's current date); otherwise YYYY-MM-DD.
func ValidateEffectiveFrom(s string) error {
	if s == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return fmt.Errorf("%w: effective_from must be YYYY-MM-DD", ErrInvalidArgument)
	}
	return nil
}

// RuleVersion is one immutable version of a definition's rules.
type RuleVersion struct {
	RuleVersionID string     `json:"rule_version_id"`
	TenantID      string     `json:"tenant_id"`
	DefinitionID  string     `json:"definition_id"`
	RuleVersion   int64      `json:"rule_version"`
	Rules         RuleSet    `json:"rules"`
	RulesSHA256   string     `json:"rules_sha256"`
	EffectiveFrom string     `json:"effective_from"`
	Status        string     `json:"status"` // DRAFT or APPROVED, derived from the approval
	Reason        string     `json:"reason"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	ApprovedBy    *string    `json:"approved_by,omitempty"`
	ApprovedAt    *time.Time `json:"approved_at,omitempty"`
}

// CreateRuleVersionRequest publishes a new draft version. BasedOnVersion is
// the definition's latest_rule_version the caller read; if another version was
// published since, the request is refused (ErrRuleVersionConflict).
type CreateRuleVersionRequest struct {
	BasedOnVersion *int64 `json:"based_on_version"`
	RulesInput
	EffectiveFrom string `json:"effective_from,omitempty"`
	Reason        string `json:"reason"`
}

func (r CreateRuleVersionRequest) Validate() error {
	if r.BasedOnVersion == nil || *r.BasedOnVersion < 0 {
		return fmt.Errorf("%w: based_on_version is required (the definition's latest_rule_version)", ErrInvalidArgument)
	}
	if strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 2000 {
		return fmt.Errorf("%w: reason is required", ErrInvalidArgument)
	}
	if err := ValidateEffectiveFrom(r.EffectiveFrom); err != nil {
		return err
	}
	_, err := r.RulesInput.RuleSet()
	return err
}
