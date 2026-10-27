package domain

import "time"

// Workflow-ref commands: the ACC-14 decisions REF-05 (accounting-period-svc)
// accepts as provenance for a period state change.
const (
	WorkflowCmdSoftClose       = "SOFT_CLOSE"
	WorkflowCmdHardClose       = "HARD_CLOSE"
	WorkflowCmdAuthorizeReopen = "AUTHORIZE_REOPEN"
	WorkflowCmdReclose         = "RECLOSE"

	WorkflowStatusApproved = "APPROVED"
	WorkflowStatusRejected = "REJECTED"
)

// ErrWorkflowRefNotFound is returned for an unknown ref, a malformed ref, and a
// ref owned by another tenant alike: the three are deliberately indistinguishable.
const ErrWorkflowRefNotFound = errorString("workflow ref not found")

// WorkflowRef is one row of close_workflow_refs: ACC-14's append-only record
// that a close/reopen command was decided for a period, which REF-05 calls back
// to verify before it changes its own state. See migration 000015.
type WorkflowRef struct {
	RefID              string    `json:"ref_id"`
	TenantID           string    `json:"tenant_id"`
	LegalEntityID      string    `json:"legal_entity_id"`
	FiscalPeriodID     string    `json:"fiscal_period_id"`
	PeriodName         string    `json:"period_name"`
	PeriodKey          string    `json:"period_key"`
	Command            string    `json:"command"`
	Status             string    `json:"status"`
	ControlSnapshotRef string    `json:"control_snapshot_ref"`
	RequestedBy        string    `json:"requested_by"`
	Reason             string    `json:"reason"`
	CreatedAt          time.Time `json:"created_at"`
}

// WorkflowRefResponse is the exact wire body of GET /v1/close/workflow-refs/{ref}.
type WorkflowRefResponse struct {
	WorkflowRef        string `json:"workflow_ref"`
	PeriodKey          string `json:"period_key"`
	LegalEntityID      string `json:"legal_entity_id"`
	Command            string `json:"command"`
	Status             string `json:"status"`
	ControlSnapshotRef string `json:"control_snapshot_ref"`
}

// MirrorActionResult is one step of a REF-05 mirror/replay.
type MirrorActionResult struct {
	Command string `json:"command"`
	Outcome string `json:"outcome"` // applied | skipped | failed
	Ref     string `json:"ref"`
	Reason  string `json:"reason,omitempty"`
}

// MirrorReplayResponse is the body of POST /v1/close/periods/{id}:mirror-to-period-service.
type MirrorReplayResponse struct {
	FiscalPeriodID string               `json:"fiscal_period_id"`
	LegacyState    string               `json:"legacy_state"`
	Actions        []MirrorActionResult `json:"actions"`
}
