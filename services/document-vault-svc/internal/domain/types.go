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
)

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
)
