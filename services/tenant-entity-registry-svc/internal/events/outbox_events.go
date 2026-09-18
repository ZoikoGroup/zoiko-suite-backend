package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"zoiko.io/tenant-entity-registry-svc/internal/outbox"
)

// This file renders the ORG-02/ORG-03 events into outbox records.
//
// Why a separate path from Publisher.emit: emit writes to Kafka directly and
// swallows the error. The events below must instead be handed to the caller as
// a value, so the caller can write them into event_outbox inside the same
// transaction as the fact they attest (ORG §9.2). The ENVELOPE is deliberately
// the same struct, so an event delivered via the outbox is byte-identical on
// the wire to one delivered directly — a consumer cannot tell which path an
// event took, and should not be able to.
//
// EVENT NAMES. The spec names events in PascalCase (TenantActivated,
// LegalEntityProfileAmended). Those are spec names, not wire names: this
// estate's wire convention is lower.dotted, already established by
// tenant.created and entity.status.changed, and consumers subscribe to the
// wire name. The mapping is recorded in asyncapi.yaml and asserted by
// contract_test.go so the two cannot drift silently.
const (
	// ORG-02 §4.2 — Events produced.
	EventTenantCreated              = "tenant.created"               // TenantCreated
	EventTenantActivated            = "tenant.activated"             // TenantActivated
	EventTenantSuspended            = "tenant.suspended"             // TenantSuspended
	EventTenantResumed              = "tenant.resumed"               // (no spec name; the inverse of Suspended)
	EventTenantTerminationInitiated = "tenant.termination.initiated" // TenantTerminationInitiated
	EventTenantTerminated           = "tenant.terminated"            // TenantTerminated
	EventTenantDefaultsChanged      = "tenant.defaults.changed"      // (ChangeDefaultLocale)

	// ORG-03 §4.3 — Events produced.
	EventLegalEntityCreated        = "entity.created"                   // LegalEntityCreated
	EventLegalEntityProfileAmended = "entity.profile.amended"           // LegalEntityProfileAmended
	EventLegalEntityStatusChanged  = "entity.status.changed"            // LegalEntityStatusChanged
	EventRegisteredOfficeChanged   = "entity.registered_office.changed" // RegisteredOfficeChanged
	EventLegalEntityNameChanged    = "entity.legal_name.changed"        // (narrowing of ProfileAmended)

	// §8 NP5 — not a spec-named event, but a quarantined duplicate that nobody
	// is told about is a quarantine nobody acts on.
	EventRegistryConflictQuarantined = "entity.registry_conflict.quarantined"
	EventRegistryConflictResolved    = "entity.registry_conflict.resolved"
)

// TenantCommandEvent maps an ORG-02 named command to the event it produces.
//
// Returns "" for a command that produces no event of its own. Written as a
// mapping rather than a switch inside the service so a test can enumerate the
// commands and assert every one either produces a named event or is
// deliberately silent.
func TenantCommandEvent(command string) string {
	switch command {
	case "ActivateTenant":
		return EventTenantActivated
	case "SuspendTenant":
		return EventTenantSuspended
	case "ResumeTenant":
		return EventTenantResumed
	case "InitiateTermination":
		return EventTenantTerminationInitiated
	case "CompleteTermination":
		return EventTenantTerminated
	case "ChangeDefaultLocale":
		return EventTenantDefaultsChanged
	}
	return ""
}

// RecordSpec is the caller-supplied half of an outbox record.
type RecordSpec struct {
	EventType     string
	TenantID      string
	LegalEntityID string
	Jurisdiction  string
	ActorID       string
	CorrelationID string
	// PartitionKey orders events for one aggregate. Kafka guarantees ordering
	// only within a partition, so every event about one tenant or one entity
	// must carry the same key — otherwise a consumer can see an entity's
	// profile amendment before its creation.
	PartitionKey string
	Payload      map[string]any
}

// BuildRecord renders spec into an outbox record carrying the platform
// envelope.
//
// Returns an error rather than swallowing one: this record is about to be
// written inside a business transaction, and a payload that will not marshal
// must abort that transaction rather than commit the fact with no event. That
// is the whole point of the outbox, and it is the one respect in which this
// path must NOT behave like Publisher.emit, which logs and moves on.
func BuildRecord(spec RecordSpec) (*outbox.Record, error) {
	if spec.EventType == "" {
		return nil, fmt.Errorf("events: event type is required")
	}
	raw, err := json.Marshal(spec.Payload)
	if err != nil {
		return nil, fmt.Errorf("events: marshal payload for %s: %w", spec.EventType, err)
	}

	eventID := "evt-" + uuid.New().String()
	env := envelope{
		EventID:       eventID,
		EventType:     spec.EventType,
		EventVersion:  "1.0",
		EmittedAt:     time.Now().UTC(),
		SchemaVersion: "1.0",
		SourceService: "tenant-entity-registry-svc",
		TenantID:      spec.TenantID,
		LegalEntityID: spec.LegalEntityID,
		Jurisdiction:  spec.Jurisdiction,
		ActorID:       spec.ActorID,
		CorrelationID: spec.CorrelationID,
		Payload:       json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("events: marshal envelope for %s: %w", spec.EventType, err)
	}

	key := spec.PartitionKey
	if key == "" {
		key = spec.TenantID
	}

	// event_outbox.event_id is a UUID column, so the "evt-" prefix the wire
	// envelope carries cannot be stored in it. The envelope keeps the prefixed
	// form (consumers already match on it) and the outbox row keys on the bare
	// UUID — the same identity, spelled for two different consumers.
	return &outbox.Record{
		EventID:      eventID[len("evt-"):],
		EventType:    spec.EventType,
		TenantID:     spec.TenantID,
		PartitionKey: key,
		Payload:      data,
	}, nil
}
