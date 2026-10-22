package domain

import (
	"sort"
	"time"
)

// AuthoritySnapshot is the certification authority behind a certification
// (Invariant 9): authorization-svc's own decision on this principal, this
// action and this legal entity, taken live at the moment of certifying, never
// from a cache. AccessDecisionID is the key of the decision's append-only
// record in authorization-svc (its AccessDecisionLog, which also holds the
// matched grants), so the snapshot can be traced to the full evaluation.
type AuthoritySnapshot struct {
	Source           string    `json:"source"` // "authorization-svc"
	PrincipalID      string    `json:"principal_id"`
	LegalEntityID    string    `json:"legal_entity_id"`
	ActionType       string    `json:"action_type"`
	AccessDecisionID string    `json:"access_decision_id"`
	DecisionOutcome  string    `json:"decision_outcome"`
	Decision         string    `json:"decision,omitempty"`
	DecisionBasis    string    `json:"decision_basis"`
	PolicySetVersion string    `json:"policy_set_version,omitempty"`
	ReasonCodes      []string  `json:"reason_codes,omitempty"`
	DecidedAt        time.Time `json:"decided_at"`
}

// CertifiedException is one exception the certification accepted unresolved:
// every exception of the run not CLOSED when it was certified (waived or
// carried forward under authority, in practice), with the authority and target
// period recorded on the transition that put it there.
type CertifiedException struct {
	ExceptionID   string `json:"exception_id"`
	State         string `json:"state"`
	Category      string `json:"category"`
	ReasonCode    string `json:"reason_code"`
	Severity      string `json:"severity"`
	Exposure      string `json:"exposure"` // exact decimal
	Currency      string `json:"currency"`
	AuthorityRef  string `json:"authority_ref,omitempty"`
	CarryToPeriod string `json:"carry_to_period,omitempty"`
}

// ControlCertification is ZS-CONTROL-001 §6's canonical ControlCertification:
// "run_id, certifier, authority snapshot, result, unresolved exceptions,
// assessed exposure, timestamp, evidence package". RunVersion is Invariant 9's
// "run version"; CertificationTransitionID is the CERTIFICATION -> CERTIFIED
// history row it records (the row a revocation references). Written once, in
// the certify transaction, and never changed.
type ControlCertification struct {
	CertificationID           string               `json:"certification_id"`
	RunID                     string               `json:"run_id"`
	RunVersion                int                  `json:"run_version"`
	CertificationTransitionID int64                `json:"certification_transition_id"`
	Certifier                 string               `json:"certifier"`
	Authority                 AuthoritySnapshot    `json:"authority_snapshot"`
	Result                    ResultState          `json:"result"`
	UnresolvedExceptions      []CertifiedException `json:"unresolved_exceptions"`
	AssessedExposure          ExceptionSummary     `json:"assessed_exposure"`
	CertifiedAt               time.Time            `json:"certified_at"`
	EvidencePackageID         string               `json:"evidence_package_id"`
	EvidenceDigest            string               `json:"evidence_digest"`
	Reason                    string               `json:"reason,omitempty"`
}

// AssessExposure is the certification's impact assessment of the exceptions it
// accepts: exact per-currency totals and, against the run's pinned materiality
// policy, whether they are aggregate-material (Summarise).
func AssessExposure(entityID, periodID string, unresolved []CertifiedException, mat *MaterialityView2) (ExceptionSummary, error) {
	rows := make([]ExposureRow, 0, len(unresolved))
	for _, e := range unresolved {
		rows = append(rows, ExposureRow{Currency: e.Currency, Severity: e.Severity, Count: 1, Exposure: e.Exposure})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Currency < rows[j].Currency })
	return Summarise(entityID, periodID, rows, mat)
}
