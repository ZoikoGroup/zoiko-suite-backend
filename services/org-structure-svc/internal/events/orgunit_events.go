package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"zoiko.io/org-structure-svc/internal/outbox"
)

// ORG-05 events (ZS-SVC-I-001 s4.5 and s7). Wire names are lower.dotted, like every
// event in this estate; SpecEvent below maps each to the spec's name.
//
// The spec's event minimum is: tenant_id, object_id, object_version,
// effective_at, recorded_at, actor, correlation_id and, where material,
// evidence_ref. All of them are in the payload, so a consumer needs nothing
// else; tenant, legal entity, actor and correlation id are also on the platform
// envelope, as for every event in this service.
const (
	EventOrgUnitCreated           = "org.unit.created"
	EventOrgUnitActivated         = "org.unit.activated"
	EventOrgUnitDeactivated       = "org.unit.deactivated"
	EventOrgUnitReparented        = "org.unit.reparented"
	EventOrgUnitManagerChanged    = "org.unit.manager.changed"
	EventOrgUnitReparentRequested = "org.unit.reparent.requested"
	EventOrgUnitReparentApproved  = "org.unit.reparent.approved"
	EventOrgUnitReparentRejected  = "org.unit.reparent.rejected"
	EventOrgUnitReparentFailed    = "org.unit.reparent.failed"
)

// OrgEventNames lists every event this service can emit, for the contract test
// that keeps asyncapi.yaml honest.
var OrgEventNames = []string{
	EventOrgUnitCreated, EventOrgUnitActivated, EventOrgUnitDeactivated, EventOrgUnitReparented,
	EventOrgUnitManagerChanged, EventOrgUnitReparentRequested, EventOrgUnitReparentApproved,
	EventOrgUnitReparentRejected, EventOrgUnitReparentFailed,
}

const (
	ObjectOrgUnit       = "org_unit"
	ObjectChangeRequest = "org_unit_change_request"
)

// OrgEvent is everything needed to render one ORG-05 event.
type OrgEvent struct {
	Name          string
	TenantID      string
	LegalEntityID string
	// UnitID keys the Kafka partition, so every event about one unit (including
	// its change requests) is delivered in order.
	UnitID        string
	ObjectType    string
	ObjectID      string
	ObjectVersion int64
	// EffectiveAt is the date the change applies from (YYYY-MM-DD). Events about
	// something with no business date (a status change) use the recorded date.
	EffectiveAt   string
	RecordedAt    time.Time
	Actor         string
	CorrelationID string
	EvidenceRef   string
	// Details are the event-specific facts: old and new parent, manager, and so on.
	Details map[string]any
}

// orgPayload is the payload schema, version 1.0.
type orgPayload struct {
	TenantID      string         `json:"tenant_id"`
	LegalEntityID string         `json:"legal_entity_id"`
	ObjectType    string         `json:"object_type"`
	ObjectID      string         `json:"object_id"`
	ObjectVersion int64          `json:"object_version"`
	UnitID        string         `json:"unit_id"`
	EffectiveAt   string         `json:"effective_at"`
	RecordedAt    time.Time      `json:"recorded_at"`
	Actor         string         `json:"actor"`
	CorrelationID string         `json:"correlation_id"`
	EvidenceRef   string         `json:"evidence_ref,omitempty"`
	Details       map[string]any `json:"details"`
}

// BuildOrgEvent renders the event into an outbox record. It refuses an event
// that lacks what the spec requires, so a command can never commit an event a
// consumer could not order or attribute.
func BuildOrgEvent(in OrgEvent) (outbox.Record, error) {
	switch {
	case in.Name == "":
		return outbox.Record{}, fmt.Errorf("org event: name is required")
	case in.TenantID == "":
		return outbox.Record{}, fmt.Errorf("org event %s: tenant_id is required", in.Name)
	case in.ObjectID == "" || in.UnitID == "":
		return outbox.Record{}, fmt.Errorf("org event %s: object_id and unit_id are required", in.Name)
	case in.ObjectVersion < 1:
		return outbox.Record{}, fmt.Errorf("org event %s: object_version must be at least 1", in.Name)
	case in.EffectiveAt == "":
		return outbox.Record{}, fmt.Errorf("org event %s: effective_at is required", in.Name)
	}
	if in.RecordedAt.IsZero() {
		in.RecordedAt = time.Now()
	}
	in.RecordedAt = in.RecordedAt.UTC()
	if in.Details == nil {
		in.Details = map[string]any{}
	}

	payload, err := json.Marshal(orgPayload{
		TenantID: in.TenantID, LegalEntityID: in.LegalEntityID, ObjectType: in.ObjectType, ObjectID: in.ObjectID,
		ObjectVersion: in.ObjectVersion, UnitID: in.UnitID, EffectiveAt: in.EffectiveAt, RecordedAt: in.RecordedAt,
		Actor: in.Actor, CorrelationID: in.CorrelationID, EvidenceRef: in.EvidenceRef, Details: in.Details,
	})
	if err != nil {
		return outbox.Record{}, fmt.Errorf("org event %s: payload: %w", in.Name, err)
	}

	eventID := uuid.NewString()
	body, err := json.Marshal(envelope{
		EventID: "evt-" + eventID, EventType: in.Name, EventVersion: "1.0", EmittedAt: in.RecordedAt,
		SchemaVersion: "1.0", SourceService: "org-structure-svc", TenantID: in.TenantID,
		LegalEntityID: in.LegalEntityID, ActorID: in.Actor, CorrelationID: in.CorrelationID, Payload: payload,
	})
	if err != nil {
		return outbox.Record{}, fmt.Errorf("org event %s: envelope: %w", in.Name, err)
	}
	return outbox.Record{
		EventID: eventID, EventType: in.Name, TenantID: in.TenantID, PartitionKey: in.UnitID, Payload: body,
	}, nil
}

// SpecEvent maps each wire name to the event name ZS-SVC-I-001 s4.5 gives it;
// "" means the spec names no such event (this service adds it). The wire names
// follow the estate convention (lower.dotted) and the spec names are
// PascalCase, so the mapping is written down here and checked against
// asyncapi.yaml's x-spec-event by a contract test, not reconstructed by eye.
var SpecEvent = map[string]string{
	EventOrgUnitCreated:           "OrgUnitCreated",
	EventOrgUnitReparented:        "OrgUnitReparented",
	EventOrgUnitManagerChanged:    "OrgUnitManagerChanged",
	EventOrgUnitDeactivated:       "OrgUnitDeactivated",
	EventOrgUnitActivated:         "",
	EventOrgUnitReparentRequested: "",
	EventOrgUnitReparentApproved:  "",
	EventOrgUnitReparentRejected:  "",
	EventOrgUnitReparentFailed:    "",
}
