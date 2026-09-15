package domain

import (
	"errors"
	"time"
)

// AUD-06 Audit Evidence is a parallel table set referencing Document/
// DocumentVersion by ID, NOT new columns on those shared tables. Document/
// DocumentVersion are consumed by every domain on this platform (including
// workflow-svc's already-live AUD-01 acceptance-evidence verification) —
// audit-only columns (source/reliability/contradiction/custody) would
// pollute a contract every other caller also depends on. The existing
// GetDocument response shape (document_id, tenant_id, legal_entity_id,
// current_version) is untouched by this addition.

// AuditEvidence.StatusFlags is a small set of independent booleans rather
// than one mutually-exclusive enum — "states may coexist where orthogonal"
// (evidence can be both Evaluated AND Contradictory at once).
type AuditEvidence struct {
	EvidenceID              string              `json:"evidence_id"`
	DocumentID              string              `json:"document_id"`
	TenantID                string              `json:"tenant_id"`
	LegalEntityID           string              `json:"legal_entity_id"`
	EngagementID            string              `json:"engagement_id"`
	EvidenceSource          string              `json:"evidence_source"`
	AcquisitionMethod       string              `json:"acquisition_method"`
	StatusFlags             EvidenceStatusFlags `json:"status_flags"`
	RegisteredByPrincipalID string              `json:"registered_by_principal_id"`
	CorrelationID           string              `json:"correlation_id"`
	RegisteredAt            time.Time           `json:"registered_at"`
}

// EvidenceStatusFlags matches the doc's own "states may coexist" model.
type EvidenceStatusFlags struct {
	IntegrityChecked     bool `json:"integrity_checked"`
	Evaluated            bool `json:"evaluated"`
	Linked               bool `json:"linked"`
	AcceptedForProcedure bool `json:"accepted_for_procedure"`
	Restricted           bool `json:"restricted"`
	Contradictory        bool `json:"contradictory"`
	Quarantined          bool `json:"quarantined"`
}

// EvidenceVersion is append-only — SupersedeEvidence inserts a new row and
// sets ONLY superseded_by_evidence_version_id on the old one; every other
// column on an existing row is permanent.
type EvidenceVersion struct {
	EvidenceVersionID             string    `json:"evidence_version_id"`
	EvidenceID                    string    `json:"evidence_id"`
	DocumentVersionID             string    `json:"document_version_id"`
	SupersededByEvidenceVersionID *string   `json:"superseded_by_evidence_version_id,omitempty"`
	CreatedAt                     time.Time `json:"created_at"`
}

type EvidenceReliabilityAssessment struct {
	AssessmentID          string    `json:"assessment_id"`
	EvidenceID            string    `json:"evidence_id"`
	AssessedByPrincipalID string    `json:"assessed_by_principal_id"`
	ReliabilityRating     string    `json:"reliability_rating"`
	Rationale             string    `json:"rationale"`
	AssessedAt            time.Time `json:"assessed_at"`
}

type EvidenceProcedureLink struct {
	LinkID              string    `json:"link_id"`
	EvidenceID          string    `json:"evidence_id"`
	ProcedureRef        string    `json:"procedure_ref"`
	LinkedByPrincipalID string    `json:"linked_by_principal_id"`
	LinkedAt            time.Time `json:"linked_at"`
}

// EvidenceContradiction has NO delete path anywhere in this store — the
// absence itself is AUD-NEG-021's own mechanism ("contradictory evidence
// is deleted after conclusion drafted -> block deletion; retain
// contradiction and reviewer visibility").
type EvidenceContradiction struct {
	ContradictionID         string    `json:"contradiction_id"`
	EvidenceID              string    `json:"evidence_id"`
	ContradictingEvidenceID *string   `json:"contradicting_evidence_id,omitempty"`
	Description             string    `json:"description"`
	RecordedByPrincipalID   string    `json:"recorded_by_principal_id"`
	RecordedAt              time.Time `json:"recorded_at"`
}

type CustodyEntry struct {
	CustodyID        string    `json:"custody_id"`
	EvidenceID       string    `json:"evidence_id"`
	Action           string    `json:"action"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	OccurredAt       time.Time `json:"occurred_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type RegisterEvidenceParams struct {
	DocumentID, TenantID, LegalEntityID, EngagementID, EvidenceSource, AcquisitionMethod, RegisteredByPrincipalID, CorrelationID string
	DocumentVersionID                                                                                                            string
}

type VerifyIntegrityParams struct {
	EvidenceID, TenantID, ActorPrincipalID string
}

type AssessReliabilityParams struct {
	EvidenceID, TenantID, AssessedByPrincipalID, ReliabilityRating, Rationale string
}

type LinkToProcedureParams struct {
	EvidenceID, TenantID, ProcedureRef, LinkedByPrincipalID string
}

type RecordContradictionParams struct {
	EvidenceID, TenantID, Description, RecordedByPrincipalID string
	ContradictingEvidenceID                                  *string
}

type RestrictEvidenceParams struct {
	EvidenceID, TenantID, ActorPrincipalID string
}

type SupersedeEvidenceParams struct {
	EvidenceID, TenantID, NewDocumentVersionID, ActorPrincipalID string
}

type QuarantineEvidenceParams struct {
	EvidenceID, TenantID, Reason, ActorPrincipalID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrEvidenceNotFound          = errors.New("audit evidence not found")
	ErrEvidenceSourceRequired    = errors.New("evidence_source is required")
	ErrAcquisitionMethodRequired = errors.New("acquisition_method is required")
	ErrEvidenceVersionNotFound   = errors.New("evidence version not found")
)
