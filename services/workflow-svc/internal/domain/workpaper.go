package domain

import "time"

// Workpaper is AUD-07's own "Workpaper" — the documentation record for one
// audit procedure area. Locking is real: LockWorkpaper computes a content
// digest and the migration's own reject-mutation trigger then refuses any
// UPDATE/DELETE on this row or its child rows once status='LOCKED'. All
// subsequent additions route through WorkpaperAddendum instead.
type Workpaper struct {
	WorkpaperID           string     `json:"workpaper_id"`
	EngagementID          string     `json:"engagement_id"`
	TenantID              string     `json:"tenant_id"`
	Reference             string     `json:"reference"`
	Purpose               string     `json:"purpose"`
	Status                string     `json:"status"`
	Required              bool       `json:"required"`
	HasContradictionFlag  bool       `json:"has_contradiction_flag"`
	PreparedByPrincipalID *string    `json:"prepared_by_principal_id,omitempty"`
	PreparedAt            *time.Time `json:"prepared_at,omitempty"`
	ReviewedAt            *time.Time `json:"reviewed_at,omitempty"`
	LockDigest            *string    `json:"lock_digest,omitempty"`
	LockedByPrincipalID   *string    `json:"locked_by_principal_id,omitempty"`
	LockedAt              *time.Time `json:"locked_at,omitempty"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	CreatedAt             time.Time  `json:"created_at"`
}

const (
	WorkpaperDraft      = "DRAFT"
	WorkpaperInProgress = "IN_PROGRESS"
	WorkpaperPrepared   = "PREPARED"
	WorkpaperReviewed   = "REVIEWED"
	WorkpaperLocked     = "LOCKED"
)

type WorkpaperProcedure struct {
	ProcedureID string    `json:"procedure_id"`
	WorkpaperID string    `json:"workpaper_id"`
	TenantID    string    `json:"tenant_id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type WorkpaperResult struct {
	ResultID    string    `json:"result_id"`
	WorkpaperID string    `json:"workpaper_id"`
	TenantID    string    `json:"tenant_id"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

type WorkpaperConclusion struct {
	ConclusionID string    `json:"conclusion_id"`
	WorkpaperID  string    `json:"workpaper_id"`
	TenantID     string    `json:"tenant_id"`
	Description  string    `json:"description"`
	CreatedAt    time.Time `json:"created_at"`
}

type WorkpaperCrossReference struct {
	CrossReferenceID string    `json:"cross_reference_id"`
	FromWorkpaperID  string    `json:"from_workpaper_id"`
	ToWorkpaperID    string    `json:"to_workpaper_id"`
	TenantID         string    `json:"tenant_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// WorkpaperEvidenceLink records that a piece of AUD-06 evidence (owned by
// document-vault-svc, referenced here only by ID) supports this
// workpaper. ContradictionFlag is copied from document-vault-svc's own
// response at link time — "contradictory evidence linked," never
// silently dropped.
type WorkpaperEvidenceLink struct {
	LinkID              string    `json:"link_id"`
	WorkpaperID         string    `json:"workpaper_id"`
	TenantID            string    `json:"tenant_id"`
	EvidenceID          string    `json:"evidence_id"`
	ContradictionFlag   bool      `json:"contradiction_flag"`
	LinkedByPrincipalID string    `json:"linked_by_principal_id"`
	CreatedAt           time.Time `json:"created_at"`
}

// WorkpaperAddendum is the only way to add content to a LOCKED workpaper.
type WorkpaperAddendum struct {
	AddendumID         string    `json:"addendum_id"`
	WorkpaperID        string    `json:"workpaper_id"`
	TenantID           string    `json:"tenant_id"`
	AddedByPrincipalID string    `json:"added_by_principal_id"`
	Reason             string    `json:"reason"`
	Effect             string    `json:"effect"`
	Content            string    `json:"content"`
	AddedAt            time.Time `json:"added_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateWorkpaperParams struct {
	EngagementID, TenantID, Reference, Purpose, CreatedByPrincipalID, CorrelationID string
	Required                                                                        bool
}

type RecordProcedureParams struct {
	WorkpaperID, TenantID, Description string
}

type LinkWorkpaperEvidenceParams struct {
	WorkpaperID, TenantID, EvidenceID, LinkedByPrincipalID string
	ContradictionFlag                                      bool
}

type RecordResultParams struct {
	WorkpaperID, TenantID, Description string
}

type RecordConclusionParams struct {
	WorkpaperID, TenantID, Description string
}

type MarkWorkpaperPreparedParams struct {
	WorkpaperID, TenantID, ActorPrincipalID, CorrelationID string
}

type AddWorkpaperCrossReferenceParams struct {
	FromWorkpaperID, ToWorkpaperID, TenantID string
}

type LockWorkpaperParams struct {
	WorkpaperID, TenantID, ActorPrincipalID, CorrelationID string
}

type AddPostLockAddendumParams struct {
	WorkpaperID, TenantID, ActorPrincipalID, Reason, Effect, Content string
}

// ── errors ───────────────────────────────────────────────────────────────────

var ErrWorkpaperNotFound = errorString("workpaper not found")
var ErrWorkpaperInvalidState = errorString("workpaper is not in a state that permits this action")

// ErrWorkpaperLockRequiresContent is LockWorkpaper's own CAS predicate
// failure — the DB-enforced form of "purpose/procedure/result/conclusion
// mandatory."
var ErrWorkpaperLockRequiresContent = errorString("workpaper requires a purpose plus at least one procedure, result, and conclusion before it can be locked")

// ErrWorkpaperAddendumIncomplete is the real enforcement of "post-lock
// additions identify who/when/why/effect."
var ErrWorkpaperAddendumIncomplete = errorString("post-lock addendum requires a reason and an effect assessment")
