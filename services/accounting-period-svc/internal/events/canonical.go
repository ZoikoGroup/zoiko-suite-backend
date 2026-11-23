package events

import (
	"fmt"

	eventingenvelope "zoiko.io/eventing/envelope"
)

// Canonical event types (ZS-EVENT-001 section 5.1). The service's pre-standard
// PascalCase names stay on the wire as the legacy event_type, so consumers that
// filter on them keep working while they move to the canonical type (expand /
// contract; see eventing README).
//
// Written as full literals on purpose: the catalogue audit in
// services/eventing/catalogue finds producers by scanning for them.
const (
	TypePeriodOpened          = "com.zoikosuite.accounting.period.opened"
	TypePeriodSoftClosed      = "com.zoikosuite.accounting.period.soft-closed"
	TypePeriodHardClosed      = "com.zoikosuite.accounting.period.hard-closed"
	TypePeriodReopened        = "com.zoikosuite.accounting.period.reopened"
	TypePeriodReclosed        = "com.zoikosuite.accounting.period.reclosed"
	TypePeriodCommandRejected = "com.zoikosuite.accounting.period.command-rejected"
)

var canonicalType = map[string]string{
	EventPeriodOpened:          TypePeriodOpened,
	EventPeriodSoftClosed:      TypePeriodSoftClosed,
	EventPeriodHardClosed:      TypePeriodHardClosed,
	EventPeriodReopened:        TypePeriodReopened,
	EventPeriodReclosed:        TypePeriodReclosed,
	EventPeriodCommandRejected: TypePeriodCommandRejected,
}

// CanonicalType returns the ZS-EVENT-001 type for one of this service's event names.
func CanonicalType(legacy string) (string, bool) {
	t, ok := canonicalType[legacy]
	return t, ok
}

// SchemaVersion is the payload contract version of every event this service emits.
const SchemaVersion = "1.0.0"

// BuildEnvelope renders one event as a ZS-EVENT-001 envelope. Like Build it is
// called at STATE-CHANGE time inside the writing transaction, and the result
// goes to the eventing outbox in that same transaction. region is the
// authoritative residency region (config EVENT_RESIDENCY_REGION).
//
// The data is exactly what Build puts in the legacy payload, so a consumer
// reading either `data` or the legacy `payload` member sees the same facts.
func BuildEnvelope(e Event, region string) (*eventingenvelope.Envelope, error) {
	canonical, known := CanonicalType(e.Type)
	if !known {
		return nil, fmt.Errorf("events: unknown event type %q", e.Type)
	}
	if e.TenantID == "" || e.ObjectID == "" {
		return nil, fmt.Errorf("events: %s requires tenant_id and object_id", e.Type)
	}
	data, err := payloadOf(e)
	if err != nil {
		return nil, err
	}
	legalEntity, _ := e.Data["legal_entity_id"].(string)
	version := e.ObjectVersion
	effective := e.EffectiveAt.UTC()
	env, err := eventingenvelope.New(eventingenvelope.Spec{
		Type:             canonical,
		LegacyType:       e.Type,
		Service:          SourceService,
		SchemaVersion:    SchemaVersion,
		OccurredAt:       e.RecordedAt.UTC(),
		EffectiveAt:      &effective,
		TenantID:         e.TenantID,
		LegalEntityID:    legalEntity,
		AggregateType:    "accounting-period",
		AggregateID:      e.ObjectID,
		AggregateVersion: &version,
		CorrelationID:    e.CorrelationID,
		CausationID:      e.CausationID,
		ActorID:          e.Actor,
		ResidencyRegion:  region,
		Classification:   eventingenvelope.Confidential,
		Data:             data,
	})
	if err != nil {
		return nil, fmt.Errorf("events: build envelope for %s: %w", e.Type, err)
	}
	return env, nil
}
