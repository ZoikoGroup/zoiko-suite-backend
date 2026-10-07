package domain

import (
	"errors"
	"time"
)

// DRC-02 Record Declaration & Classification (ZS-SVC-S-001 §4). A
// separate governed aggregate from Document — "document state and
// record state are separate; declaration captures an exact immutable
// version" (§0.1/§3.1). A record is created only by declaring one
// exact, already-committed DocumentVersion as governed evidence; once
// declared it "cannot be edited" (DRC-I04) — correction happens only
// through an explicit RecordRelationship to a new record.
//
// retention_schedule_ref is stored as a caller-supplied reference
// only. No retention-rule engine exists in this codebase (that is
// DRC-03, deliberately not built in this pass — the spec itself warns
// that wrong retention logic "can destroy evidence irreversibly," so
// disposition automation is explicitly a later, dry-run-first wave).

type RecordClass string

const (
	RecordClassAccountingWorkpaper RecordClass = "ACCOUNTING_WORKPAPER"
	RecordClassTaxReturnSupport    RecordClass = "TAX_RETURN_SUPPORT"
	RecordClassPayrollOutput       RecordClass = "PAYROLL_OUTPUT"
	RecordClassEmploymentRecord    RecordClass = "EMPLOYMENT_RECORD"
	RecordClassLegalContract       RecordClass = "LEGAL_CONTRACT"
	RecordClassLegalMatterRecord   RecordClass = "LEGAL_MATTER_RECORD"
	RecordClassComplianceEvidence  RecordClass = "COMPLIANCE_EVIDENCE"
	RecordClassCorporateRecord     RecordClass = "CORPORATE_RECORD"
)

func (c RecordClass) Valid() bool {
	switch c {
	case RecordClassAccountingWorkpaper, RecordClassTaxReturnSupport, RecordClassPayrollOutput,
		RecordClassEmploymentRecord, RecordClassLegalContract, RecordClassLegalMatterRecord,
		RecordClassComplianceEvidence, RecordClassCorporateRecord:
		return true
	}
	return false
}

// RecordState per §3.1/§4: a record is born DECLARED — "no mutable
// draft record." DISPOSED is schema-ready but unreachable by any
// command in this wave.
type RecordState string

const (
	RecordStateDeclared   RecordState = "DECLARED"
	RecordStateSuperseded RecordState = "SUPERSEDED"
	RecordStateDisposed   RecordState = "DISPOSED"
)

// RelationshipType is §4.5's evidentiary relationship vocabulary.
type RelationshipType string

const (
	RelationshipSupersedes  RelationshipType = "SUPERSEDES"
	RelationshipCorrects    RelationshipType = "CORRECTS"
	RelationshipAmends      RelationshipType = "AMENDS"
	RelationshipAttachesTo  RelationshipType = "ATTACHES_TO"
	RelationshipEvidences   RelationshipType = "EVIDENCES"
	RelationshipDerivedFrom RelationshipType = "DERIVED_FROM"
	RelationshipDuplicateOf RelationshipType = "DUPLICATE_OF"
)

func (t RelationshipType) Valid() bool {
	switch t {
	case RelationshipSupersedes, RelationshipCorrects, RelationshipAmends, RelationshipAttachesTo,
		RelationshipEvidences, RelationshipDerivedFrom, RelationshipDuplicateOf:
		return true
	}
	return false
}

// Record is DRC-02's core entity — immutable once declared, except for
// record_state, which moves forward exactly once (DECLARED ->
// SUPERSEDED) and only via a SUPERSEDES relationship.
type Record struct {
	RecordID              string      `json:"record_id"`
	TenantID              string      `json:"tenant_id"`
	LegalEntityID         string      `json:"legal_entity_id"`
	DocumentID            string      `json:"document_id"`
	DocumentVersionID     string      `json:"document_version_id"`
	RecordClass           RecordClass `json:"record_class"`
	JurisdictionScope     string      `json:"jurisdiction_scope"`
	BusinessContext       string      `json:"business_context"`
	RetentionScheduleRef  *string     `json:"retention_schedule_ref,omitempty"`
	RecordState           RecordState `json:"record_state"`
	DeclaredByPrincipalID string      `json:"declared_by_principal_id"`
	DeclarationReason     string      `json:"declaration_reason"`
	DeclaredAt            time.Time   `json:"declared_at"`
	CorrelationID         *string     `json:"correlation_id,omitempty"`
}

// RecordRelationship is one append-only, evidentiary link between two
// records. SourceRecordID is the new/acting record; TargetRecordID is
// the one being acted upon.
type RecordRelationship struct {
	RelationshipID       string           `json:"relationship_id"`
	TenantID             string           `json:"tenant_id"`
	SourceRecordID       string           `json:"source_record_id"`
	TargetRecordID       string           `json:"target_record_id"`
	RelationshipType     RelationshipType `json:"relationship_type"`
	CreatedByPrincipalID string           `json:"created_by_principal_id"`
	CreatedAt            time.Time        `json:"created_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

// DeclareRecordV2Params declares an exact document version as a
// governed record. Named V2 to avoid colliding with the existing
// DeclareRecordParams (the Document-row flag from migration 000005,
// unchanged).
type DeclareRecordV2Params struct {
	DocumentID            string
	DocumentVersionID     string
	LegalEntityID         string
	RecordClass           string
	JurisdictionScope     string
	BusinessContext       string
	RetentionScheduleRef  string
	DeclaredByPrincipalID string
	DeclarationReason     string
	CorrelationID         string
}

type CreateRecordRelationshipParams struct {
	SourceRecordID       string
	TargetRecordID       string
	RelationshipType     string
	CreatedByPrincipalID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrRecordNotFound                   = errors.New("record not found")
	ErrInvalidRecordClass               = errors.New("invalid record_class")
	ErrJurisdictionScopeRequired        = errors.New("jurisdiction_scope is required")
	ErrDocumentVersionNotFoundForRecord = errors.New("document version not found")
	ErrDocumentVersionAlreadyDeclared   = errors.New("this document version has already been declared as a record")
	ErrInvalidRelationshipType          = errors.New("invalid relationship_type")
	ErrRecordRelationshipSelfReference  = errors.New("a record cannot have a relationship to itself")
	ErrRecordAlreadySuperseded          = errors.New("record has already been superseded")
	ErrDuplicateRecordRelationship      = errors.New("this exact relationship between these two records has already been recorded")
)
