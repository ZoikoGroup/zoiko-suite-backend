package domain

// Tax engine provenance for a payroll calculation.
//
// A payslip's tax used to be one number (taxable amount times a placeholder rate). When the calculation
// is wired to payroll-tax-svc, each slip keeps the engine's own breakdown: one line per tax component,
// with the calculation id, the engine type, the rule version and the jurisdiction it was computed under.
// WFP never recomputes any of it; it records what the engine said, classifies each component for the
// accounting and liability interfaces, and refuses what it cannot classify.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	// FlagTaxEngineUncertified is set on every result whose tax came from payroll-tax-svc while its
	// engine is not declared certified (config PAYROLL_TAX_ENGINE_CERTIFIED). Tax computed by an
	// uncertified engine is recorded and attributed, but is not presented as rule-backed.
	FlagTaxEngineUncertified = "TAX_ENGINE_UNCERTIFIED"
	// FlagInputNotFrozen is set when the run did not consume a frozen WFP input batch (WFP-CTRL-006).
	FlagInputNotFrozen = "INPUT_NOT_FROZEN"

	// TaxManifestVersionPrefix ties a registered rule manifest to the tax engine's rule version: the
	// manifest that backs a result computed under rule version V must have manifest_version
	// "payroll-tax:V". The operator who registers it supplies the evidence reference.
	TaxManifestVersionPrefix = "payroll-tax:"
)

// TaxManifestVersion is the manifest_version a result computed under ruleVersion must name.
func TaxManifestVersion(ruleVersion string) string { return TaxManifestVersionPrefix + ruleVersion }

// PaySlipTaxLine is one tax component the engine computed for one payslip.
type PaySlipTaxLine struct {
	LineID           string    `json:"line_id"`
	TenantID         string    `json:"tenant_id"`
	SlipID           string    `json:"slip_id"`
	Sequence         int       `json:"sequence"`
	TaxName          string    `json:"tax_name"`
	TaxType          string    `json:"tax_type"`
	RatePct          float64   `json:"rate_pct"`
	Amount           float64   `json:"amount"`
	CalculationID    string    `json:"calculation_id"`
	EngineType       string    `json:"engine_type"`
	RuleVersion      string    `json:"rule_version"`
	JurisdictionCode string    `json:"jurisdiction_code"`
	CreatedAt        time.Time `json:"created_at"`
}

// ComponentKindForTaxType classifies an engine tax type for WFP. An engine type this service does not
// know is refused, never defaulted: a mis-classified statutory deduction would post to the wrong
// payable and be owed to the wrong authority.
func ComponentKindForTaxType(taxType string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(taxType)) {
	case "FEDERAL", "STATE", "LOCAL", "STATE_FEDERAL", "INCOME":
		return KindEmployeeTax, nil
	case "SOCIAL", "SOCIAL_SECURITY", "HEALTH_STATUTORY", "HEALTH":
		return KindEmployeeSocial, nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnmappedTaxType, taxType)
}

// TaxComponentCode is the stable component code of a tax line in a WFP result.
func TaxComponentCode(taxName string) string {
	var b strings.Builder
	b.WriteString("TAX:")
	prevUnderscore := false
	for _, r := range strings.ToUpper(strings.TrimSpace(taxName)) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.TrimRight(b.String(), "_")
}

// TaxCorrelationID is the idempotency key sent to the tax engine: identical inputs for one run and
// employee reuse the engine's stored calculation (and its real id); changed inputs are a new
// calculation.
func TaxCorrelationID(runID, employeeID string, taxable float64, jurisdiction, currency string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%.4f|%s|%s", taxable, jurisdiction, currency)))
	return "wfp-tax:" + runID + ":" + employeeID + ":" + hex.EncodeToString(sum[:])[:12]
}

// SlipTaxSource describes where a run's tax came from, derived from its payslips.
type SlipTaxSource struct {
	EngineSlips      int      // slips whose tax has engine lines
	PlaceholderSlips int      // slips with tax withheld and no engine lines
	RuleVersions     []string // distinct rule versions, sorted
}

// TaxSourceOf inspects a run's payslips.
func TaxSourceOf(slips []PaySlip) SlipTaxSource {
	var out SlipTaxSource
	seen := map[string]bool{}
	for _, s := range slips {
		switch {
		case len(s.TaxLines) > 0:
			out.EngineSlips++
			for _, l := range s.TaxLines {
				if !seen[l.RuleVersion] {
					seen[l.RuleVersion] = true
					out.RuleVersions = append(out.RuleVersions, l.RuleVersion)
				}
			}
		case s.TaxWithheld > 0:
			out.PlaceholderSlips++
		}
	}
	for i := 1; i < len(out.RuleVersions); i++ {
		for j := i; j > 0 && out.RuleVersions[j] < out.RuleVersions[j-1]; j-- {
			out.RuleVersions[j], out.RuleVersions[j-1] = out.RuleVersions[j-1], out.RuleVersions[j]
		}
	}
	return out
}

var (
	ErrUnmappedTaxType             = errorString("the tax engine returned a tax type this service cannot classify")
	ErrTaxEngineUnavailable        = errorString("payroll-tax-svc is unavailable or refused the calculation; the run is blocked, never estimated")
	ErrTaxJurisdictionUnresolved   = errorString("an employee has no single payroll jurisdiction in their worker financial profile; the tax cannot be computed")
	ErrTaxTotalMismatch            = errorString("the tax engine's total differs from the sum of its components")
	ErrManifestRuleVersionMismatch = errorString("the rule manifest does not match the rule version the tax engine used (manifest_version must be payroll-tax:<rule version>)")
	ErrMixedTaxRuleVersions        = errorString("the run's tax was computed under more than one rule version; a result names exactly one rule manifest")
	ErrTaxEngineNotConfigured      = errorString("tax engine mode requires payroll-tax-svc and the worker financial profile service to be configured")
)
