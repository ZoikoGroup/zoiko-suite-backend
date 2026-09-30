// Package domain defines the authoritative domain types for
// analytical-data-platform-svc (DATA-04, ZS-SVC-N-001 §4). This service
// creates governed analytical snapshots, curated datasets and query-ready
// projections. It never owns systems of record or authoritative
// posting/tax/payment state — it is a downstream projection layer.
package domain

import (
	"fmt"
	"time"
)

const (
	PrefixDataset        = "dad_"
	PrefixDatasetVersion = "ddv_"
	PrefixSnapshot       = "dsn_"
	PrefixPartition      = "dpt_"
	PrefixCertification  = "ddc_"
)

type errorString string

func (e errorString) Error() string { return string(e) }

type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

const SellerScope = "seller"

// ── AnalyticalDataset / DatasetVersion ──────────────────────────────────────

type AnalyticalDataset struct {
	DatasetID string    `json:"dataset_id"`
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

type DatasetVersionStatus string

const (
	VersionDraft       DatasetVersionStatus = "Draft"
	VersionBuilding    DatasetVersionStatus = "Building"
	VersionValidated   DatasetVersionStatus = "Validated"
	VersionPublished   DatasetVersionStatus = "Published"
	VersionDeprecated  DatasetVersionStatus = "Deprecated"
	VersionQuarantined DatasetVersionStatus = "Quarantined"
)

// DatasetVersion is one versioned build of a dataset. Published versions
// are immutable — a "rebuild" of published content is always a brand new
// version, never an edit of the one already live (the doc's own named
// acceptance test: "published dataset cannot be silently rebuilt in
// place").
type DatasetVersion struct {
	VersionID             string                 `json:"version_id"`
	TenantID              string                 `json:"tenant_id"`
	DatasetID             string                 `json:"dataset_id"`
	VersionNumber         int                    `json:"version_number"`
	SchemaDefinition      map[string]interface{} `json:"schema_definition"`
	TransformationVersion string                 `json:"transformation_version"`
	SourceCheckpointRef   string                 `json:"source_checkpoint_ref"`
	ResidencyRegion       string                 `json:"residency_region"`
	Classification        string                 `json:"classification"`
	MaxStalenessSeconds   int64                  `json:"max_staleness_seconds"`
	Status                DatasetVersionStatus   `json:"status"`
	CreatedAt             time.Time              `json:"created_at"`
	CreatedBy             string                 `json:"created_by"`
	PublishedAt           *time.Time             `json:"published_at,omitempty"`
	PublishedBy           *string                `json:"published_by,omitempty"`
}

type CreateDatasetVersionRequest struct {
	DatasetName           string                 `json:"dataset_name"`
	SchemaDefinition      map[string]interface{} `json:"schema_definition"`
	TransformationVersion string                 `json:"transformation_version"`
	ResidencyRegion       string                 `json:"residency_region"`
	Classification        string                 `json:"classification"`
	MaxStalenessSeconds   int64                  `json:"max_staleness_seconds"`
}

func (r CreateDatasetVersionRequest) Validate() error {
	if r.DatasetName == "" {
		return fmt.Errorf("dataset_name is required")
	}
	if r.TransformationVersion == "" {
		return fmt.Errorf("transformation_version is required")
	}
	if r.ResidencyRegion == "" {
		return fmt.Errorf("residency_region is required")
	}
	if r.Classification == "" {
		return fmt.Errorf("classification is required")
	}
	if r.MaxStalenessSeconds <= 0 {
		return fmt.Errorf("max_staleness_seconds must be positive")
	}
	return nil
}

// ── Snapshot ─────────────────────────────────────────────────────────────────

// Snapshot is one materialization of a DatasetVersion's data — the
// caller-supplied evidence of what was actually built (row count, content
// hash, source watermark). Immutable once written; BuildSnapshot may be
// called more than once while the owning version is still unpublished
// (each rebuild-before-publish gets its own Snapshot row), but never
// after Publish.
type Snapshot struct {
	SnapshotID  string    `json:"snapshot_id"`
	TenantID    string    `json:"tenant_id"`
	VersionID   string    `json:"version_id"`
	Watermark   string    `json:"watermark"`
	RowCount    int64     `json:"row_count"`
	ContentHash string    `json:"content_hash"`
	BuiltAt     time.Time `json:"built_at"`
	BuiltBy     string    `json:"built_by"`
}

type BuildSnapshotRequest struct {
	VersionID   string `json:"version_id"`
	Watermark   string `json:"watermark"`
	RowCount    int64  `json:"row_count"`
	ContentHash string `json:"content_hash"`
}

func (r BuildSnapshotRequest) Validate() error {
	if r.VersionID == "" {
		return fmt.Errorf("version_id is required")
	}
	if r.Watermark == "" {
		return fmt.Errorf("watermark is required")
	}
	if r.RowCount <= 0 {
		return fmt.Errorf("row_count must be positive")
	}
	if r.ContentHash == "" {
		return fmt.Errorf("content_hash is required")
	}
	return nil
}

// ── Partition ────────────────────────────────────────────────────────────────

// Partition is one keyed slice of a Snapshot (e.g. a date or region
// range). RebuildPartition always creates a NEW partition row for a given
// key — reproducible rebuild, never an in-place edit — and is only
// permitted while the owning version is still unpublished.
type Partition struct {
	PartitionID  string    `json:"partition_id"`
	TenantID     string    `json:"tenant_id"`
	VersionID    string    `json:"version_id"`
	SnapshotID   string    `json:"snapshot_id"`
	PartitionKey string    `json:"partition_key"`
	RowCount     int64     `json:"row_count"`
	ContentHash  string    `json:"content_hash"`
	BuiltAt      time.Time `json:"built_at"`
	BuiltBy      string    `json:"built_by"`
}

type RebuildPartitionRequest struct {
	VersionID    string `json:"version_id"`
	SnapshotID   string `json:"snapshot_id"`
	PartitionKey string `json:"partition_key"`
	RowCount     int64  `json:"row_count"`
	ContentHash  string `json:"content_hash"`
}

func (r RebuildPartitionRequest) Validate() error {
	if r.VersionID == "" {
		return fmt.Errorf("version_id is required")
	}
	if r.SnapshotID == "" {
		return fmt.Errorf("snapshot_id is required")
	}
	if r.PartitionKey == "" {
		return fmt.Errorf("partition_key is required")
	}
	if r.RowCount < 0 {
		return fmt.Errorf("row_count must not be negative")
	}
	if r.ContentHash == "" {
		return fmt.Errorf("content_hash is required")
	}
	return nil
}

// ── DataProductCertification ────────────────────────────────────────────────

// DataProductCertification is the sealed, immutable evidence produced at
// PublishDatasetVersion time — same "sealed evidence, sha256, immutable
// at the database" doctrine as every other sealed-evidence entity in this
// session's work.
type DataProductCertification struct {
	CertificationID string               `json:"certification_id"`
	TenantID        string               `json:"tenant_id"`
	VersionID       string               `json:"version_id"`
	Summary         CertificationSummary `json:"summary"`
	SummarySHA256   string               `json:"summary_sha256"`
	CertifiedAt     time.Time            `json:"certified_at"`
	CertifiedBy     string               `json:"certified_by"`
}

type CertificationSummary struct {
	VersionID             string `json:"version_id"`
	DatasetID             string `json:"dataset_id"`
	VersionNumber         int    `json:"version_number"`
	SnapshotID            string `json:"snapshot_id"`
	Watermark             string `json:"watermark"`
	RowCount              int64  `json:"row_count"`
	ContentHash           string `json:"content_hash"`
	TransformationVersion string `json:"transformation_version"`
}

// Freshness is a computed (never stored) assurance signal: how stale the
// dataset version's latest snapshot is as of a given moment, against its
// own declared bound.
type Freshness struct {
	VersionID           string `json:"version_id"`
	LatestSnapshotID    string `json:"latest_snapshot_id"`
	StalenessSeconds    int64  `json:"staleness_seconds"`
	MaxStalenessSeconds int64  `json:"max_staleness_seconds"`
	IsStale             bool   `json:"is_stale"`
}

var (
	ErrDatasetNotFound        = errorString("analytical dataset not found")
	ErrDatasetVersionNotFound = errorString("dataset version not found")
	ErrSnapshotNotFound       = errorString("snapshot not found")
	ErrNoSnapshotYet          = errorString("dataset version has no snapshot yet")
	ErrCertificationNotFound  = errorString("data product certification not found")
	ErrVersionNotValidated    = errorString("dataset version must be Validated (has at least one snapshot) to publish")
	ErrVersionPublished       = errorString("dataset version is published and immutable; rebuild as a new version instead")
	ErrVersionNotDeprecatable = errorString("dataset version must be Published or Validated to deprecate")
	ErrIdempotencyKeyReused   = errorString("idempotency key was already used for a different request")
	ErrImmutableViolation     = errorString("this record cannot be mutated in that way")
)
