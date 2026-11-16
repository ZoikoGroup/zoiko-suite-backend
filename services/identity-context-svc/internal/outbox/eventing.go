package outbox

// ZS-EVENT-001 migration. Every event this service publishes is still rendered
// by events.Render as the legacy envelope (event_type, payload, source_service
// ...), because that is what authorization-svc, the gateway and the SIEM
// consume. At the moment of enqueue that rendered record is turned into the
// canonical eventing envelope and written to the shared eventing_outbox, in the
// same transaction as before. The envelope still emits the legacy field names
// (expand/contract), so no consumer changes; what is new is the canonical type,
// the catalogue/ownership/schema governance, and delivery by eventing's relay
// with acknowledged Kafka writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	eventingoutbox "zoiko.io/eventing/outbox"
)

// PlatformTenant is the tenant id carried by an event that has no tenant: a
// resolution that failed before one was resolved, a login attempt against an
// unknown tenant. ZS-EVENT-001 section 4 requires a tenant on every event, and
// refusing to record such an event would lose exactly the ones an investigation
// needs. Consumers that filter by tenant never match it.
const PlatformTenant = "platform"

var (
	regionMu    sync.RWMutex
	eventRegion string
)

// SetEventRegion sets the residency region stamped on every event (EVENT_RESIDENCY_REGION).
// It is process-wide because residency is a property of the deployment, and it
// is deliberately unset by default: an event with no region is refused rather
// than guessed.
func SetEventRegion(region string) {
	regionMu.Lock()
	defer regionMu.Unlock()
	eventRegion = region
}

func region() string {
	regionMu.RLock()
	defer regionMu.RUnlock()
	return eventRegion
}

// legacyOverrides are the pre-standard names that do not follow the
// "domain.aggregate.fact" shape and so cannot be derived by rule.
var legacyOverrides = map[string]string{
	"session.invalidated":      "identity.session.invalidated",
	"principal.status.changed": "identity.principal.status-changed",
}

// CanonicalType maps a legacy event name to its canonical ZS-EVENT-001 type:
// "identity.mfa_factor.enrolled" -> "com.zoikosuite.identity.mfa-factor.enrolled".
// Underscores become hyphens (the standard is kebab-case); the identity domain is
// the one ZS-IAM-001 section 23 already names.
func CanonicalType(legacy string) string {
	n := strings.ToLower(strings.TrimSpace(legacy))
	if o, ok := legacyOverrides[n]; ok {
		n = o
	}
	n = strings.ReplaceAll(n, "_", "-")
	if !strings.HasPrefix(n, "identity.") {
		n = "identity." + n
	}
	return "com.zoikosuite." + n
}

// ToEnvelope converts a rendered legacy record into the canonical envelope.
func ToEnvelope(rec Record) (*envelope.Envelope, error) {
	var legacy struct {
		EmittedAt     time.Time       `json:"emitted_at"`
		SourceService string          `json:"source_service"`
		TenantID      string          `json:"tenant_id"`
		LegalEntityID string          `json:"legal_entity_id"`
		ActorID       string          `json:"actor_id"`
		CorrelationID string          `json:"correlation_id"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(rec.Payload, &legacy); err != nil {
		return nil, fmt.Errorf("outbox: event %q is not a rendered envelope: %w", rec.EventType, err)
	}
	if legacy.SourceService == "" {
		legacy.SourceService = "identity-context-svc"
	}
	if legacy.EmittedAt.IsZero() {
		legacy.EmittedAt = time.Now().UTC()
	}
	// rec.TenantID is what the caller set app.tenant_id to, so it wins.
	tenant := rec.TenantID
	if tenant == "" {
		tenant = legacy.TenantID
	}
	if tenant == "" {
		tenant = PlatformTenant
	}
	typ := CanonicalType(rec.EventType)
	spec := envelope.Spec{
		Type:            typ,
		LegacyType:      rec.EventType,
		Service:         legacy.SourceService,
		SchemaVersion:   "1.0.0",
		OccurredAt:      legacy.EmittedAt,
		TenantID:        tenant,
		LegalEntityID:   legacy.LegalEntityID,
		CorrelationID:   legacy.CorrelationID,
		ActorID:         legacy.ActorID,
		ResidencyRegion: region(),
		Classification:  envelope.Confidential,
		Data:            legacy.Payload,
	}
	if rec.PartitionKey != "" {
		spec.AggregateType = strings.Split(strings.TrimPrefix(typ, "com.zoikosuite.identity."), ".")[0]
		spec.AggregateID = rec.PartitionKey
	}
	env, err := envelope.New(spec)
	if err != nil {
		return nil, fmt.Errorf("outbox: build %s: %w", typ, err)
	}
	return env, nil
}

// EnqueueEventingTx converts rec and writes it to eventing_outbox inside tx.
// The caller must already have set app.tenant_id on tx to the event's tenant
// (PlatformTenant for a tenantless event), because the outbox's RLS policy
// requires it.
func EnqueueEventingTx(ctx context.Context, tx pgx.Tx, rec Record) error {
	if rec.EventType == "" {
		return errors.New("outbox: event_type is required")
	}
	env, err := ToEnvelope(rec)
	if err != nil {
		return err
	}
	return eventingoutbox.Enqueue(ctx, tx, env)
}

// EventingTenant is the tenant an event will be scoped to once converted.
func EventingTenant(recTenant string) string {
	if recTenant == "" {
		return PlatformTenant
	}
	return recTenant
}
