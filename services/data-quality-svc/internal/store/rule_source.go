package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/data-quality-svc/internal/domain"
)

// A run is judged against exactly one rule source: a rule-set version (the original model: every rule
// mandatory, no severity) or an exact PUBLISHED DQC-01 contract version (rules with severity,
// dimension, tolerance, and a certification profile naming which are mandatory).
// ZS-SVC-Z-001 DQC-02: validation evaluates against an EXACT contract, never "latest".

type ruleSpec struct {
	Key       string
	Mandatory bool
	Def       *domain.RuleDefinition // rule-set rule
	Rule      *domain.ContractRule   // contract rule
}

type ruleSource struct {
	RuleSetVersionID *string
	ContractID       *string
	AssetID          string
	ContractSHA256   string
	Specs            []ruleSpec
}

func (s *ruleSource) isContract() bool { return s.ContractID != nil }

func (s *ruleSource) spec(key string) (ruleSpec, bool) {
	for _, sp := range s.Specs {
		if sp.Key == key {
			return sp, true
		}
	}
	return ruleSpec{}, false
}

func sourceFromRuleSet(v *domain.DQRuleSetVersion) *ruleSource {
	src := &ruleSource{RuleSetVersionID: &v.VersionID}
	for i := range v.Rules {
		src.Specs = append(src.Specs, ruleSpec{Key: v.Rules[i].RuleKey, Mandatory: true, Def: &v.Rules[i]})
	}
	return src
}

func sourceFromContract(c *domain.ContractVersion) *ruleSource {
	src := &ruleSource{ContractID: &c.ContractID, AssetID: c.AssetID, ContractSHA256: c.ContentSHA256}
	mandatory := map[string]bool{}
	for _, k := range c.Body.CertificationProfile.MandatoryRules {
		mandatory[k] = true
	}
	for i := range c.Body.Rules {
		r := &c.Body.Rules[i]
		// A BLOCKER is mandatory whatever the profile says; Validate already refuses a contract that
		// says otherwise, this keeps the guarantee if one ever predates that check.
		src.Specs = append(src.Specs, ruleSpec{Key: r.RuleKey, Mandatory: mandatory[r.RuleKey] || r.Severity == domain.SeverityBlocker, Rule: r})
	}
	return src
}

func loadContractByID(ctx context.Context, tx pgx.Tx, contractID string) (*domain.ContractVersion, error) {
	v, err := scanContract(tx.QueryRow(ctx, `SELECT `+contractCols+` FROM dqc_contract_versions c JOIN dqc_assets a ON a.asset_id = c.asset_id
		WHERE c.contract_id = $1`, contractID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrContractVersionUnknown
	}
	return v, err
}

// loadRuleSource resolves a run request's rule source. A contract must be PUBLISHED: a DRAFT or
// REVIEW version is not yet a contract anyone may be judged against, and a DEPRECATED or RETIRED one
// is not used for new evidence.
func loadRuleSource(ctx context.Context, tx pgx.Tx, req domain.RunDQRequest) (*ruleSource, error) {
	if req.ContractAssetID == "" {
		v, err := loadRuleSetVersion(ctx, tx, req.RuleSetVersionID)
		if err != nil {
			return nil, err
		}
		return sourceFromRuleSet(v), nil
	}
	c, err := loadContract(ctx, tx, req.ContractAssetID, req.ContractVersion, false)
	if err != nil {
		return nil, err
	}
	if c.Status != domain.ContractPublished {
		return nil, fmt.Errorf("%w: %s v%d is %s", domain.ErrContractNotPublished, c.AssetKey, c.VersionNumber, c.Status)
	}
	return sourceFromContract(c), nil
}

// loadRuleSourceForRun re-resolves the source a stored run was judged against, WITHOUT the published
// check: certifying or reperforming a run that was judged against a contract which has since been
// deprecated is legitimate, because the run's evidence is bound to that exact version.
func loadRuleSourceForRun(ctx context.Context, tx pgx.Tx, run *domain.DQRun) (*ruleSource, error) {
	if run.ContractID != nil {
		c, err := loadContractByID(ctx, tx, *run.ContractID)
		if err != nil {
			return nil, err
		}
		return sourceFromContract(c), nil
	}
	v, err := loadRuleSetVersion(ctx, tx, run.RuleSetVersionID)
	if err != nil {
		return nil, err
	}
	return sourceFromRuleSet(v), nil
}

// evaluate judges one submitted outcome against the rule's own definition.
func (sp ruleSpec) evaluate(ro domain.RuleOutcome) (domain.DQResultStatus, error) {
	if sp.Rule == nil {
		return evaluateOutcome(sp.Def, ro)
	}
	return evaluateContractOutcome(sp.Rule.Tolerance, ro)
}

// evaluateContractOutcome applies a contract rule's explicit tolerance:
//   - no records evaluated -> Indeterminate (the absence of evidence is not a pass)
//   - no failures -> Pass
//   - failures and no tolerance -> Fail (strict)
//   - ABSOLUTE: Pass when the failure magnitude sum is within the tolerance
//   - PERCENT:  Pass when failures as a percentage of evaluated records are within the tolerance
func evaluateContractOutcome(t *domain.Tolerance, ro domain.RuleOutcome) (domain.DQResultStatus, error) {
	return domain.EvaluateTolerance(t, ro.RuleKey, ro.PassCount, ro.FailCount, ro.FailureMagnitudeSum)
}

func contractIDOf(run *domain.DQRun) string {
	if run.ContractID == nil {
		return ""
	}
	return *run.ContractID
}
