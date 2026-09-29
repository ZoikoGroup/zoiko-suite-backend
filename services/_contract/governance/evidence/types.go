package evidence

import (
	"time"

	"zoiko.io/contract/types"
)

// EvidenceObjectType categorizes the physical or logical payload (§10, §28).
type EvidenceObjectType string

const (
	ObjectTypeDocument       EvidenceObjectType = "DOCUMENT"
	ObjectTypeWorkflowEvent   EvidenceObjectType = "WORKFLOW_EVENT"
	ObjectTypeApprovalProof   EvidenceObjectType = "APPROVAL_PROOF"
	ObjectTypeAuditLogRecord  EvidenceObjectType = "AUDIT_LOG_RECORD"
	ObjectTypeTaxFilingReceipt EvidenceObjectType = "TAX_FILING_RECEIPT"
)

// ManifestStatus represents the integrity and sealing state of an evidence manifest (§10, DG-028, NP-09).
type ManifestStatus string

const (
	ManifestStatusOpen     ManifestStatus = "OPEN"
	ManifestStatusSealed   ManifestStatus = "SEALED"
	ManifestStatusVerified ManifestStatus = "VERIFIED"
	ManifestStatusTampered ManifestStatus = "TAMPERED" // Hash mismatch detected (NP-09)
)

// CustodyEventType defines governed custody and access lifecycle events (§22, DG-030).
type CustodyEventType string

const (
	CustodyEventSealed      CustodyEventType = "SEALED"
	CustodyEventAccessed    CustodyEventType = "ACCESSED"
	CustodyEventExported    CustodyEventType = "EXPORTED"
	CustodyEventTransferred CustodyEventType = "TRANSFERRED"
	CustodyEventVerified    CustodyEventType = "VERIFIED"
)

// EvidenceObject represents an immutable, hash-verifiable evidence artifact (§10, §28, DG-028, DG-029).
type EvidenceObject struct {
	ObjectID           types.UUID         `json:"object_id"`
	TenantID           types.UUID         `json:"tenant_id"`
	SubjectEntityType  string             `json:"subject_entity_type"` // e.g. "SALES_INVOICE", "JOURNAL_ENTRY"
	SubjectEntityID    types.UUID         `json:"subject_entity_id"`   // DG-029: Direct binding to subject
	ObjectType         EvidenceObjectType `json:"object_type"`
	MimeType           string             `json:"mime_type"`
	ContentHashSHA256  string             `json:"content_hash_sha256"` // SHA-256 digest of payload content
	StorageURI         string             `json:"storage_uri"`
	ByteSize           int64              `json:"byte_size"`
	SealedAt           time.Time          `json:"sealed_at"`
	CreatedBy          string             `json:"created_by"`
}

// EvidenceManifest models a sealed package binding multiple evidence objects and integrity metadata (§10, §28).
type EvidenceManifest struct {
	ManifestID           types.UUID     `json:"manifest_id"`
	TenantID             types.UUID     `json:"tenant_id"`
	ManifestCode         string         `json:"manifest_code"`
	PackageType          string         `json:"package_type"` // e.g. "AUDIT_PACKAGE", "STATUTORY_FILING", "LEGAL_DISCOVERY"
	ObjectCount          int            `json:"object_count"`
	TotalBytes           int64          `json:"total_bytes"`
	ManifestDigestSHA256 string         `json:"manifest_digest_sha256"` // Combined hash across all object hashes
	Status               ManifestStatus `json:"status"`
	SealedAt             time.Time      `json:"sealed_at"`
	SealedBy             string         `json:"sealed_by"`
}

// CustodyEvent records an auditable event in the chain of custody (§22, DG-030, NP-10).
type CustodyEvent struct {
	EventID          types.UUID       `json:"event_id"`
	TenantID         types.UUID       `json:"tenant_id"`
	ObjectID         *types.UUID      `json:"object_id,omitempty"`
	ManifestID       *types.UUID      `json:"manifest_id,omitempty"`
	EventType        CustodyEventType `json:"event_type"`
	ActorPrincipalID string           `json:"actor_principal_id"`
	ActorRole        string           `json:"actor_role"`
	Purpose          string           `json:"purpose"` // e.g. "EXTERNAL_AUDIT_DISCOVERY", "REGULATORY_REQUEST"
	OccurredAt       time.Time        `json:"occurred_at"`
}
