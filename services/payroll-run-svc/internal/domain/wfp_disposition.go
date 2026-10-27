package domain

// WFP records are financial and payroll evidence. Before anything that owns them deletes, redacts, exports or
// migrates them, the disposition guard asks retention-registry-svc (WFP-NP-041). WFP never deletes anything
// itself and the guard authorizes nothing by being asked: it reports whether disposition is allowed right now.

import "time"

// Record classes the guard accepts. They are the classes WFP owns; the registry holds the policies for them.
const (
	RecordClassPayrollInput    = "WFP_PAYROLL_INPUT"
	RecordClassPayrollResult   = "WFP_PAYROLL_RESULT"
	RecordClassAccountingEvent = "WFP_ACCOUNTING_EVENT"
	RecordClassLiability       = "WFP_LIABILITY"
	RecordClassPaymentInstr    = "WFP_PAYMENT_INSTRUCTION"
	RecordClassReconciliation  = "WFP_RECONCILIATION_EVIDENCE"
)

var ValidRecordClasses = map[string]bool{
	RecordClassPayrollInput: true, RecordClassPayrollResult: true, RecordClassAccountingEvent: true,
	RecordClassLiability: true, RecordClassPaymentInstr: true, RecordClassReconciliation: true,
}

// Reasons a disposition is refused. Stable codes: runbooks and callers key off them.
const (
	DispositionHoldActive        = "LEGAL_HOLD_ACTIVE"
	DispositionNoPolicy          = "NO_RETENTION_POLICY"
	DispositionRetentionNotOver  = "RETENTION_PERIOD_NOT_ELAPSED"
	DispositionRetentionNotKnown = "RECORD_CREATION_DATE_REQUIRED"
)

type DispositionDecision struct {
	RecordClass      string     `json:"record_class"`
	Allowed          bool       `json:"allowed"`
	Reasons          []string   `json:"reasons"`
	LegalHoldID      *string    `json:"legal_hold_id,omitempty"`
	PolicyID         *string    `json:"retention_policy_id,omitempty"`
	MinRetentionDays *int       `json:"min_retention_days,omitempty"`
	EarliestDisposal *time.Time `json:"earliest_disposition_at,omitempty"`
	Note             string     `json:"note"`
}

var ErrRetentionUnavailable = errorString("retention-registry-svc could not answer; disposition is not allowed")

// DecideDisposition applies the rules to the registry's findings. Anything missing means NOT allowed: a record
// is disposable only when there is no hold, a policy applies, and its minimum retention has run out.
func DecideDisposition(recordClass string, blocked bool, holdID *string, policyID *string, minDays *int, createdAt *time.Time, now time.Time) DispositionDecision {
	d := DispositionDecision{RecordClass: recordClass, Reasons: []string{}, LegalHoldID: holdID, PolicyID: policyID, MinRetentionDays: minDays,
		Note: "this answer authorizes nothing by itself: the service that owns the record deletes it, and WFP never does"}
	if blocked {
		d.Reasons = append(d.Reasons, DispositionHoldActive)
	}
	switch {
	case policyID == nil || minDays == nil:
		d.Reasons = append(d.Reasons, DispositionNoPolicy)
	case createdAt == nil:
		d.Reasons = append(d.Reasons, DispositionRetentionNotKnown)
	default:
		earliest := createdAt.UTC().AddDate(0, 0, *minDays)
		d.EarliestDisposal = &earliest
		if now.Before(earliest) {
			d.Reasons = append(d.Reasons, DispositionRetentionNotOver)
		}
	}
	d.Allowed = len(d.Reasons) == 0
	return d
}
