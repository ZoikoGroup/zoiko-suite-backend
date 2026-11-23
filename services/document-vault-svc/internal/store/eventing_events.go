package store

// BIZ-01 / BIZ-02 events on the shared eventing outbox (ZS-EVENT-001).
//
// Nine of this service's events move to the platform envelope and the
// eventing_outbox table: the five BIZ-01 events the DRC spec (ZS-SVC-S-001
// section 9.3) does not name, and the four BIZ-02 classification events. Each
// is written in the same transaction as the change it announces, with a
// canonical com.zoikosuite.business.* type, and its payload is validated
// against contracts/events by the store tests.
//
// document_created and document_version_committed stay on the older outbox on
// purpose: ZS-SVC-S-001 section 9.3 names them, and renaming them is a decision
// about two specs and their consumers that has not been made. The DRC events
// (retention, legal hold, preservation) are untouched too.
//
// The old event_type is carried as the envelope's legacy type, so a consumer
// that filters on it keeps working while the envelope still emits legacy fields.

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
)

// canonicalEvent describes where a legacy event type goes on the eventing outbox.
type canonicalEvent struct {
	canonicalType string
	aggregateType string
}

// canonicalDocumentEvents maps the legacy event_type of each migrated event to its
// canonical type and aggregate.
var canonicalDocumentEvents = map[string]canonicalEvent{
	"document.record_declared":              {"com.zoikosuite.business.document.record-declared", "document"},
	"document.superseded":                   {"com.zoikosuite.business.document.superseded", "document"},
	"document.archived":                     {"com.zoikosuite.business.document.archived", "document"},
	"document.disposition_requested":        {"com.zoikosuite.business.document.disposition-requested", "document"},
	"document.version_upload_quarantined":   {"com.zoikosuite.business.document.version-upload-quarantined", "document"},
	"classification.proposed":               {"com.zoikosuite.business.classification.proposed", "classification"},
	"classification.confirmed":              {"com.zoikosuite.business.classification.confirmed", "classification"},
	"classification.reclassified":           {"com.zoikosuite.business.classification.reclassified", "classification"},
	"classification.superseded":             {"com.zoikosuite.business.classification.superseded", "classification"},
}

// enqueueCanonicalEvent writes one migrated event to eventing_outbox in the caller's
// transaction. An empty event region fails the write (and rolls the change back)
// rather than emitting an event with a guessed region.
func (s *PgStore) enqueueCanonicalEvent(ctx context.Context, tx pgx.Tx, ce canonicalEvent, legacyType, documentID, tenantID, legalEntityID, actorID, correlationID string, payload map[string]any) error {
	aggregateID := documentID
	if ce.aggregateType == "classification" {
		// The aggregate is the classification record itself.
		if id, ok := payload["classification_id"].(string); ok && id != "" {
			aggregateID = id
		}
	}
	env, err := envelope.New(envelope.Spec{
		Type:            ce.canonicalType,
		LegacyType:      legacyType,
		Service:         "document-vault-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   ce.aggregateType,
		AggregateID:     aggregateID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		// Document and classification metadata is confidential business information.
		Classification: envelope.Confidential,
		Data:           payload,
	})
	if err != nil {
		return fmt.Errorf("build %s event: %w", ce.canonicalType, err)
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		return fmt.Errorf("enqueue %s event: %w", ce.canonicalType, err)
	}
	return nil
}
