package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"

	svcenvelope "zoiko.io/commercial-account-svc/internal/envelope"
)

// Events are written through the shared transactional outbox
// (services/eventing, ZS-EVENT-001 §6): emit inserts one eventing_outbox row
// on the caller's transaction — the one carrying the business write — and the
// shared relay (cmd/server) delivers it. Every write path in the service
// calls emit; there is no other way an event leaves.

// PlatformScopeID is the tenant of a platform-scope fact: something
// ZoikoSuite, as the seller, did to its own catalogue or policy (a product, a
// price version, a meter, a transition rule, a migration offer, an
// entitlement or dunning policy) that belongs to no customer organization.
// Every event needs a tenant (ZS-EVENT-001 §4); inventing a customer for these
// would be wrong and a NULL would hide them, so they carry the platform's own
// scope id — the same constant authorization-svc and capability-registry-svc
// use. Such events are only ever written on the seller plane.
const PlatformScopeID = "00000000-0000-0000-0000-00000000f001"

// canonicalType is the ZS-EVENT-001 §5.1 type for one of this service's event
// names: com.zoikosuite.commercial.<aggregate>.<fact>. "commercial" is the
// domain the Event Catalogue's Commercial Platform family (§19) uses; the
// aggregate and fact keep the service's own words in lower-case kebab form,
// which also reproduces every catalogue name the service already emits
// (subscription.started, subscription.changed, platform-invoice.issued,
// platform-payment.settled). The one spelling difference is taken from the
// catalogue: subscription.canceled -> commercial.subscription.cancelled.
func canonicalType(legacy string) string {
	if c, ok := canonicalOverrides[legacy]; ok {
		return c
	}
	return "com.zoikosuite.commercial." + strings.ReplaceAll(legacy, "_", "-")
}

var canonicalOverrides = map[string]string{
	"subscription.canceled": "com.zoikosuite.commercial.subscription.cancelled",
}

// Option configures a PgStore.
type Option func(*PgStore)

// WithEventProfile sets the governed envelope attributes stamped on every
// event: residency region (EVENT_RESIDENCY_REGION) and data classification
// (EVENT_CLASSIFICATION). Without them every write fails at enqueue time
// rather than publishing an event with an invented value.
func WithEventProfile(region string, classification envelope.Classification) Option {
	return func(s *PgStore) { s.eventRegion, s.eventClassification = region, classification }
}

// emit builds the canonical envelope for one fact and inserts it on tx — one
// INSERT, no extra round-trip.
//
//   - legacyType is the service's event name, kept as the legacy event_type
//     existing consumers (audit-event-store-svc) read; aggregateType is the
//     service's aggregate noun, kebab-cased for the envelope and partition key.
//   - tenantID is the organization the fact belongs to, or PlatformScopeID.
//     It must match the transaction's own scope: eventing_outbox's RLS policy
//     (migration 000021) admits the transaction's tenant, the seller plane,
//     and the relay.
//   - actor is the principal the write path already records; when a path has
//     none in scope, the request envelope's authenticated actor is used. The
//     correlation id always comes from the request envelope (mandatory on
//     writes); a background worker has none, and none is invented.
//   - LegalEntityID is deliberately empty: the commercial objects carry no
//     legal entity, and the request's X-Legal-Entity-Id is the caller's
//     context, not the entity the fact belongs to.
func (s *PgStore) emit(ctx context.Context, tx pgx.Tx, legacyType, aggregateType, aggregateID, tenantID, actor string, data any) error {
	req := svcenvelope.MustFromContext(ctx)
	if actor == "" {
		actor = req.Actor()
	}
	env, err := envelope.New(envelope.Spec{
		Type:            canonicalType(legacyType),
		LegacyType:      legacyType,
		Service:         "commercial-account-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		AggregateType:   strings.ReplaceAll(aggregateType, "_", "-"),
		AggregateID:     aggregateID,
		CorrelationID:   req.CorrelationID,
		ActorID:         actor,
		ResidencyRegion: s.eventRegion,
		Classification:  s.eventClassification,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", legacyType, err)
	}
	return outbox.Enqueue(ctx, tx, env)
}
