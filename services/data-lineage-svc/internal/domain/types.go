// Package domain defines the authoritative domain types for
// data-lineage-svc (DATA-03, ZS-SVC-N-001 §4). This service records
// source-to-derived provenance — it never owns authoritative business
// storage or mutable business history; it records the FACT that one
// entity was derived from another via a transformation, nothing more.
package domain

import (
	"fmt"
	"time"
)

const (
	PrefixLineageEntity         = "dle_"
	PrefixLineageActivity       = "dla_"
	PrefixLineageEdge           = "dlg_"
	PrefixTransformationVersion = "dtv_"
	PrefixProvenanceManifest    = "dpm_"
	PrefixEvidenceAttachment    = "dea_"
)

// errorString is a plain sentinel error, comparable with errors.Is.
type errorString string

func (e errorString) Error() string { return string(e) }

// IdempotentReplayError is returned when a claim key was already used for
// an identical request — the caller gets back the resource that request
// already produced, not a duplicate.
type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

// IdempotencyClaim is recorded in the same transaction as the change it
// guards.
type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

// SellerScope is the idempotency owner scope for lineage commands.
const SellerScope = "seller"

// ── Entities ─────────────────────────────────────────────────────────────────

// LineageEntity is one addressable thing in the provenance graph — a
// LandingObject, a QuarantineItem, a dataset, a report, an AI extraction.
// EntityType is deliberately an open string, not a closed enum: future
// services register new entity types without a schema migration here.
// ExternalRef is that source system's own ID for the thing (e.g. a
// LandingObject's landing_id) — the (tenant, entity_type, external_ref)
// triple is how this service finds-or-creates the same entity across
// repeated calls, never minting a duplicate row for the same real thing.
type LineageEntity struct {
	EntityID    string    `json:"entity_id"`
	TenantID    string    `json:"tenant_id"`
	EntityType  string    `json:"entity_type"`
	ExternalRef string    `json:"external_ref"`
	CreatedAt   time.Time `json:"created_at"`
}

// TransformationVersion is an immutable record of one version of a named
// transformation (e.g. "data-ingestion-svc:CommitBatch:v1") — referenced
// by LineageActivity so a later audit can tell exactly which code/logic
// produced a derived fact.
type TransformationVersion struct {
	VersionID    string    `json:"version_id"`
	TenantID     string    `json:"tenant_id"`
	Name         string    `json:"name"`
	RegisteredAt time.Time `json:"registered_at"`
	RegisteredBy string    `json:"registered_by"`
}

// LineageActivity is one transformation/derivation event. TransformationVersion
// is nullable: a caller MAY record a derivation without full evidence, but
// SealManifest refuses to certify a manifest whose upstream graph has any
// activity missing one (the doc's own named acceptance test).
type LineageActivity struct {
	ActivityID            string    `json:"activity_id"`
	TenantID              string    `json:"tenant_id"`
	ActivityType          string    `json:"activity_type"`
	TransformationVersion *string   `json:"transformation_version,omitempty"`
	Agent                 string    `json:"agent"`
	OccurredAt            time.Time `json:"occurred_at"`
}

// LineageEdge is a directed edge connecting a source entity to a derived
// entity through an activity. Edges are fully append-only: SupersedeLineage
// never mutates an existing edge's own source/activity/derived fields — it
// inserts a NEW edge with SupersedesEdgeID pointing at the old one. An
// edge's "current" status is therefore computed (is anything superseding
// it?), never stored as a mutable flag.
type LineageEdge struct {
	EdgeID           string    `json:"edge_id"`
	TenantID         string    `json:"tenant_id"`
	SourceEntityID   string    `json:"source_entity_id"`
	ActivityID       string    `json:"activity_id"`
	DerivedEntityID  string    `json:"derived_entity_id"`
	SupersedesEdgeID *string   `json:"supersedes_edge_id,omitempty"`
	Reason           *string   `json:"reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	CreatedBy        string    `json:"created_by"`
}

// ProvenanceManifest is a sealed, immutable snapshot of the full upstream
// lineage graph for one entity, computed and frozen server-side at
// SealManifest time — proof without re-walking the live graph later.
type ProvenanceManifest struct {
	ManifestID     string               `json:"manifest_id"`
	TenantID       string               `json:"tenant_id"`
	EntityID       string               `json:"entity_id"`
	Manifest       LineageGraphSnapshot `json:"manifest"`
	ManifestSHA256 string               `json:"manifest_sha256"`
	SealedAt       time.Time            `json:"sealed_at"`
	SealedBy       string               `json:"sealed_by"`
}

// LineageGraphSnapshot is the manifest's own content shape — every entity,
// activity and edge encountered while walking upstream from the sealed
// entity.
type LineageGraphSnapshot struct {
	RootEntityID string            `json:"root_entity_id"`
	Entities     []LineageEntity   `json:"entities"`
	Activities   []LineageActivity `json:"activities"`
	Edges        []LineageEdge     `json:"edges"`
}

// EvidenceAttachment is an external evidence reference (a hash, a URL, a
// document ID) attached to a LineageEntity without altering the entity
// itself.
type EvidenceAttachment struct {
	AttachmentID string    `json:"attachment_id"`
	TenantID     string    `json:"tenant_id"`
	EntityID     string    `json:"entity_id"`
	EvidenceRef  string    `json:"evidence_ref"`
	AttachedAt   time.Time `json:"attached_at"`
	AttachedBy   string    `json:"attached_by"`
}

// ── Requests ─────────────────────────────────────────────────────────────────

// EntityRef identifies a LineageEntity by its natural key — how callers
// name an entity without needing to know its internal entity_id.
type EntityRef struct {
	EntityType  string `json:"entity_type"`
	ExternalRef string `json:"external_ref"`
}

func (r EntityRef) Validate(field string) error {
	if r.EntityType == "" {
		return fmt.Errorf("%s.entity_type is required", field)
	}
	if r.ExternalRef == "" {
		return fmt.Errorf("%s.external_ref is required", field)
	}
	return nil
}

type RecordDerivationRequest struct {
	SourceEntity          EntityRef `json:"source_entity"`
	ActivityType          string    `json:"activity_type"`
	TransformationVersion *string   `json:"transformation_version,omitempty"`
	Agent                 string    `json:"agent"`
	DerivedEntity         EntityRef `json:"derived_entity"`
}

func (r RecordDerivationRequest) Validate() error {
	if err := r.SourceEntity.Validate("source_entity"); err != nil {
		return err
	}
	if err := r.DerivedEntity.Validate("derived_entity"); err != nil {
		return err
	}
	if r.ActivityType == "" {
		return fmt.Errorf("activity_type is required")
	}
	if r.Agent == "" {
		return fmt.Errorf("agent is required")
	}
	if r.TransformationVersion != nil && *r.TransformationVersion == "" {
		return fmt.Errorf("transformation_version, if given, must not be empty")
	}
	return nil
}

type SupersedeLineageRequest struct {
	EdgeID string `json:"edge_id"`
	Reason string `json:"reason"`
}

func (r SupersedeLineageRequest) Validate() error {
	if r.EdgeID == "" {
		return fmt.Errorf("edge_id is required")
	}
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

type AttachEvidenceRequest struct {
	Entity      EntityRef `json:"entity"`
	EvidenceRef string    `json:"evidence_ref"`
}

func (r AttachEvidenceRequest) Validate() error {
	if err := r.Entity.Validate("entity"); err != nil {
		return err
	}
	if r.EvidenceRef == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	return nil
}

var (
	ErrLineageEntityNotFound        = errorString("lineage entity not found")
	ErrLineageEdgeNotFound          = errorString("lineage edge not found")
	ErrLineageEdgeAlreadySuperseded = errorString("lineage edge is already superseded")
	ErrProvenanceManifestNotFound   = errorString("provenance manifest not found")
	ErrMissingTransformationVersion = errorString("upstream lineage has an activity with no transformation_version recorded; cannot seal a certified manifest")
	ErrIdempotencyKeyReused         = errorString("idempotency key was already used for a different request")
)
