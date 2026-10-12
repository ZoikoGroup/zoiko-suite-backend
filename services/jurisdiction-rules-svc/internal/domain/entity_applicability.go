package domain

import "time"

// EntityJurisdictionApplicability is one jurisdiction that applies to a legal
// entity over a period — this service's projection of tenant-entity-registry-svc's
// entity.created (primary jurisdiction) and entity.jurisdiction.changed
// (assignments) events. Doc 03 §8.2 lists "entity-jurisdiction applicability
// mappings" under what this service owns; the registry stays the system of
// record for the assignments themselves.
type EntityJurisdictionApplicability struct {
	ApplicabilityID     string     `json:"applicability_id"`
	TenantID            string     `json:"tenant_id"`
	LegalEntityID       string     `json:"legal_entity_id"`
	JurisdictionID      string     `json:"jurisdiction_id"`
	ApplicabilityType   string     `json:"applicability_type"`
	EffectiveFrom       time.Time  `json:"effective_from"`
	EffectiveTo         *time.Time `json:"effective_to"`
	SourceObjectVersion int64      `json:"source_object_version"`
	SourceEventType     string     `json:"source_event_type"`
	SourceEventID       string     `json:"source_event_id"`
	EvidenceRef         *string    `json:"evidence_ref,omitempty"`
	RecordedAt          time.Time  `json:"recorded_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// ApplicabilityEntityPrimary is the type of the row entity.created yields
// for the entity's primary jurisdiction. Distinct from the producer's own
// PRIMARY assignment type, which arrives through entity.jurisdiction.changed.
const ApplicabilityEntityPrimary = "ENTITY_PRIMARY"

// ErrInvalidEvent is a consumed event that can never be applied (malformed
// id, inverted period, missing tenant). It is recorded as consumed and
// skipped, so one bad message cannot stall the partition behind it.
var ErrInvalidEvent = errorString("consumed event is malformed and cannot be applied")
