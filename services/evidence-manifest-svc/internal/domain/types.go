// Package domain defines canonical types for evidence-manifest-svc.
//
// Purpose (docs/architecture/03-microservices.md §14.4): builds structured
// evidence sets for audit, regulator, legal-discovery, and compliance-review
// scenarios — assembling records that are already, individually, evidence
// (01-backend.md, Diagram 7 "Every governance decision is itself evidence")
// into one coherent, checksummed, retrievable package.
//
// IMPORTANT — a real constraint that shapes this service's v1 scope, found
// while building it, documented rather than hidden:
//   - governance-decision-log-svc exposes a real list/filter query
//     (GET /v1/decisions?entity=...&from=...&to=...) — this is the only
//     source service with genuine discovery. Manifests can auto-populate
//     from it.
//   - authorization-svc exposes ONLY GET /v1/access-decisions/{id} — no list,
//     no filter. A manifest can only include an access decision if the
//     caller already knows its ID.
//   - workflow-svc exposes ONLY GET /v1/workflows/{id}, and even that returns
//     the instance + stages, NOT the transition history itself (there is no
//     endpoint exposing workflow_transitions at all).
//   - audit-event-store-svc exposes NO query API whatsoever (health probes
//     only) — it is a pure Kafka consumer. It is NOT included in v1 manifests;
//     this is a deferred gap, not an oversight (see ManifestSection below).
package domain

import (
	"errors"
	"time"
)

type ScenarioType string

const (
	ScenarioAudit            ScenarioType = "AUDIT"
	ScenarioRegulator        ScenarioType = "REGULATOR"
	ScenarioLegalDiscovery   ScenarioType = "LEGAL_DISCOVERY"
	ScenarioComplianceReview ScenarioType = "COMPLIANCE_REVIEW"
)

func (s ScenarioType) Valid() bool {
	switch s {
	case ScenarioAudit, ScenarioRegulator, ScenarioLegalDiscovery, ScenarioComplianceReview:
		return true
	}
	return false
}

type ManifestStatus string

const (
	StatusPending   ManifestStatus = "PENDING"
	StatusGenerated ManifestStatus = "GENERATED"
	StatusFailed    ManifestStatus = "FAILED"
)

// EvidenceManifest is the top-level, immutable-once-generated record. Per the
// doctrine shared with every other evidential store in this platform
// (01-backend.md, "Required Properties of Every Evidential Record"): once
// status is GENERATED, this row and its included_records are never mutated —
// a re-run produces a NEW manifest, never edits an old one.
type EvidenceManifest struct {
	ManifestID     string         `json:"manifest_id"`
	TenantID       string         `json:"tenant_id"`
	LegalEntityID  string         `json:"legal_entity_id"`
	ScenarioType   ScenarioType   `json:"scenario_type"`
	RequestedBy    string         `json:"requested_by"`
	Status         ManifestStatus `json:"status"`
	ChecksumSHA256 *string        `json:"checksum_sha256,omitempty"`
	FailureReason  *string        `json:"failure_reason,omitempty"`
	RequestedAt    time.Time      `json:"requested_at"`
	GeneratedAt    *time.Time     `json:"generated_at,omitempty"`
}

// SourceType identifies which source service a ManifestRecord came from.
type SourceType string

const (
	SourceGovernanceDecision SourceType = "GOVERNANCE_DECISION"
	SourceAccessDecision     SourceType = "ACCESS_DECISION"
	SourceWorkflowInstance   SourceType = "WORKFLOW_INSTANCE"
)

// ManifestRecord is one append-only reference to a source-of-truth record
// included in a manifest. It stores a pointer + a snapshot (RecordSnapshot,
// JSON) of what that record looked like AT MANIFEST GENERATION TIME — the
// manifest must remain reconstructable even if the source record's service
// is unavailable later, or evolves. This is what makes a manifest genuinely
// "retrievable" evidence, not just a list of foreign keys.
type ManifestRecord struct {
	ManifestRecordID string     `json:"manifest_record_id"`
	ManifestID       string     `json:"manifest_id"`
	SourceType       SourceType `json:"source_type"`
	SourceRecordID   string     `json:"source_record_id"`
	RecordSnapshot   []byte     `json:"record_snapshot"` // raw JSON from the source service
	FetchedAt        time.Time  `json:"fetched_at"`
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// GenerateManifestRequest is deliberately explicit about the real discovery
// constraint above: governance decisions can be auto-discovered by entity +
// date range; access decisions and workflow instances cannot be discovered at
// all from their own services and MUST be supplied by reference ID.
type GenerateManifestRequest struct {
	TenantID      string       `json:"tenant_id"`
	LegalEntityID string       `json:"legal_entity_id"`
	ScenarioType  ScenarioType `json:"scenario_type"`
	RequestedBy   string       `json:"requested_by,omitempty"`

	// GovernanceDecisionsFrom/To scope an auto-discovery query against
	// governance-decision-log-svc's real list endpoint. Both optional; if
	// both empty, no governance decisions are auto-discovered (the caller
	// must supply GovernanceDecisionIDs explicitly instead).
	GovernanceDecisionsFrom *time.Time `json:"governance_decisions_from,omitempty"`
	GovernanceDecisionsTo   *time.Time `json:"governance_decisions_to,omitempty"`

	// Explicit reference IDs — required for these two sources because
	// authorization-svc and workflow-svc expose no discovery query.
	GovernanceDecisionIDs []string `json:"governance_decision_ids,omitempty"`
	AccessDecisionIDs     []string `json:"access_decision_ids,omitempty"`
	WorkflowInstanceIDs   []string `json:"workflow_instance_ids,omitempty"`
}

// ---------------------------------------------------------------------------
// Sentinel errors
// ---------------------------------------------------------------------------

var (
	ErrManifestNotFound         = errors.New("evidence manifest not found")
	ErrInvalidScenarioType      = errors.New("invalid scenario_type")
	ErrNoRecordsRequested       = errors.New("no evidence records requested — supply governance_decisions_from/to, or explicit record IDs")
	ErrStoreUnavailable         = errors.New("evidence manifest store unavailable")
	ErrSourceServiceUnavailable = errors.New("a required source service is unavailable — manifest generation fails closed")
)
