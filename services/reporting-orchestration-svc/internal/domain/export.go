// AUD-10's export/redact/deliver half. The archive/verify half lives in
// audit-event-store-svc, operating directly on its own hash chain — this
// package never re-derives or re-verifies that chain, it only cites the
// real archive record audit-event-store-svc already produced (see
// internal/archivestore/client.go). BuildExportPackage therefore cannot
// fabricate export content the way OrchestratReportRun used to fabricate
// report content (see types.go's own RunStatusNotImplemented doc comment
// for that precedent) — it fails the export outright if the referenced
// archive cannot be fetched, rather than inventing a manifest.
package domain

import (
	"errors"
	"time"
)

type ExportStatus string

const (
	ExportRequested ExportStatus = "REQUESTED"
	ExportApproved  ExportStatus = "APPROVED"
	ExportBuilding  ExportStatus = "BUILDING"
	ExportSealed    ExportStatus = "SEALED"
	ExportDelivered ExportStatus = "DELIVERED"
	ExportRevoked   ExportStatus = "REVOKED"
	ExportFailed    ExportStatus = "FAILED"
)

// AuditExportRequest tracks one export of an audit archive end to end. It
// references ArchiveID by plain string ID rather than a foreign key —
// audit-event-store-svc is a different service/database; the reference is
// validated live at BuildExportPackage time via a real HTTP call, not at
// insert time.
type AuditExportRequest struct {
	ExportID               string       `json:"export_id"`
	TenantID               string       `json:"tenant_id"`
	LegalEntityID          string       `json:"legal_entity_id"`
	ArchiveID              string       `json:"archive_id"`
	Purpose                string       `json:"purpose"`
	Status                 ExportStatus `json:"status"`
	RequestedByPrincipalID string       `json:"requested_by_principal_id"`
	ApprovedByPrincipalID  *string      `json:"approved_by_principal_id,omitempty"`
	DeliveredToPrincipalID *string      `json:"delivered_to_principal_id,omitempty"`
	FailureReason          string       `json:"failure_reason,omitempty"`
	CreatedAt              time.Time    `json:"created_at"`
	ApprovedAt             *time.Time   `json:"approved_at,omitempty"`
	SealedAt               *time.Time   `json:"sealed_at,omitempty"`
	DeliveredAt            *time.Time   `json:"delivered_at,omitempty"`
	RevokedAt              *time.Time   `json:"revoked_at,omitempty"`
}

// ExportManifest is one artifact entry in a built export package —
// append-only, and its Sha256 is always a digest fetched live from the
// upstream archive, never computed from data this service invented.
type ExportManifest struct {
	ManifestID   string    `json:"manifest_id"`
	ExportID     string    `json:"export_id"`
	ArtifactName string    `json:"artifact_name"`
	Sha256       string    `json:"sha256"`
	RecordedAt   time.Time `json:"recorded_at"`
}

type RedactionDecision struct {
	DecisionID           string    `json:"decision_id"`
	ExportID             string    `json:"export_id"`
	FieldOrScope         string    `json:"field_or_scope"`
	Reason               string    `json:"reason"`
	DecidedByPrincipalID string    `json:"decided_by_principal_id"`
	DecidedAt            time.Time `json:"decided_at"`
}

type DeliveryReceipt struct {
	ReceiptID              string    `json:"receipt_id"`
	ExportID               string    `json:"export_id"`
	DeliveredToPrincipalID string    `json:"delivered_to_principal_id"`
	DeliveryChannel        string    `json:"delivery_channel"`
	DeliveredAt            time.Time `json:"delivered_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

type CreateExportRequestParams struct {
	TenantID, LegalEntityID, ArchiveID, Purpose, RequestedByPrincipalID string
}

type ApproveExportParams struct {
	ExportID, TenantID, ApprovedByPrincipalID string
}

type RecordRedactionParams struct {
	ExportID, TenantID, FieldOrScope, Reason, DecidedByPrincipalID string
}

type SealExportParams struct {
	ExportID, TenantID string
}

type DeliverExportParams struct {
	ExportID, TenantID, DeliveredToPrincipalID, DeliveryChannel string
}

type RevokeExportParams struct {
	ExportID, TenantID string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrExportNotFound       = errors.New("audit export request not found")
	ErrExportInvalidState   = errors.New("audit export is not in a state that permits this action")
	ErrExportSelfApproval   = errors.New("principal may not approve their own export request")
	ErrArchiveFetchFailed   = errors.New("could not fetch the referenced archive from audit-event-store-svc: export not built")
	ErrManifestRequired     = errors.New("export has no manifest: cannot seal an empty package")
	ErrLegalHoldActive      = errors.New("delivery blocked: a legal hold applies to this export's records")
	ErrRetentionServiceDown = errors.New("retention-registry-svc unavailable: delivery blocked (fail closed)")
)
