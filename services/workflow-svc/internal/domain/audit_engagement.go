package domain

import "time"

// AuditEngagement is the authoritative AUD-01 engagement record.  It lives
// beside generic workflow execution because this deployment already owns
// governed lifecycle transitions; it is deliberately not represented as an
// unstructured workflow payload.
type AuditEngagement struct {
	EngagementID              string     `json:"engagement_id"`
	TenantID                  string     `json:"tenant_id"`
	LegalEntityID             string     `json:"legal_entity_id"`
	EngagementCode            string     `json:"engagement_code"`
	EngagementType            string     `json:"engagement_type"`
	ReportingPeriodStart      time.Time  `json:"reporting_period_start"`
	ReportingPeriodEnd        time.Time  `json:"reporting_period_end"`
	FrameworkProfileID        string     `json:"framework_profile_id"`
	FrameworkProfileVersion   string     `json:"framework_profile_version"`
	MethodologyID             string     `json:"methodology_id"`
	MethodologyVersion        string     `json:"methodology_version"`
	ResponsiblePartnerID      string     `json:"responsible_partner_id"`
	ScopeSummary              string     `json:"scope_summary"`
	AcceptanceDocumentID      *string    `json:"acceptance_document_id,omitempty"`
	AcceptanceDocumentVersion *int       `json:"acceptance_document_version,omitempty"`
	Status                    string     `json:"status"`
	ScopeVersion              int        `json:"scope_version"`
	CreatedByPrincipalID      string     `json:"created_by_principal_id"`
	CreatedAt                 time.Time  `json:"created_at"`
	EffectiveFrom             time.Time  `json:"effective_from"`
	EffectiveTo               *time.Time `json:"effective_to,omitempty"`
}

const (
	AuditEngagementProposed          = "PROPOSED"
	AuditEngagementAcceptanceReview  = "ACCEPTANCE_REVIEW"
	AuditEngagementAccepted          = "ACCEPTED"
	AuditEngagementActive            = "ACTIVE"
	AuditEngagementFieldworkComplete = "FIELDWORK_COMPLETE"
	AuditEngagementCompletionReview  = "COMPLETION_REVIEW"
	AuditEngagementReportReady       = "REPORT_READY"
	AuditEngagementReleased          = "RELEASED"
	AuditEngagementClosed            = "CLOSED"
	AuditEngagementWithdrawn         = "WITHDRAWN"
)

type CreateAuditEngagementParams struct {
	TenantID, LegalEntityID, EngagementCode, EngagementType  string
	FrameworkProfileID, FrameworkProfileVersion              string
	MethodologyID, MethodologyVersion                        string
	ResponsiblePartnerID, ScopeSummary, CreatedByPrincipalID string
	ReportingPeriodStart, ReportingPeriodEnd                 time.Time
	CorrelationID                                            string
}

type SubmitAuditEngagementAcceptanceParams struct {
	EngagementID, TenantID, EvidenceDocumentID, ActorPrincipalID, CorrelationID string
	EvidenceDocumentVersion                                                     int
}

type TransitionAuditEngagementParams struct {
	EngagementID, TenantID, ActorPrincipalID, CorrelationID string
	ExpectedStatuses                                        []string
	NextStatus                                              string
}

// AmendAuditEngagementScopeParams bumps scope_version — the mechanism
// behind "scope/framework changes invalidate dependent approvals" (see
// PgStore.AmendAuditEngagementScope's own doc comment).
type AmendAuditEngagementScopeParams struct {
	EngagementID, TenantID, ActorPrincipalID, CorrelationID   string
	ScopeSummary, FrameworkProfileID, FrameworkProfileVersion string
	MethodologyID, MethodologyVersion                         string
}

// CompletionGate is one named precondition for a lifecycle transition,
// returned so a caller blocked by MarkFieldworkComplete/MarkReportReady
// can see exactly why, not just a generic 422.
type CompletionGate struct {
	Name      string `json:"name"`
	Satisfied bool   `json:"satisfied"`
}

var ErrAuditEngagementNotFound = errorString("audit engagement not found")
var ErrAuditEngagementDuplicateCode = errorString("audit engagement code already exists for this entity")
var ErrAuditEngagementInvalidState = errorString("audit engagement is not in a state that permits this action")
var ErrAuditAcceptanceEvidenceRequired = errorString("acceptance evidence document is required")
var ErrAuditAcceptanceEvidenceUnavailable = errorString("acceptance evidence cannot be verified")
var ErrAuditAcceptanceEvidenceInvalid = errorString("acceptance evidence does not belong to this engagement scope")
var ErrAuditEngagementSelfAcceptance = errorString("engagement creator may not record the acceptance decision")

// ErrAuditEngagementGateBlocked is MarkFieldworkComplete/MarkReportReady's
// own gate-check failure — see the accompanying []CompletionGate for which
// gate(s) are unsatisfied.
var ErrAuditEngagementGateBlocked = errorString("engagement completion gates are not satisfied")
