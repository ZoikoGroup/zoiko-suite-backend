package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// RiskCategory is one scored dimension of compliance risk.
type RiskCategory string

const (
	CategoryRegulatoryObligations RiskCategory = "REGULATORY_OBLIGATIONS"
	CategoryPolicyViolations      RiskCategory = "POLICY_VIOLATIONS"
	CategoryAuditExceptions       RiskCategory = "AUDIT_EXCEPTIONS"
	CategoryDataPrivacy           RiskCategory = "DATA_PRIVACY"
	CategoryTaxCompliance         RiskCategory = "TAX_COMPLIANCE"

	// CategoryComposite is not a scored factor; a threshold rule for
	// COMPOSITE sets the band ladder for the overall composite score.
	CategoryComposite RiskCategory = "COMPOSITE"
)

// IsScoredFactor reports whether c is one of the five model factors.
func (c RiskCategory) IsScoredFactor() bool {
	for _, factor := range CurrentModel.Factors {
		if factor.Category == c {
			return true
		}
	}
	return false
}

// IsThresholdCategory reports whether a threshold rule may target c.
func (c RiskCategory) IsThresholdCategory() bool {
	return c == CategoryComposite || c.IsScoredFactor()
}

// RiskTier is the band a score falls into.
type RiskTier string

const (
	TierLow      RiskTier = "LOW"
	TierModerate RiskTier = "MODERATE"
	TierHigh     RiskTier = "HIGH"
	TierCritical RiskTier = "CRITICAL"
)

// IsValid reports whether t is a known tier.
func (t RiskTier) IsValid() bool {
	switch t {
	case TierLow, TierModerate, TierHigh, TierCritical:
		return true
	}
	return false
}

// RequiresHumanReview: a HIGH or CRITICAL score is advisory and must be
// dispositioned by a person before anyone treats it as a finding
// (Data/Search/Analytics/AI spec §2: "Anomaly scores or AI confidence cannot
// alone trigger adverse, irreversible or material financial actions").
func (t RiskTier) RequiresHumanReview() bool {
	return t == TierHigh || t == TierCritical
}

// BandCutoffs is a band ladder: a score >= Critical is CRITICAL, >= High is
// HIGH, >= Moderate is MODERATE, otherwise LOW.
type BandCutoffs struct {
	Moderate Hundredths `json:"moderate"`
	High     Hundredths `json:"high"`
	Critical Hundredths `json:"critical"`
}

// TierFor derives the band for score.
func (b BandCutoffs) TierFor(score Hundredths) RiskTier {
	if score >= b.Critical {
		return TierCritical
	}
	if score >= b.High {
		return TierHigh
	}
	if score >= b.Moderate {
		return TierModerate
	}
	return TierLow
}

// Validate enforces 0 < moderate < high < critical <= 100.
func (b BandCutoffs) Validate() []FieldError {
	var problems []FieldError
	if b.Moderate <= 0 || b.Moderate > OneHundredPercent {
		problems = append(problems, FieldError{Field: "moderate_threshold", Detail: "must be > 0 and <= 100"})
	}
	if b.High <= 0 || b.High > OneHundredPercent {
		problems = append(problems, FieldError{Field: "high_threshold", Detail: "must be > 0 and <= 100"})
	}
	if b.Critical <= 0 || b.Critical > OneHundredPercent {
		problems = append(problems, FieldError{Field: "critical_threshold", Detail: "must be > 0 and <= 100"})
	}
	if len(problems) > 0 {
		return problems
	}
	if !(b.Moderate < b.High && b.High < b.Critical) {
		problems = append(problems, FieldError{
			Field:  "thresholds",
			Detail: "must be strictly increasing: moderate_threshold < high_threshold < critical_threshold",
		})
	}
	return problems
}

// FactorDefinition describes how one category is scored.
//
//	raw score      = min(100, input_count / saturation_count x 100)
//	weighted score = raw score x weight / 100
//
// SaturationCount is the count at which the category reaches 100.
type FactorDefinition struct {
	Category        RiskCategory `json:"risk_category"`
	InputField      string       `json:"input_field"`
	Weight          Hundredths   `json:"weight_percent"`
	SaturationCount int64        `json:"saturation_count"`
	DriverTemplate  string       `json:"driver_template"`
}

// ScoringModel is a named, immutable scoring configuration. Any change to
// weights, saturation points or default bands must ship as a NEW Version so
// historic scores stay reproducible (AI spec §2: "silent model switching is
// prohibited for material workflows").
type ScoringModel struct {
	Version      string             `json:"model_version"`
	Description  string             `json:"description"`
	Factors      []FactorDefinition `json:"factors"`
	DefaultBands BandCutoffs        `json:"default_bands"`
}

// CurrentModel is the only model this build scores with. Its weights and
// saturation points are the ones the 000001-era code hard-coded, so scores
// stay comparable across the upgrade; the only behavioural change is exact
// decimal arithmetic (the old code scored privacy incidents at 33.3 each, so
// three incidents gave 99.9 instead of the documented 100).
var CurrentModel = ScoringModel{
	Version: "CRS-WEIGHTED-1.0",
	Description: "Deterministic weighted sum of five saturating count factors. " +
		"Output is ADVISORY: HIGH/CRITICAL scores require human review before use.",
	Factors: []FactorDefinition{
		{Category: CategoryRegulatoryObligations, InputField: "open_obligations_count", Weight: 3000, SaturationCount: 8,
			DriverTemplate: "%d open regulatory obligations pending resolution"},
		{Category: CategoryPolicyViolations, InputField: "policy_violations_count", Weight: 2500, SaturationCount: 5,
			DriverTemplate: "%d active policy non-compliance breaches logged"},
		{Category: CategoryAuditExceptions, InputField: "audit_exceptions_count", Weight: 2000, SaturationCount: 4,
			DriverTemplate: "%d unresolved internal/external audit findings"},
		{Category: CategoryDataPrivacy, InputField: "privacy_incidents_count", Weight: 1500, SaturationCount: 3,
			DriverTemplate: "%d data privacy compliance incidents"},
		{Category: CategoryTaxCompliance, InputField: "tax_penalties_count", Weight: 1000, SaturationCount: 2,
			DriverTemplate: "%d statutory tax penalties or late-filing flags"},
	},
	DefaultBands: BandCutoffs{Moderate: 2600, High: 5100, Critical: 7600},
}

// ScoringInputs are the observed counts a score is computed from.
type ScoringInputs struct {
	OpenObligationsCount  int64
	PolicyViolationsCount int64
	AuditExceptionsCount  int64
	PrivacyIncidentsCount int64
	TaxPenaltiesCount     int64
}

// CountFor returns the input count that feeds category.
func (in ScoringInputs) CountFor(category RiskCategory) int64 {
	switch category {
	case CategoryRegulatoryObligations:
		return in.OpenObligationsCount
	case CategoryPolicyViolations:
		return in.PolicyViolationsCount
	case CategoryAuditExceptions:
		return in.AuditExceptionsCount
	case CategoryDataPrivacy:
		return in.PrivacyIncidentsCount
	case CategoryTaxCompliance:
		return in.TaxPenaltiesCount
	}
	return 0
}

// ThresholdSource says where the composite band ladder came from.
type ThresholdSource string

const (
	ThresholdSourcePlatformDefault ThresholdSource = "PLATFORM_DEFAULT"
	ThresholdSourceTenantRule      ThresholdSource = "TENANT_RULE"
)

// AppliedThresholds records exactly which ladder banded a score.
type AppliedThresholds struct {
	Source      ThresholdSource `json:"source"`
	RuleID      *string         `json:"rule_id"`
	RuleVersion *int            `json:"rule_version"`
	Cutoffs     BandCutoffs     `json:"cutoffs"`
}

// ScoringThresholds is everything that bands a calculation: the composite
// ladder plus optional per-category ladders from tenant rules.
type ScoringThresholds struct {
	Composite  AppliedThresholds
	ByCategory map[RiskCategory]BandCutoffs
}

// FactorResult is one row of the explainable breakdown.
type FactorResult struct {
	Category              RiskCategory
	InputCount            int64
	SaturationCount       int64
	Weight                Hundredths
	RawScore              Hundredths
	WeightedScore         Hundredths
	CategoryThresholdBand *RiskTier
	DriverSummary         string
}

// ScoreResult is the full, deterministic output of one calculation.
type ScoreResult struct {
	ModelVersion   string
	CompositeScore Hundredths
	Tier           RiskTier
	Factors        []FactorResult
	BandRationale  string
	InputHash      string
}

// ComputeScore runs model over inputs. It is a pure function: the same
// model, subject, inputs and thresholds always produce the same result and
// the same InputHash.
func ComputeScore(model ScoringModel, subject ScoreSubject, inputs ScoringInputs, thresholds ScoringThresholds) ScoreResult {
	factors := make([]FactorResult, 0, len(model.Factors))
	composite := Hundredths(0)

	for _, definition := range model.Factors {
		factor := computeFactor(definition, inputs.CountFor(definition.Category))

		if categoryCutoffs, hasRule := thresholds.ByCategory[definition.Category]; hasRule {
			band := categoryCutoffs.TierFor(factor.RawScore)
			if band != TierLow {
				factor.CategoryThresholdBand = &band
			}
		}

		composite += factor.WeightedScore
		factors = append(factors, factor)
	}

	tier := thresholds.Composite.Cutoffs.TierFor(composite)

	return ScoreResult{
		ModelVersion:   model.Version,
		CompositeScore: composite,
		Tier:           tier,
		Factors:        factors,
		BandRationale:  explainBand(composite, tier, thresholds.Composite),
		InputHash:      hashInputs(model.Version, subject, inputs, thresholds.Composite.Cutoffs),
	}
}

// computeFactor scores one category with exact integer arithmetic.
func computeFactor(definition FactorDefinition, count int64) FactorResult {
	rawScore := Hundredths(divideRoundHalfUp(count*int64(OneHundredPercent), definition.SaturationCount))
	if rawScore > OneHundredPercent {
		rawScore = OneHundredPercent
	}

	// raw (hundredths) x weight (hundredths of a percent) / 100.00% -> hundredths
	weightedScore := Hundredths(divideRoundHalfUp(int64(rawScore)*int64(definition.Weight), int64(OneHundredPercent)))

	return FactorResult{
		Category:        definition.Category,
		InputCount:      count,
		SaturationCount: definition.SaturationCount,
		Weight:          definition.Weight,
		RawScore:        rawScore,
		WeightedScore:   weightedScore,
		DriverSummary:   fmt.Sprintf(definition.DriverTemplate, count),
	}
}

// explainBand states in words why the score landed in its band.
func explainBand(score Hundredths, tier RiskTier, applied AppliedThresholds) string {
	cutoffs := applied.Cutoffs
	source := string(applied.Source)
	if applied.RuleID != nil {
		source = fmt.Sprintf("%s %s v%d", applied.Source, *applied.RuleID, *applied.RuleVersion)
	}

	switch tier {
	case TierCritical:
		return fmt.Sprintf("composite %s >= CRITICAL cut-off %s (thresholds: %s)", score, cutoffs.Critical, source)
	case TierHigh:
		return fmt.Sprintf("composite %s >= HIGH cut-off %s and < CRITICAL cut-off %s (thresholds: %s)",
			score, cutoffs.High, cutoffs.Critical, source)
	case TierModerate:
		return fmt.Sprintf("composite %s >= MODERATE cut-off %s and < HIGH cut-off %s (thresholds: %s)",
			score, cutoffs.Moderate, cutoffs.High, source)
	default:
		return fmt.Sprintf("composite %s < MODERATE cut-off %s (thresholds: %s)", score, cutoffs.Moderate, source)
	}
}

// hashInputs fingerprints everything that determines a score so a later
// reviewer can prove two scores were computed from identical inputs.
func hashInputs(modelVersion string, subject ScoreSubject, inputs ScoringInputs, cutoffs BandCutoffs) string {
	parts := []string{
		"model=" + modelVersion,
		"legal_entity=" + subject.LegalEntityID,
		"subject_type=" + string(subject.Type),
		"subject_id=" + subject.ID,
		fmt.Sprintf("counts=%d,%d,%d,%d,%d", inputs.OpenObligationsCount, inputs.PolicyViolationsCount,
			inputs.AuditExceptionsCount, inputs.PrivacyIncidentsCount, inputs.TaxPenaltiesCount),
		fmt.Sprintf("bands=%s,%s,%s", cutoffs.Moderate, cutoffs.High, cutoffs.Critical),
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "sha256:" + hex.EncodeToString(digest[:])
}
