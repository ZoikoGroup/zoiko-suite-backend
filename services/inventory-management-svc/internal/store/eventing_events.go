package store

// BIZ-07 catalog events on the shared eventing outbox (ZS-EVENT-001).
//
// OfferingCreated, OfferingActivated, OfferingSuspended, OfferingRetired and
// OfferingVersionSuperseded are written to eventing_outbox in the SAME transaction as the
// change they announce, with canonical com.zoikosuite.business.* types. They used to be
// published by the handler after the commit through a Kafka writer whose failure was only
// logged, so a broker problem lost them silently. Payload schemas live in contracts/events and
// are enforced by the store tests. The old event_type is carried as the envelope's legacy
// type. The service's other events (items, locations, movements, valuation, stock counts)
// still use the direct publisher.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
	svcmiddleware "zoiko.io/inventory-management-svc/internal/middleware"
)

type catalogEventKind struct {
	canonicalType string
	legacyType    string
}

var (
	offeringCreatedEvent           = catalogEventKind{"com.zoikosuite.business.offering.created", "catalog.offering.created"}
	offeringActivatedEvent         = catalogEventKind{"com.zoikosuite.business.offering.activated", "catalog.offering.activated"}
	offeringSuspendedEvent         = catalogEventKind{"com.zoikosuite.business.offering.suspended", "catalog.offering.suspended"}
	offeringRetiredEvent           = catalogEventKind{"com.zoikosuite.business.offering.retired", "catalog.offering.retired"}
	offeringVersionSupersededEvent = catalogEventKind{"com.zoikosuite.business.offering-version.superseded", "catalog.offering_version.superseded"}
)

// enqueueCatalogEvent writes one catalog event to eventing_outbox in the caller's transaction.
// An empty event region fails the write (and rolls the change back) rather than emitting an
// event with a guessed region. The correlation id comes from the request context.
func (s *PgStore) enqueueCatalogEvent(ctx context.Context, tx pgx.Tx, k catalogEventKind, actorID, tenantID, legalEntityID, offeringID string, data map[string]any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            k.canonicalType,
		LegacyType:      k.legacyType,
		Service:         "inventory-management-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   "offering",
		AggregateID:     offeringID,
		CorrelationID:   svcmiddleware.CorrelationFromContext(ctx),
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		Classification:  envelope.Internal,
		Data:            data,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", k.canonicalType, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", k.canonicalType, err)
	}
	return nil
}
