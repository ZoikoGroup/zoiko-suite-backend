package domain

import (
	"fmt"
	"strings"
)

// ZS-JUR-001 s23, s31 and JUR-NEG-19: the remediation workflow for outcomes produced with a pack version that was rolled back or withdrawn.
//
// Rolling back or withdrawing changes FUTURE resolution only; past outcomes are never rewritten. A remediation case snapshots the decisions made with
// the version and tracks one disposition per decision, with evidence, until every one is answered for and someone other than the opener closes it.
// Re-evaluating an outcome is the job of the service that used it; this records that it was done and who vouched for it.

// Remediation dispositions.
const (
	DispositionReconfirmed = "RECONFIRMED"
	DispositionNotAffected = "NOT_AFFECTED"
	DispositionRecalc      = "RECALCULATED"
	DispositionSuperseded  = "SUPERSEDED_BY_NEW_DECISION"
)

// Remediation triggers.
const (
	RemediationRollback = "ROLLBACK"
	RemediationWithdraw = "WITHDRAW"
	RemediationManual   = "MANUAL"
)

var (
	ErrRemediationNotFound  = errorString("remediation case not found")
	ErrNothingToRemediate   = errorString("no decision was made with this pack version in this scope, so there is nothing to remediate")
	ErrRemediationRefused   = errorString("the remediation record was refused")
	ErrDecisionNotInCase    = errorString("a decision named is not part of this remediation case")
	ErrDispositionDiffers   = errorString("this decision already has a different disposition: dispositions are final")
)

// ValidDisposition reports whether d is a known disposition.
func ValidDisposition(d string) bool {
	return d == DispositionReconfirmed || d == DispositionNotAffected || d == DispositionRecalc || d == DispositionSuperseded
}

// DispositionNeedsReplacement reports whether the disposition must name the decision that replaced the original.
func DispositionNeedsReplacement(d string) bool { return d == DispositionRecalc || d == DispositionSuperseded }

// DispositionInput is one decision's disposition as submitted.
type DispositionInput struct {
	DecisionID    string  `json:"decision_id"`
	Disposition   string  `json:"disposition"`
	EvidenceRef   string  `json:"evidence_ref"`
	Note          string  `json:"note"`
	NewDecisionID *string `json:"new_decision_id"`
}

// Validate checks one disposition's shape (the database re-checks all of it).
func (d DispositionInput) Validate() error {
	if strings.TrimSpace(d.DecisionID) == "" {
		return fmt.Errorf("decision_id is required")
	}
	if !ValidDisposition(d.Disposition) {
		return fmt.Errorf("disposition must be one of RECONFIRMED, NOT_AFFECTED, RECALCULATED, SUPERSEDED_BY_NEW_DECISION")
	}
	if strings.TrimSpace(d.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required: a disposition without evidence is an assertion nobody can defend at audit")
	}
	if len(d.Note) > 2000 {
		return fmt.Errorf("note is at most 2000 characters")
	}
	hasNew := d.NewDecisionID != nil && strings.TrimSpace(*d.NewDecisionID) != ""
	if DispositionNeedsReplacement(d.Disposition) && !hasNew {
		return fmt.Errorf("%s needs new_decision_id: the decision that replaced this one", d.Disposition)
	}
	if !DispositionNeedsReplacement(d.Disposition) && hasNew {
		return fmt.Errorf("%s takes no new_decision_id", d.Disposition)
	}
	if hasNew && *d.NewDecisionID == d.DecisionID {
		return fmt.Errorf("new_decision_id must be a different decision")
	}
	return nil
}
