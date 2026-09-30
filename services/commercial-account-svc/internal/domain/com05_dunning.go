// COM-05 Platform Commercial Billing, part 5d (ZS-SVC-Q-001 §4.5). Owns
// DunningCase and CommercialReconciliation — the last two entities of
// COM-05's Owns list.
//
// COM-CTRL-030 (dunning-entitlement policy boundary): a DunningCase is
// billing-health evidence, never a capability grant or denial. Nothing in
// this file computes, stores or exposes an entitlement decision — that
// remains COM-03's alone, reading this service's facts if it chooses to.
package domain

import "time"

const (
	PrefixDunningCase    = "cdun_"
	PrefixReconciliation = "crec_"
)

type DunningStatus string

const (
	DunningNotice1    DunningStatus = "NOTICE_1"
	DunningNotice2    DunningStatus = "NOTICE_2"
	DunningRestricted DunningStatus = "RESTRICTED"
	DunningSuspended  DunningStatus = "SUSPENDED"
	DunningClosed     DunningStatus = "CLOSED"
)

// dunningStageOrder is the one place the escalation sequence is named —
// AdvanceDunning and the migration's own lifecycle trigger must never
// disagree about what "the next stage" is.
var dunningStageOrder = []DunningStatus{DunningNotice1, DunningNotice2, DunningRestricted, DunningSuspended}

// NextDunningStage returns the stage one step past current, or "" if
// current is already the last escalation stage (SUSPENDED) or CLOSED.
func NextDunningStage(current DunningStatus) DunningStatus {
	for i, s := range dunningStageOrder {
		if s == current && i+1 < len(dunningStageOrder) {
			return dunningStageOrder[i+1]
		}
	}
	return ""
}

// DunningPolicyVersion is the versioned set of day-count thresholds a
// DunningCase binds to when it opens (COM-CTRL-012; negative path #34: a
// later version never retroactively changes an already-open case).
type DunningPolicyVersion struct {
	PolicyVersion        int       `json:"policy_version"`
	Notice1AfterDays     int       `json:"notice1_after_days"`
	Notice2AfterDays     int       `json:"notice2_after_days"`
	RestrictAfterDays    int       `json:"restrict_after_days"`
	SuspendAfterDays     int       `json:"suspend_after_days"`
	EffectiveFrom        time.Time `json:"effective_from"`
	Reason               string    `json:"reason"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

func ValidateDunningPolicyVersion(p *DunningPolicyVersion) error {
	if p.Notice1AfterDays < 0 {
		return invalid("notice1_after_days", "must not be negative")
	}
	if p.Notice2AfterDays <= p.Notice1AfterDays {
		return invalid("notice2_after_days", "must be greater than notice1_after_days")
	}
	if p.RestrictAfterDays <= p.Notice2AfterDays {
		return invalid("restrict_after_days", "must be greater than notice2_after_days")
	}
	if p.SuspendAfterDays <= p.RestrictAfterDays {
		return invalid("suspend_after_days", "must be greater than restrict_after_days")
	}
	if strEmpty(p.Reason) {
		return invalid("reason", "is required")
	}
	return nil
}

// DunningCase is one invoice's collections escalation, bound for its whole
// life to the policy version effective when it opened.
type DunningCase struct {
	CaseID              string        `json:"case_id"`
	OrganizationID      string        `json:"organization_id"`
	InvoiceID           string        `json:"invoice_id"`
	PolicyVersion       int           `json:"policy_version"`
	Status              DunningStatus `json:"status"`
	OpenedAt            time.Time     `json:"opened_at"`
	OpenedByPrincipalID string        `json:"opened_by_principal_id"`
	LastAdvancedAt      *time.Time    `json:"last_advanced_at,omitempty"`
	// AppliedRestrictionID is set once the case escalates to RESTRICTED or
	// SUSPENDED and a CommercialRestriction (COM-03) is applied on its
	// behalf — so StopDunning knows exactly which restriction to lift.
	// Never set for NOTICE_1/NOTICE_2, which are informational only.
	AppliedRestrictionID *string    `json:"applied_restriction_id,omitempty"`
	ClosedAt             *time.Time `json:"closed_at,omitempty"`
	ClosedByPrincipalID  *string    `json:"closed_by_principal_id,omitempty"`
	CloseReason          *string    `json:"close_reason,omitempty"`
}

// ── Reconciliation ───────────────────────────────────────────────────────────

type ReconciliationStatus string

const (
	ReconciliationClean          ReconciliationStatus = "CLEAN"
	ReconciliationExceptionsOpen ReconciliationStatus = "EXCEPTIONS_OPEN"
)

// ReconciliationException is one concrete, evidenced discrepancy — never a
// bare "something is wrong" (negative path #43: "do not hide difference").
type ReconciliationException struct {
	InvoiceID string `json:"invoice_id"`
	Check     string `json:"check"`
	Detail    string `json:"detail"`
}

// CommercialReconciliation is one immutable, point-in-time re-verification
// run of an organization's invoices against the invariants this service
// already enforces at write time.
type CommercialReconciliation struct {
	ReconciliationID string                    `json:"reconciliation_id"`
	OrganizationID   string                    `json:"organization_id"`
	Status           ReconciliationStatus      `json:"status"`
	InvoiceCount     int                       `json:"invoice_count"`
	ExceptionCount   int                       `json:"exception_count"`
	Exceptions       []ReconciliationException `json:"exceptions"`
	RunAt            time.Time                 `json:"run_at"`
	RunByPrincipalID string                    `json:"run_by_principal_id"`
}

var (
	ErrDunningPolicyNotFound            = errorString("no dunning policy version is effective yet")
	ErrDunningCaseNotFound              = errorString("dunning case not found")
	ErrDunningCaseAlreadyOpenForInvoice = errorString("an open dunning case already exists for this invoice")
	ErrDunningCaseInvalidState          = errorString("dunning case is not in a state that allows this action")
	ErrDunningCaseFullyEscalated        = errorString("dunning case is already at its final escalation stage")
	ErrReconciliationNotFound           = errorString("no reconciliation run found for this organization")
)
