package archive

import (
	"time"

	"zoiko.io/contract/types"
)

// ArchiveStatus represents package integrity and lifecycle state (§21, DG-047..DG-050).
type ArchiveStatus string

const (
	ArchiveStatusSealed   ArchiveStatus = "SEALED"
	ArchiveStatusRestored ArchiveStatus = "RESTORED"
	ArchiveStatusTampered ArchiveStatus = "TAMPERED" // NP-26
)

// ArchivePackage models an immutable, sealed archive unit plus manifest and policy bindings (§21, §28, DG-047).
type ArchivePackage struct {
	ArchiveID            types.UUID    `json:"archive_id"`
	TenantID             types.UUID    `json:"tenant_id"`
	ArchiveCode          string        `json:"archive_code"`
	RecordCount          int           `json:"record_count"`
	TotalBytes           int64         `json:"total_bytes"`
	PackageDigestSHA256  string        `json:"package_digest_sha256"` // Digest computed over archive contents (NP-26)
	RetentionScheduleRef string        `json:"retention_schedule_ref"`
	HoldBindings         string        `json:"hold_bindings,omitempty"`
	Status               ArchiveStatus `json:"status"`
	SealedAt             time.Time     `json:"sealed_at"`
	SealedBy             string        `json:"sealed_by"`
}

// RestoreRequest models a governed request to rehydrate archived data (§21, DG-048).
type RestoreRequest struct {
	RequestID                 types.UUID `json:"request_id"`
	TenantID                  types.UUID `json:"tenant_id"`
	ArchiveID                 types.UUID `json:"archive_id"`
	RequestedBy               string     `json:"requested_by"`
	AuthorizedPurpose         string     `json:"authorized_purpose"` // Must have approved purpose (NP-25)
	TargetRestoreEnvironment  string     `json:"target_restore_environment"`
	ReapplyTombstonesRequired bool       `json:"reapply_tombstones_required"` // DG-049, NP-24
	RequestedAt               time.Time  `json:"requested_at"`
}
