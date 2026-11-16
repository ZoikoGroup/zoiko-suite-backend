package events

// REF-09 unit events (ZS-SVC-I-001 s4.19: UnitCreated, ConversionRuleActivated, ConversionRuleSuperseded, UnitRetired).
//
// They carry identifiers, codes, the exact factor and its source, and dates. A quantity-to-money calculation downstream
// pins the conversion version number from a read, not from an event, but the events let a consumer know a factor changed.

import (
	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

const (
	EventQuantityDimensionCreated = "unit.dimension.created"     // (no spec name)
	EventUnitCreated              = "unit.created"               // UnitCreated
	EventConversionProposed       = "unit.conversion.proposed"   // (no spec name)
	EventConversionActivated      = "unit.conversion.activated"  // ConversionRuleActivated
	EventConversionSuperseded     = "unit.conversion.superseded" // ConversionRuleSuperseded
	EventUnitRetired              = "unit.retired"               // UnitRetired
)

// UnitEventNames is every unit wire name the code can emit.
var UnitEventNames = []string{
	EventQuantityDimensionCreated, EventUnitCreated, EventConversionProposed, EventConversionActivated,
	EventConversionSuperseded, EventUnitRetired,
}

// QuantityDimensionCreatedRecord is unit.dimension.created.
func QuantityDimensionCreatedRecord(d domain.QuantityDimension, corr string) (*outbox.Record, error) {
	return BuildRecord(RecordSpec{
		EventType: EventQuantityDimensionCreated, TenantID: d.TenantID, ActorID: d.CreatedBy, CorrelationID: corr,
		ObjectID: d.DimensionID, ObjectVersion: 1, PartitionKey: d.DimensionID,
		Payload: map[string]any{"tenant_id": d.TenantID, "dimension_id": d.DimensionID, "dimension_code": d.DimensionCode, "base_unit_code": d.BaseUnitCode},
	})
}

func unitSpec(eventType string, u domain.Unit, actor, corr, evidence string, payload map[string]any) RecordSpec {
	payload["tenant_id"] = u.TenantID
	payload["unit_id"] = u.UnitID
	payload["unit_code"] = u.UnitCode
	payload["dimension_code"] = u.DimensionCode
	payload["is_base"] = u.IsBase
	payload["status"] = u.Status
	return RecordSpec{
		EventType: eventType, TenantID: u.TenantID, ActorID: actor, CorrelationID: corr,
		ObjectID: u.UnitID, ObjectVersion: u.RecordVersion, PartitionKey: u.UnitID, EvidenceRef: evidence, Payload: payload,
	}
}

// UnitCreatedRecord is unit.created.
func UnitCreatedRecord(u domain.Unit, corr string) (*outbox.Record, error) {
	return BuildRecord(unitSpec(EventUnitCreated, u, u.CreatedBy, corr, "", map[string]any{"decimals": u.Decimals, "symbol": u.Symbol}))
}

// ConversionRecord is unit.conversion.proposed, unit.conversion.activated or unit.conversion.superseded for version c.
// prev is the version it replaces, when any.
func ConversionRecord(eventType string, u domain.Unit, c domain.ConversionVersion, prev *domain.ConversionVersion, actor, corr string) (*outbox.Record, error) {
	p := map[string]any{"version_no": c.VersionNo, "factor": c.Factor, "source_class": c.SourceClass, "source_ref": c.SourceRef,
		"valid_from": c.ValidFrom, "valid_to": c.ValidTo, "conversion_status": c.Status, "proposed_by": c.ProposedBy, "approved_by": c.ApprovedBy}
	if prev != nil {
		p["previous_version_no"], p["previous_factor"] = prev.VersionNo, prev.Factor
	}
	return BuildRecord(unitSpec(eventType, u, actor, corr, c.EvidenceRef, p))
}

// UnitRetiredRecord is unit.retired.
func UnitRetiredRecord(u domain.Unit, evidence, corr string) (*outbox.Record, error) {
	return BuildRecord(unitSpec(EventUnitRetired, u, u.RetiredBy, corr, evidence, map[string]any{"retired_on": u.RetiredOn, "reason": u.RetireReason}))
}
