package domain

// ZS-SVC-L-001 WFP-02 — Time & Leave Financial Import.
//
// This service imports leave that its own approval workflow has already
// approved; it does not decide attendance truth or leave approval. Hours are
// exact decimal STRINGS here (the database column is NUMERIC(10,2)), never
// float64, so control totals reconcile exactly.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// SourceLeaveAbsence identifies leave-absence-svc as the source system of
	// the leave it snapshots. Worked time has no source yet (decision 2026-10-14).
	SourceLeaveAbsence = "leave-absence-svc"

	// NormalizationVersion changes whenever the way a leave request becomes a
	// financial line changes, so a batch can always say how it was normalized.
	NormalizationVersion = "leave-v1"

	UnitHours = "HOURS"
)

const (
	BatchReceived   = "RECEIVED"
	BatchAccepted   = "ACCEPTED"
	BatchFrozen     = "FROZEN"
	BatchSuperseded = "SUPERSEDED"
	BatchRejected   = "REJECTED"
)

const (
	LineAccepted    = "ACCEPTED"
	LineQuarantined = "QUARANTINED"
)

// OnInvalid is the batch policy for invalid lines (spec WFP-02 state/workflow).
const (
	OnInvalidQuarantineLines = "QUARANTINE_LINES"
	OnInvalidRejectBatch     = "REJECT_BATCH"
)

// Quarantine reasons. Stable codes: runbook WFP-RB-02 keys off them.
const (
	ReasonNotApproved             = "NOT_APPROVED"
	ReasonMissingApprovalEvidence = "MISSING_APPROVAL_EVIDENCE"
	ReasonCrossPeriod             = "CROSS_PERIOD_REQUIRES_ALLOCATION"
	ReasonManual                  = "MANUALLY_QUARANTINED"
	// Worker-context reasons come from employee-master-svc's
	// validate-context (WORKER_NOT_FOUND, CROSS_ENTITY_WORKER, INACTIVE_WORKER,
	// NO_ACTIVE_PROFILE) and are stored verbatim.
)

type TimeLeaveBatch struct {
	BatchID              string     `json:"batch_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	PayrollPeriodID      string     `json:"payroll_period_id"`
	PeriodStart          string     `json:"period_start"`
	PeriodEnd            string     `json:"period_end"`
	SourceSystemID       string     `json:"source_system_id"`
	BatchVersion         int        `json:"batch_version"`
	OnInvalid            string     `json:"on_invalid"`
	State                string     `json:"state"`
	RejectionReason      *string    `json:"rejection_reason,omitempty"`
	LineCount            int        `json:"line_count"`
	AcceptedLineCount    int        `json:"accepted_line_count"`
	QuarantinedLineCount int        `json:"quarantined_line_count"`
	TotalUnits           string     `json:"total_units"`
	PaidUnits            string     `json:"paid_units"`
	UnpaidUnits          string     `json:"unpaid_units"`
	UnitOfMeasure        string     `json:"unit_of_measure"`
	SourceHash           *string    `json:"source_hash,omitempty"`
	NormalizationVersion string     `json:"normalization_version"`
	SupersedesBatchID    *string    `json:"supersedes_batch_id,omitempty"`
	Version              int        `json:"version"`
	FrozenAt             *time.Time `json:"frozen_at,omitempty"`
	FrozenBy             *string    `json:"frozen_by,omitempty"`
	CorrelationID        string     `json:"correlation_id"`
	CreatedBy            string     `json:"created_by"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type TimeLeaveLine struct {
	LineID           string     `json:"line_id"`
	BatchID          string     `json:"batch_id"`
	SourceLineID     string     `json:"source_line_id"`
	WorkerID         string     `json:"worker_id"`
	LeaveTypeID      string     `json:"leave_type_id"`
	LeaveTypeCode    string     `json:"leave_type_code"`
	IsPaid           bool       `json:"is_paid"`
	StartDate        string     `json:"start_date"`
	EndDate          string     `json:"end_date"`
	Units            string     `json:"units"`
	UnitOfMeasure    string     `json:"unit_of_measure"`
	ApprovedBy       *string    `json:"approved_by,omitempty"`
	ApprovedAt       *time.Time `json:"approved_at,omitempty"`
	State            string     `json:"state"`
	QuarantineReason *string    `json:"quarantine_reason,omitempty"`
	LineHash         string     `json:"line_hash"`
}

// SourceLeave is one APPROVED leave request as the source system holds it.
type SourceLeave struct {
	RequestID     string
	WorkerID      string
	LeaveTypeID   string
	LeaveTypeCode string
	IsPaid        bool
	StartDate     string
	EndDate       string
	Units         string // exact decimal, as stored
	Status        string
	ApprovedBy    *string
	ApprovedAt    *time.Time
}

type ImportTimeLeaveBatchRequest struct {
	LegalEntityID   string `json:"legal_entity_id"`
	PayrollPeriodID string `json:"payroll_period_id"`
	PeriodStart     string `json:"period_start"`
	PeriodEnd       string `json:"period_end"`
	OnInvalid       string `json:"on_invalid,omitempty"`
}

// WithDefaults applies the conservative policy: an invalid line rejects the
// batch unless the caller explicitly chooses to quarantine lines.
func (r ImportTimeLeaveBatchRequest) WithDefaults() ImportTimeLeaveBatchRequest {
	if r.OnInvalid == "" {
		r.OnInvalid = OnInvalidRejectBatch
	}
	return r
}

func (r ImportTimeLeaveBatchRequest) Validate() error {
	if strings.TrimSpace(r.LegalEntityID) == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if strings.TrimSpace(r.PayrollPeriodID) == "" {
		return fmt.Errorf("payroll_period_id is required")
	}
	from, err := time.Parse("2006-01-02", r.PeriodStart)
	if err != nil {
		return fmt.Errorf("period_start must be YYYY-MM-DD")
	}
	to, err := time.Parse("2006-01-02", r.PeriodEnd)
	if err != nil {
		return fmt.Errorf("period_end must be YYYY-MM-DD")
	}
	if to.Before(from) {
		return fmt.Errorf("period_end must not be before period_start")
	}
	if r.OnInvalid != "" && r.OnInvalid != OnInvalidQuarantineLines && r.OnInvalid != OnInvalidRejectBatch {
		return fmt.Errorf("on_invalid must be QUARANTINE_LINES or REJECT_BATCH")
	}
	return nil
}

type BatchTransitionRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason,omitempty"`
	// OnInvalid optionally overrides the batch policy on supersede.
	OnInvalid string `json:"on_invalid,omitempty"`
}

// ClassifyLine decides whether a source leave can enter the batch as financial
// units. workerReason is the worker-context result ("" when valid). The checks
// are ordered so the most fundamental failure is the one recorded; nothing is
// ever defaulted or repaired.
func ClassifyLine(l SourceLeave, periodStart, periodEnd, workerReason string) string {
	switch {
	case l.Status != "APPROVED":
		return ReasonNotApproved
	case l.ApprovedBy == nil || strings.TrimSpace(*l.ApprovedBy) == "" || l.ApprovedAt == nil:
		return ReasonMissingApprovalEvidence
	case l.StartDate < periodStart || l.EndDate > periodEnd:
		// The source holds one total for the whole request; splitting it across
		// periods would invent a per-day allocation rule nobody has approved.
		return ReasonCrossPeriod
	case workerReason != "":
		return workerReason
	}
	return ""
}

// LineHash identifies the material content of one line so a changed source
// request is detectable after the snapshot was taken.
func LineHash(l SourceLeave) string {
	h := sha256.New()
	by, at := "", ""
	if l.ApprovedBy != nil {
		by = *l.ApprovedBy
	}
	if l.ApprovedAt != nil {
		at = l.ApprovedAt.UTC().Format(time.RFC3339Nano)
	}
	for _, f := range []string{l.RequestID, l.WorkerID, l.LeaveTypeID, l.StartDate, l.EndDate, l.Units, fmt.Sprint(l.IsPaid), by, at} {
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SourceHash is the batch-level control hash: over the ACCEPTED lines only,
// sorted by source line so the order of discovery never changes it.
func SourceHash(accepted []TimeLeaveLine) string {
	hashes := make([]string, 0, len(accepted))
	for _, l := range accepted {
		hashes = append(hashes, l.SourceLineID+"="+l.LineHash)
	}
	sort.Strings(hashes)
	h := sha256.New()
	for _, s := range hashes {
		fmt.Fprintf(h, "%d:%s|", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ── events ───────────────────────────────────────────────────────────────────

const (
	EventBatchReceived   = "time_leave_financial_batch_received"
	EventBatchAccepted   = "time_leave_financial_batch_accepted"
	EventLineQuarantined = "time_leave_line_quarantined"
	EventBatchFrozen     = "time_leave_financial_batch_frozen"
	EventBatchSuperseded = "time_leave_financial_batch_superseded" // beyond spec 3.2
	EventBatchRejected   = "time_leave_financial_batch_rejected"   // beyond spec 3.2
)

var EventCatalogue = map[string]bool{
	EventBatchReceived: true, EventBatchAccepted: true, EventLineQuarantined: true,
	EventBatchFrozen: true, EventBatchSuperseded: true, EventBatchRejected: true,
}

// OutboxEvent is one event to be written in the caller's transaction.
type OutboxEvent struct {
	TenantID  string
	ObjectID  string
	EventType string
	Payload   []byte // the fully built envelope
}

// SourceIssue is a difference between a batch line and its source request.
type SourceIssue struct {
	LineID       string `json:"line_id"`
	SourceLineID string `json:"source_line_id"`
	Issue        string `json:"issue"`
}

const (
	IssueSourceNotApproved = "SOURCE_NO_LONGER_APPROVED"
	IssueSourceChanged     = "SOURCE_CHANGED_SINCE_SNAPSHOT"
	IssueSourceMissing     = "SOURCE_MISSING"
)

func NewID() string { return uuid.NewString() }

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrBatchNotFound            = errorString("time/leave batch not found")
	ErrLineNotFound             = errorString("time/leave line not found")
	ErrBatchVersionConflict     = errorString("stale expected_version: the batch changed since it was read")
	ErrLiveBatchExists          = errorString("a live batch already exists for this entity and period; supersede it instead")
	ErrBatchNotAccepted         = errorString("only an ACCEPTED batch can be frozen or have lines quarantined")
	ErrBatchNotSupersedable     = errorString("only an ACCEPTED or FROZEN batch can be superseded")
	ErrLineNotAccepted          = errorString("only an ACCEPTED line can be quarantined")
	ErrSourceChanged            = errorString("source leave changed since the snapshot; supersede the batch")
	ErrSupersedeWouldReject     = errorString("the replacement batch would be REJECTED; the existing batch is left untouched")
	ErrWorkerContextUnavailable = errorString("worker financial context could not be verified")
)
