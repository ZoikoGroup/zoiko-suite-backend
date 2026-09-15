package domain

import "time"

// AUD-05 PBC / Evidence Request is the request/response half this service
// was missing: EvidenceRequirement/EvidenceEvaluation (above) answer "does
// sufficient evidence exist," but nothing tracked "ask a specific party for
// it, by when, and record what came back." PBC requests may reference an
// EvidenceRequirement (RequirementID nil = a free-standing request not tied
// to the catalog), and SubmitResponse's own artifact reference is verified
// through the SAME documentvault.Client this service already uses for
// present-artifact checks.

type EvidenceRequestStatus string

const (
	EvidenceRequestDraft                 EvidenceRequestStatus = "DRAFT"
	EvidenceRequestSent                  EvidenceRequestStatus = "SENT"
	EvidenceRequestViewed                EvidenceRequestStatus = "VIEWED"
	EvidenceRequestResponseReceived      EvidenceRequestStatus = "RESPONSE_RECEIVED"
	EvidenceRequestUnderEvaluation       EvidenceRequestStatus = "UNDER_EVALUATION"
	EvidenceRequestSatisfied             EvidenceRequestStatus = "SATISFIED"
	EvidenceRequestClarificationRequired EvidenceRequestStatus = "CLARIFICATION_REQUIRED"
	EvidenceRequestClosed                EvidenceRequestStatus = "CLOSED"
)

// EvidenceRequest is AUD-05's own "EvidenceRequest" — IsOverdue is
// deliberately NOT a stored column: it is derived at read time from
// due_at/status, so it can never drift from the truth those two columns
// already carry.
type EvidenceRequest struct {
	RequestID             string                `json:"request_id"`
	TenantID              string                `json:"tenant_id"`
	LegalEntityID         string                `json:"legal_entity_id"`
	RequirementID         *string               `json:"requirement_id,omitempty"`
	Title                 string                `json:"title"`
	Description           string                `json:"description"`
	AssignedToPrincipalID string                `json:"assigned_to_principal_id"`
	DueAt                 time.Time             `json:"due_at"`
	Status                EvidenceRequestStatus `json:"status"`
	CreatedByPrincipalID  string                `json:"created_by_principal_id"`
	CorrelationID         string                `json:"correlation_id"`
	CreatedAt             time.Time             `json:"created_at"`
	IsOverdue             bool                  `json:"is_overdue"`
}

// EvidenceRequestResponse is append-only — SubmitResponse always inserts
// a new version, never updates a prior one. MalwareScanStatus starts
// PENDING and MarkSatisfied refuses unless the latest response is CLEAN.
type EvidenceRequestResponse struct {
	ResponseID             string    `json:"response_id"`
	RequestID              string    `json:"request_id"`
	Version                int       `json:"version"`
	SubmittedByPrincipalID string    `json:"submitted_by_principal_id"`
	ArtifactDocumentID     *string   `json:"artifact_document_id,omitempty"`
	MalwareScanStatus      string    `json:"malware_scan_status"`
	CreatedAt              time.Time `json:"created_at"`
}

const (
	MalwareScanPending     = "PENDING"
	MalwareScanClean       = "CLEAN"
	MalwareScanQuarantined = "QUARANTINED"
)

// EvidenceRequestNote's Visibility controls whether a client contributor
// can see it — "audit-only notes hidden from client contributor."
type EvidenceRequestNote struct {
	NoteID            string    `json:"note_id"`
	RequestID         string    `json:"request_id"`
	AuthorPrincipalID string    `json:"author_principal_id"`
	Body              string    `json:"body"`
	Visibility        string    `json:"visibility"`
	CreatedAt         time.Time `json:"created_at"`
}

const (
	NoteVisibilityShared    = "SHARED"
	NoteVisibilityAuditOnly = "AUDIT_ONLY"
)

type EvidenceRequestReceipt struct {
	ReceiptID                 string    `json:"receipt_id"`
	ResponseID                string    `json:"response_id"`
	AcknowledgedByPrincipalID string    `json:"acknowledged_by_principal_id"`
	AcknowledgedAt            time.Time `json:"acknowledged_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateEvidenceRequestParams struct {
	TenantID, LegalEntityID, Title, Description, AssignedToPrincipalID, CreatedByPrincipalID, CorrelationID string
	RequirementID                                                                                           *string
	DueAt                                                                                                   time.Time
}

type SendEvidenceRequestParams struct {
	RequestID, TenantID, CorrelationID string
}

type ReassignEvidenceRequestParams struct {
	RequestID, TenantID, NewAssigneePrincipalID string
}

type ExtendDueDateParams struct {
	RequestID, TenantID string
	NewDueAt            time.Time
}

type ViewEvidenceRequestParams struct {
	RequestID, TenantID string
}

type SubmitResponseParams struct {
	RequestID, TenantID, SubmittedByPrincipalID, CorrelationID string
	ArtifactDocumentID                                         *string
}

type ScanResultParams struct {
	ResponseID, TenantID, Result string
}

type AcknowledgeReceiptParams struct {
	ResponseID, TenantID, AcknowledgedByPrincipalID string
}

type AddRequestNoteParams struct {
	RequestID, TenantID, AuthorPrincipalID, Body, Visibility string
}

type RequestClarificationParams struct {
	RequestID, TenantID, Reason string
}

type MarkSatisfiedParams struct {
	RequestID, TenantID, ActorPrincipalID string
}

type CloseEvidenceRequestParams struct {
	RequestID, TenantID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrEvidenceRequestNotFound     = errorString("evidence request not found")
	ErrEvidenceRequestInvalidState = errorString("evidence request is not in a state that permits this action")
	ErrResponseNotFound            = errorString("evidence request response not found")

	// ErrSelfEvaluationNotAllowed is AUD-NEG-016's own mechanism: the
	// principal who submitted a response may not be the one who marks it
	// satisfied — enforced both in the handler and as a CAS WHERE clause
	// in the store, so no second code path can bypass it.
	ErrSelfEvaluationNotAllowed = errorString("the principal who submitted this response may not mark it satisfied")

	// ErrArtifactNotScanned is the malware-scan gate: MarkSatisfied
	// refuses unless the latest response's own artifact has been scanned
	// CLEAN.
	ErrArtifactNotScanned = errorString("the latest response's artifact has not been confirmed clean by malware scanning")
)
