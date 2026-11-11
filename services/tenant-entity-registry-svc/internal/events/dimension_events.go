package events

// REF-08 dimension events (ZS-SVC-I-001 s4.18: DimensionValueCreated, DimensionHierarchyChanged,
// DimensionValueDeactivated, SourceDimensionMapped).
//
// They carry identifiers, the type and value codes, status and effective dates. A label is a name, not personal data, but
// events carry codes and ids so a consumer reads the label from the registry at the version it needs.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventDimensionTypeCreated       = "dimension.type.created"       // (no spec name)
	EventDimensionValueCreated      = "dimension.value.created"      // DimensionValueCreated
	EventDimensionValueActivated    = "dimension.value.activated"    // (no spec name)
	EventDimensionValueDeactivated  = "dimension.value.deactivated"  // DimensionValueDeactivated
	EventDimensionHierarchyProposed = "dimension.hierarchy.proposed" // (no spec name)
	EventDimensionHierarchyChanged  = "dimension.hierarchy.changed"  // DimensionHierarchyChanged
	EventDimensionSourceMapped      = "dimension.source.mapped"      // SourceDimensionMapped
)

// DimensionEventNames is every dimension wire name the code can emit.
var DimensionEventNames = []string{
	EventDimensionTypeCreated, EventDimensionValueCreated, EventDimensionValueActivated, EventDimensionValueDeactivated,
	EventDimensionHierarchyProposed, EventDimensionHierarchyChanged, EventDimensionSourceMapped,
}

// DimensionTypeCreatedRecord is dimension.type.created.
func DimensionTypeCreatedRecord(t domain.DimensionType, corr string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType: EventDimensionTypeCreated, TenantID: t.TenantID, ActorID: t.CreatedBy, CorrelationID: corr,
		ObjectID: t.TypeID, ObjectVersion: 1, PartitionKey: t.TypeID,
		Payload: map[string]any{"tenant_id": t.TenantID, "type_id": t.TypeID, "type_code": t.TypeCode, "hierarchical": t.Hierarchical,
			"controlled": t.Controlled, "source_domain": t.SourceDomain},
	})
}

func dimensionSpec(eventType string, v domain.DimensionValue, actor, corr, evidence, effective string, payload map[string]any) RecordSpec {
	payload["tenant_id"] = v.TenantID
	payload["value_id"] = v.ValueID
	payload["type_id"] = v.TypeID
	payload["type_code"] = v.TypeCode
	payload["code"] = v.Code
	payload["status"] = v.Status
	payload["effective_date"] = effective
	if v.LegalEntityID != "" {
		payload["legal_entity_id"] = v.LegalEntityID
	}
	return RecordSpec{
		EventType: eventType, TenantID: v.TenantID, LegalEntityID: v.LegalEntityID, ActorID: actor, CorrelationID: corr,
		ObjectID: v.ValueID, ObjectVersion: v.RecordVersion, PartitionKey: v.TypeID, EvidenceRef: evidence, Payload: payload,
	}
}

// DimensionValueCreatedRecord is dimension.value.created.
func DimensionValueCreatedRecord(v domain.DimensionValue, h domain.HierarchyVersion, corr string) (*outbox.Record, error) {
	return BuildRecord(dimensionSpec(EventDimensionValueCreated, v, v.CreatedBy, corr, h.EvidenceRef, h.ValidFrom,
		map[string]any{"parent_value_id": h.ParentValueID, "label": h.Label}))
}

// DimensionStatusRecord is dimension.value.activated or dimension.value.deactivated. A deactivation can carry a controlled
// posting exception, stated in the event so a posting service does not have to guess.
func DimensionStatusRecord(v domain.DimensionValue, t domain.DimensionTransition, corr string) (*outbox.Record, error) {
	eventType := EventDimensionValueActivated
	if t.To == domain.DimInactive {
		eventType = EventDimensionValueDeactivated
	}
	return BuildRecord(dimensionSpec(eventType, v, t.Actor, corr, t.EvidenceRef, t.EffectiveDate, map[string]any{
		"from_status": t.From, "to_status": t.To, "command": t.Command, "posting_exception_until": t.ExceptionUntil,
	}))
}

// DimensionHierarchyRecord is dimension.hierarchy.proposed (a change awaiting independent approval) or
// dimension.hierarchy.changed (a change that takes effect from valid_from). prev is the version it replaces, when any.
func DimensionHierarchyRecord(eventType string, v domain.DimensionValue, next domain.HierarchyVersion, prev *domain.HierarchyVersion, actor, corr string) (*outbox.Record, error) {
	p := map[string]any{"version_no": next.VersionNo, "parent_value_id": next.ParentValueID, "label": next.Label, "reason": next.Reason,
		"proposed_by": next.ProposedBy, "approved_by": next.ApprovedBy, "hierarchy_status": next.Status}
	if prev != nil {
		p["previous_version_no"], p["previous_parent_value_id"], p["previous_label"] = prev.VersionNo, prev.ParentValueID, prev.Label
	}
	return BuildRecord(dimensionSpec(eventType, v, actor, corr, next.EvidenceRef, next.ValidFrom, p))
}

// DimensionSourceMappedRecord is dimension.source.mapped.
func DimensionSourceMappedRecord(v domain.DimensionValue, m domain.SourceMapping, prev *domain.SourceMapping, actor, corr string) (*outbox.Record, error) {
	p := map[string]any{"mapping_id": m.MappingID, "source_domain": m.SourceDomain, "source_object_id": m.SourceObjectID, "mapping_version": m.VersionNo}
	if prev != nil {
		p["previous_value_id"] = prev.ValueID
	}
	return BuildRecord(dimensionSpec(EventDimensionSourceMapped, v, actor, corr, m.EvidenceRef, m.ValidFrom, p))
}
