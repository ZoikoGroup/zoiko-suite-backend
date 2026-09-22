// Package domain defines canonical types for document-vault-svc.
// Field shape follows docs/architecture/01-backend.md §8.3: version history,
// access history, integrity validation, retention, jurisdiction-aware
// residency — a document is preserved as evidence, not merely stored.
package domain

import (
	"errors"
	"time"
)

type Classification string

const (
	ClassificationPublic       Classification = "PUBLIC"
	ClassificationInternal     Classification = "INTERNAL"
	ClassificationConfidential Classification = "CONFIDENTIAL"
	ClassificationRestricted   Classification = "RESTRICTED"
)

func (c Classification) Valid() bool {
	switch c {
	case ClassificationPublic, ClassificationInternal, ClassificationConfidential, ClassificationRestricted:
		return true
	}
	return false
}

type DocumentStatus string

const (
	StatusActive       DocumentStatus = "ACTIVE"
	StatusRetained     DocumentStatus = "RETAINED"
	StatusPurgePending DocumentStatus = "PURGE_PENDING"
	// StatusSuperseded is reached only via SupersedeDocument — mirrors
	// AUD-06's evidence_versions.superseded_by_evidence_version_id
	// forward-link pattern (migration 000003), now reused for documents
	// themselves (migration 000005).
	StatusSuperseded DocumentStatus = "SUPERSEDED"
	// StatusArchived is reached only via MoveToArchive (migration 000006).
	StatusArchived DocumentStatus = "ARCHIVED"
)

// CanDeclareRecord: only an ACTIVE document with no prior declaration.
// Once declared, migration 000005's own trigger blocks any further
// change to the declaration fields — this is DeclareRecord's one and
// only opportunity, not a repeatable action.
func CanDeclareRecord(d *Document) bool {
	return d.Status == StatusActive && d.DeclaredAt == nil
}

// CanSupersede: any document not already superseded. A declared record
// MAY be superseded (that is the whole point of the command — replacing
// an authoritative record with a corrected one); an undeclared document
// may be too.
func CanSupersede(d *Document) bool {
	return d.SupersededByDocumentID == nil
}

// CanArchive: ACTIVE or SUPERSEDED documents only — not already ARCHIVED
// or PURGE_PENDING. A document ends its active life in one of two ways
// (superseded by a replacement, or archived outright), and either may
// still be archived afterward.
func CanArchive(d *Document) bool {
	return d.Status == StatusActive || d.Status == StatusSuperseded
}

// CanRequestDisposition: any status except PURGE_PENDING itself — once a
// disposition request exists, migration 000006's trigger blocks a second
// one from silently replacing it.
func CanRequestDisposition(d *Document) bool {
	return d.DispositionRequestedAt == nil
}

type AccessType string

const (
	AccessMetadata AccessType = "METADATA"
	AccessDownload AccessType = "DOWNLOAD"
)

// Document is the current-state pointer/metadata record. The actual bytes and
// version lineage live in DocumentVersion rows — this row is mutable only for
// current_version/status/updated_at; everything evidentiary is append-only.
type Document struct {
	DocumentID           string         `json:"document_id"`
	TenantID             string         `json:"tenant_id"`
	LegalEntityID        string         `json:"legal_entity_id"`
	Title                string         `json:"title"`
	Classification       Classification `json:"classification"`
	RetentionPolicy      string         `json:"retention_policy"`
	ResidencyRegionCode  *string        `json:"residency_region_code,omitempty"`
	CurrentVersion       int            `json:"current_version"`
	Status               DocumentStatus `json:"status"`
	CreatedByPrincipalID string         `json:"created_by_principal_id"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`

	// DeclaredVersion/DeclaredAt/DeclaredByPrincipalID (Wave 2): set together,
	// exactly once, by DeclareRecord — "record declaration tracked
	// separately" per the doc's own words, i.e. orthogonal to Status, not a
	// value of it. migration 000005's trigger blocks any further change.
	DeclaredVersion       *int       `json:"declared_version,omitempty"`
	DeclaredAt            *time.Time `json:"declared_at,omitempty"`
	DeclaredByPrincipalID *string    `json:"declared_by_principal_id,omitempty"`
	// SupersededByDocumentID (Wave 2): set exactly once by SupersedeDocument.
	// Forward link only — the new document's own row never points back.
	SupersededByDocumentID *string `json:"superseded_by_document_id,omitempty"`

	// ArchivedAt/ArchivedByPrincipalID/ArchiveReason (Wave 3): set together
	// by MoveToArchive.
	ArchivedAt            *time.Time `json:"archived_at,omitempty"`
	ArchivedByPrincipalID *string    `json:"archived_by_principal_id,omitempty"`
	ArchiveReason         *string    `json:"archive_reason,omitempty"`
	// DispositionRequestedAt/DispositionRequestedByPrincipalID/DispositionReason
	// (Wave 3): set together by RequestDisposition. This service records
	// the request only — DATA-GOV owns the actual disposition/purge
	// decision, per BIZ-01's own dependency line.
	DispositionRequestedAt            *time.Time `json:"disposition_requested_at,omitempty"`
	DispositionRequestedByPrincipalID *string    `json:"disposition_requested_by_principal_id,omitempty"`
	DispositionReason                 *string    `json:"disposition_reason,omitempty"`
}

// DocumentVersion is one immutable entry in a document's lineage. Rows are
// INSERTed only — never updated, never deleted (§8.3 "version history").
type DocumentVersion struct {
	DocumentVersionID    string    `json:"document_version_id"`
	DocumentID           string    `json:"document_id"`
	Version              int       `json:"version"`
	ChecksumSHA256       string    `json:"checksum_sha256"`
	StorageKey           string    `json:"storage_key"`
	SizeBytes            int64     `json:"size_bytes"`
	ContentType          string    `json:"content_type"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
}

// DocumentAccessLog is one append-only record of a read (§8.3 "access
// history"). Same doctrine as authorization-svc's access_decision_log: never
// updated, never deleted.
type DocumentAccessLog struct {
	AccessLogID           string     `json:"access_log_id"`
	DocumentID            string     `json:"document_id"`
	DocumentVersionID     *string    `json:"document_version_id,omitempty"`
	AccessedByPrincipalID string     `json:"accessed_by_principal_id"`
	AccessType            AccessType `json:"access_type"`
	CorrelationID         *string    `json:"correlation_id,omitempty"`
	AccessedAt            time.Time  `json:"accessed_at"`
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type CreateDocumentRequest struct {
	TenantID            string         `json:"tenant_id"`
	LegalEntityID       string         `json:"legal_entity_id"`
	Title               string         `json:"title"`
	Classification      Classification `json:"classification"`
	RetentionPolicy     string         `json:"retention_policy,omitempty"`
	ResidencyRegionCode *string        `json:"residency_region_code,omitempty"`
	ContentType         string         `json:"content_type"`
	ContentBase64       string         `json:"content_base64"`
}

type CreateDocumentVersionRequest struct {
	ContentType   string `json:"content_type"`
	ContentBase64 string `json:"content_base64"`
}

type DocumentResponse struct {
	Document Document `json:"document"`
}

// DeclareRecordParams is DeclareRecord's input — declares the document's
// CURRENT version (as of the call) the authoritative record. There is no
// "declare a specific past version" mode: only the live version can
// become the declared record, matching how AddVersion is the only way
// current_version ever advances.
type DeclareRecordParams struct {
	DocumentID            string
	DeclaredByPrincipalID string
}

// SupersedeDocumentParams is SupersedeDocument's input — marks DocumentID
// as superseded by SupersededByDocumentID, an already-existing document.
// This never creates the new document; the caller creates it first via
// the normal CreateDocument path, then links the two.
type SupersedeDocumentParams struct {
	DocumentID             string
	SupersededByDocumentID string
	ActorPrincipalID       string
}

// MoveToArchiveParams is MoveToArchive's input.
type MoveToArchiveParams struct {
	DocumentID            string
	ArchivedByPrincipalID string
	Reason                string
}

// RequestDispositionParams is RequestDisposition's input — see
// Document.DispositionRequestedAt's own doc comment: this only records
// the request, it never performs a purge.
type RequestDispositionParams struct {
	DocumentID             string
	RequestedByPrincipalID string
	Reason                 string
}

// DigestVerification is VerifyDigest's result — a pass/fail integrity
// check, never the content itself (that disclosure stays GetContent's).
type DigestVerification struct {
	DocumentID     string `json:"document_id"`
	Version        int    `json:"version"`
	ChecksumSHA256 string `json:"checksum_sha256"`
	Verified       bool   `json:"verified"`
}

// DocumentLink is one append-only row answering BIZ-01's GetLinkedObjects
// query — see migration 000007's own doc comment. LinkedObjectType/
// LinkedObjectID are opaque, caller-supplied identifiers for a business
// object this service has no schema access to (e.g. "EXPENSE_CLAIM" /
// the expense claim's own ID in expense-claim-svc).
type DocumentLink struct {
	LinkID              string    `json:"link_id"`
	DocumentID          string    `json:"document_id"`
	LinkedObjectType    string    `json:"linked_object_type"`
	LinkedObjectID      string    `json:"linked_object_id"`
	LinkedByPrincipalID string    `json:"linked_by_principal_id"`
	CorrelationID       *string   `json:"correlation_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

// LinkDocumentParams is LinkDocument's input.
type LinkDocumentParams struct {
	DocumentID          string
	LinkedObjectType    string
	LinkedObjectID      string
	LinkedByPrincipalID string
	CorrelationID       string
}

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

var (
	ErrDocumentNotFound        = errors.New("document not found")
	ErrDocumentVersionNotFound = errors.New("document version not found")
	ErrInvalidClassification   = errors.New("invalid classification")
	ErrEmptyContent            = errors.New("document content must not be empty")
	ErrChecksumMismatch        = errors.New("checksum mismatch — stored content failed integrity verification")
	ErrRetentionActive         = errors.New("document is under an active retention policy and cannot be purged")
	ErrResidencyViolation      = errors.New("document access would violate its residency constraint")
	ErrStoreUnavailable        = errors.New("document store unavailable")

	// ErrIdentityMissing is returned when a request carries no resolved
	// principal.
	//
	// There was no such error, and no such check. The handler read
	// X-Actor-Principal-ID, fell back to X-Principal-Id, and fell back again to
	// the literal string "unknown" — so an unidentified caller was not refused,
	// it was RECORDED, and the append-only access log that exists to answer
	// "who downloaded this RESTRICTED document" could answer "unknown" and read
	// as though it had answered.
	ErrIdentityMissing = errors.New("caller identity missing")

	// ErrTenantMissing is returned when a request carries no X-Tenant-Id.
	//
	// Distinct from ErrIdentityMissing so a forgotten tenant header is not
	// reported as a missing principal. This was the second half of the same
	// hole: the store's predicate was `($2::uuid IS NULL OR tenant_id = $2)`,
	// which evaluates TRUE for every row when no tenant is supplied — a filter
	// that switches itself off when omitted rather than refusing.
	ErrTenantMissing = errors.New("tenant context missing")

	// ErrAuthorizationDenied is an explicit DENIED from authorization-svc.
	ErrAuthorizationDenied = errors.New("not authorized for this document action")

	// ErrAuthzServiceUnavailable covers every non-decision from
	// authorization-svc. Callers must treat it as a refusal.
	ErrAuthzServiceUnavailable = errors.New("authorization-svc unavailable")

	// ErrDocumentAlreadyDeclared backs DeclareRecord — see CanDeclareRecord.
	ErrDocumentAlreadyDeclared = errors.New("document is already a declared record")
	// ErrDocumentNotActive backs DeclareRecord — only an ACTIVE document
	// may be declared.
	ErrDocumentNotActive = errors.New("document is not ACTIVE")
	// ErrDocumentAlreadySuperseded backs SupersedeDocument — see CanSupersede.
	ErrDocumentAlreadySuperseded = errors.New("document has already been superseded")
	// ErrSupersedingDocumentNotFound backs SupersedeDocument — the
	// replacement document must already exist.
	ErrSupersedingDocumentNotFound = errors.New("superseding document not found")
	// ErrCannotSupersedeSelf backs SupersedeDocument.
	ErrCannotSupersedeSelf = errors.New("a document cannot supersede itself")
	// ErrDocumentNotArchivable backs MoveToArchive — see CanArchive.
	ErrDocumentNotArchivable = errors.New("document is not in a state that can be archived")
	// ErrDispositionAlreadyRequested backs RequestDisposition — see
	// CanRequestDisposition.
	ErrDispositionAlreadyRequested = errors.New("a disposition request already exists for this document")
	// ErrDuplicateLink backs LinkDocument — the same document already
	// linked to the same object is a caller bug, not a new fact.
	ErrDuplicateLink = errors.New("this document is already linked to that object")

	// ErrInvalidPaging is returned for an out-of-range limit or offset.
	ErrInvalidPaging = errors.New("limit must be between 1 and 500 and offset must not be negative")
)
