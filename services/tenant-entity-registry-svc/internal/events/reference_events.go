package events

// MDM-04 reference events (ZS-SVC-U-001 s20.1: reference_release_published).
//
// A release event carries the set and release identifiers, the effective date and the compatibility statement, which is what
// a consumer needs to decide whether to adopt it. The changed codes are read from the registry at the release, not carried.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventReferenceSetCreated       = "reference.set.created"       // (no spec name)
	EventReferenceReleaseStaged    = "reference.release.staged"    // (no spec name)
	EventReferenceReleasePublished = "reference.release.published" // reference_release_published
)

// ReferenceEventNames is every reference wire name the code can emit.
var ReferenceEventNames = []string{EventReferenceSetCreated, EventReferenceReleaseStaged, EventReferenceReleasePublished}

// ReferenceSetCreatedRecord is reference.set.created.
func ReferenceSetCreatedRecord(s domain.ReferenceSet, corr string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType: EventReferenceSetCreated, TenantID: s.TenantID, ActorID: s.CreatedBy, CorrelationID: corr,
		ObjectID: s.SetID, ObjectVersion: 1, PartitionKey: s.SetID,
		Payload: map[string]any{"tenant_id": s.TenantID, "reference_set_id": s.SetID, "set_code": s.SetCode, "source_authority": s.SourceAuthority, "allow_custom": s.AllowCustom},
	})
}

// ReferenceReleaseRecord is reference.release.staged or reference.release.published.
func ReferenceReleaseRecord(eventType, tenantID, setID string, r domain.ReferenceRelease, actor, corr string) (*outbox.Record, error) {
	evidence := r.EvidenceRef
	if r.ReleaseEvidence != "" {
		evidence = r.ReleaseEvidence
	}
	return BuildRecord(RecordSpec{
		EventType: eventType, TenantID: tenantID, ActorID: actor, CorrelationID: corr,
		ObjectID: r.ReleaseID, ObjectVersion: 1, PartitionKey: setID, EvidenceRef: evidence,
		Payload: map[string]any{
			"tenant_id": tenantID, "reference_set_id": setID, "set_code": r.SetCode, "release_version": r.ReleaseVersion,
			"effective_from": r.EffectiveFrom, "compatibility": r.Compatibility, "source_authority": r.SourceAuthority,
			"entry_count": r.EntryCount, "changes": r.Changes, "release_status": r.Status, "staged_by": r.StagedBy, "released_by": r.ReleasedBy,
		},
	})
}
