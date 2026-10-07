package domain

import (
	"errors"
	"time"
)

// DRC-04 Preservation, Rendition & Content Integrity (ZS-SVC-S-001 §6).
// A rendition is always a distinct object from the document_version it
// derives from (DRC-I15) — never a rewrite of the original. Every
// preservation/access/redacted rendition gets its own fixity_manifest
// (DRC-I20), whose repair path must restore independently verified
// bytes rather than ever rewriting metadata to fake a hash match.

type RenditionClass string

const (
	RenditionPreservation RenditionClass = "PRESERVATION_RENDITION"
	RenditionAccess       RenditionClass = "ACCESS_RENDITION"
	RenditionRedacted     RenditionClass = "REDACTED_RENDITION"
	RenditionOCRText      RenditionClass = "OCR_TEXT"
	RenditionNormalized   RenditionClass = "NORMALIZED_DATA"
)

func (c RenditionClass) Valid() bool {
	switch c {
	case RenditionPreservation, RenditionAccess, RenditionRedacted, RenditionOCRText, RenditionNormalized:
		return true
	}
	return false
}

// Rendition is a derivative content object — a preservation copy, an
// access copy, a redacted copy, OCR text, or normalized data — each
// with its own hash and storage location. source_document_version_id
// always points at the ORIGINAL (document_versions, DRC-01);
// parent_rendition_id optionally points at another rendition this one
// was built from (e.g. a REDACTED_RENDITION built from an
// ACCESS_RENDITION), so lineage is never ambiguous.
type Rendition struct {
	RenditionID             string         `json:"rendition_id"`
	TenantID                string         `json:"tenant_id"`
	SourceDocumentVersionID string         `json:"source_document_version_id"`
	ParentRenditionID       *string        `json:"parent_rendition_id,omitempty"`
	RenditionClass          RenditionClass `json:"rendition_class"`
	TransformationProfile   string         `json:"transformation_profile"`
	ChecksumSHA256          string         `json:"checksum_sha256"`
	StorageKey              string         `json:"storage_key"`
	SizeBytes               int64          `json:"size_bytes"`
	ContentType             string         `json:"content_type"`
	CreatedByPrincipalID    string         `json:"created_by_principal_id"`
	CreatedAt               time.Time      `json:"created_at"`
}

type CreateRenditionParams struct {
	SourceDocumentVersionID string
	ParentRenditionID       *string
	RenditionClass          string
	TransformationProfile   string
	ChecksumSHA256          string
	StorageKey              string
	SizeBytes               int64
	ContentType             string
	CreatedByPrincipalID    string
}

// FixityIntegrityState is §6's fixity state graph. A failure state
// (MISMATCH/MISSING/UNREADABLE) can only be exited via REPAIRING —
// never a bare re-verification — so a known-bad manifest is always
// visibly under repair before it can claim VERIFIED again.
type FixityIntegrityState string

const (
	FixityVerified   FixityIntegrityState = "VERIFIED"
	FixityMismatch   FixityIntegrityState = "MISMATCH"
	FixityMissing    FixityIntegrityState = "MISSING"
	FixityUnreadable FixityIntegrityState = "UNREADABLE"
	FixityRepairing  FixityIntegrityState = "REPAIRING"
)

func (s FixityIntegrityState) Valid() bool {
	switch s {
	case FixityVerified, FixityMismatch, FixityMissing, FixityUnreadable, FixityRepairing:
		return true
	}
	return false
}

func (s FixityIntegrityState) IsFailure() bool {
	switch s {
	case FixityMismatch, FixityMissing, FixityUnreadable:
		return true
	}
	return false
}

// FixityManifest is the integrity control for exactly one content
// object — either a document_version or a rendition, never both.
type FixityManifest struct {
	ManifestID             string               `json:"manifest_id"`
	TenantID               string               `json:"tenant_id"`
	DocumentVersionID      *string              `json:"document_version_id,omitempty"`
	RenditionID            *string              `json:"rendition_id,omitempty"`
	SourceHashAlgorithm    string               `json:"source_hash_algorithm"`
	SourceHash             string               `json:"source_hash"`
	IntegrityState         FixityIntegrityState `json:"integrity_state"`
	GeneratedAt            time.Time            `json:"generated_at"`
	GeneratedByPrincipalID string               `json:"generated_by_principal_id"`
	LastVerifiedAt         *time.Time           `json:"last_verified_at,omitempty"`
	LastVerifiedHash       *string              `json:"last_verified_hash,omitempty"`
	RepairSourceRef        *string              `json:"repair_source_ref,omitempty"`
	RepairStartedAt        *time.Time           `json:"repair_started_at,omitempty"`
	RepairedByPrincipalID  *string              `json:"repaired_by_principal_id,omitempty"`
}

// RecordVerificationParams records the outcome of checking an object's
// bytes against its manifest's source_hash. Allowed only when the
// manifest is currently VERIFIED (routine re-check) or REPAIRING (the
// repair attempt's own re-verification). Claiming outcome VERIFIED
// requires ObservedHash to actually equal the manifest's source_hash —
// the store enforces this itself rather than trusting the caller's
// claim, which is what makes "repair must restore correct bytes,
// never rewrite metadata to fake a hash match" a structural guarantee
// and not just a documented intention.
type RecordVerificationParams struct {
	ManifestID            string
	ObservedHash          string
	ClaimedOutcome        string
	VerifiedByPrincipalID string
}

// StartRepairParams moves a failed manifest into REPAIRING. Only valid
// from MISMATCH/MISSING/UNREADABLE — a manifest that has never failed
// has nothing to repair.
type StartRepairParams struct {
	ManifestID           string
	RepairSourceRef      string
	StartedByPrincipalID string
}

// RedactionProfile ties a REDACTED_RENDITION to the purpose, recipient
// and authority that justified removing content — the unredacted
// source itself is never touched (DRC-I17).
type RedactionProfile struct {
	RedactionID           string    `json:"redaction_id"`
	TenantID              string    `json:"tenant_id"`
	RenditionID           string    `json:"rendition_id"`
	Purpose               string    `json:"purpose"`
	RecipientClass        string    `json:"recipient_class"`
	FieldsRemoved         []string  `json:"fields_removed"`
	LegalBasisRef         string    `json:"legal_basis_ref,omitempty"`
	ApprovedByPrincipalID string    `json:"approved_by_principal_id"`
	CreatedAt             time.Time `json:"created_at"`
}

type CreateRedactionProfileParams struct {
	RenditionID           string
	Purpose               string
	RecipientClass        string
	FieldsRemoved         []string
	LegalBasisRef         string
	ApprovedByPrincipalID string
}

// ExportPackage is a sealed, point-in-time manifest of exactly what
// left the vault in one export (DRC-I27). PackageHash is computed by
// the store from the ordered item hashes — never caller-supplied.
type ExportPackage struct {
	PackageID              string              `json:"package_id"`
	TenantID               string              `json:"tenant_id"`
	RequestedByPrincipalID string              `json:"requested_by_principal_id"`
	PackageHash            string              `json:"package_hash"`
	CreatedAt              time.Time           `json:"created_at"`
	Items                  []ExportPackageItem `json:"items,omitempty"`
}

type ExportItemType string

const (
	ExportItemDocumentVersion ExportItemType = "DOCUMENT_VERSION"
	ExportItemRendition       ExportItemType = "RENDITION"
)

func (t ExportItemType) Valid() bool {
	return t == ExportItemDocumentVersion || t == ExportItemRendition
}

type ExportPackageItem struct {
	ItemID            string         `json:"item_id"`
	TenantID          string         `json:"tenant_id"`
	PackageID         string         `json:"package_id"`
	ItemType          ExportItemType `json:"item_type"`
	DocumentVersionID *string        `json:"document_version_id,omitempty"`
	RenditionID       *string        `json:"rendition_id,omitempty"`
	RedactionID       *string        `json:"redaction_id,omitempty"`
	Included          bool           `json:"included"`
	IncludedHash      *string        `json:"included_hash,omitempty"`
	OmissionReason    *string        `json:"omission_reason,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
}

// ExportPackageItemInput is one requested item of an export — either
// included (in which case the store resolves its current hash from
// the referenced version/rendition) or deliberately omitted (in which
// case OmissionReason is required and no hash is recorded).
type ExportPackageItemInput struct {
	ItemType       string
	RefID          string
	RedactionID    *string
	Omit           bool
	OmissionReason string
}

type CreateExportPackageParams struct {
	RequestedByPrincipalID string
	Items                  []ExportPackageItemInput
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrInvalidRenditionClass         = errors.New("invalid rendition_class")
	ErrSourceDocumentVersionNotFound = errors.New("source document version not found")
	ErrParentRenditionNotFound       = errors.New("parent rendition not found")

	ErrFixityManifestNotFound       = errors.New("fixity manifest not found")
	ErrFixityNotVerifiedOrRepairing = errors.New("fixity manifest must be VERIFIED or REPAIRING to record a verification")
	ErrFixityNotFailed              = errors.New("fixity manifest must be MISMATCH, MISSING or UNREADABLE to start a repair")
	ErrFixityInvalidOutcome         = errors.New("invalid verification outcome")
	ErrFixityHashMismatchClaim      = errors.New("claimed outcome VERIFIED requires observed_hash to equal the manifest's source_hash")

	ErrRenditionNotFound         = errors.New("rendition not found")
	ErrRenditionNotRedactedClass = errors.New("rendition is not a REDACTED_RENDITION")
	ErrRedactionProfileExists    = errors.New("this rendition already has a redaction profile")

	ErrExportPackageNotFound        = errors.New("export package not found")
	ErrNoExportPackageItems         = errors.New("at least one item is required")
	ErrInvalidExportItemType        = errors.New("invalid item_type")
	ErrExportItemRefNotFound        = errors.New("export item reference not found")
	ErrExportOmissionReasonRequired = errors.New("omission_reason is required when an item is omitted")
	ErrExportRedactionNotFound      = errors.New("referenced redaction profile not found")
)
