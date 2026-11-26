package store

// BIZ-09 events on the shared eventing outbox (ZS-EVENT-001).
//
// CommentAdded, CommentEdited, CommentModerated, MentionCreated and ThreadResolved are
// written to eventing_outbox in the SAME transaction as the change they announce, with
// canonical com.zoikosuite.business.* types. They used to be published by the handler after
// the commit through a Kafka writer whose failure was only logged, so a broker problem lost
// the event with no trace. An idempotent replay returns before any event is written, so a
// retried request announces nothing twice. Payload schemas live in contracts/events and are
// enforced by the store tests. The old event_type is carried as the envelope's legacy type.
// Payloads carry identifiers only, never the comment body.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/comments-collaboration-svc/internal/domain"
	"zoiko.io/eventing/envelope"
	"zoiko.io/eventing/outbox"
)

type commentEventKind struct {
	canonicalType string
	legacyType    string
	aggregateType string
}

var (
	commentAddedEvent     = commentEventKind{"com.zoikosuite.business.comment.added", "comment.added", "comment"}
	commentEditedEvent    = commentEventKind{"com.zoikosuite.business.comment.edited", "comment.edited", "comment"}
	commentModeratedEvent = commentEventKind{"com.zoikosuite.business.comment.moderated", "comment.moderated", "comment"}
	mentionCreatedEvent   = commentEventKind{"com.zoikosuite.business.mention.created", "mention.created", "comment"}
	threadResolvedEvent   = commentEventKind{"com.zoikosuite.business.thread.resolved", "thread.resolved", "thread"}
)

// eventClassification maps a thread's classification onto the envelope's. A thread that
// predates classification (none recorded) is treated as confidential, never as public.
func eventClassification(c string) envelope.Classification {
	switch domain.Classification(c) {
	case domain.ClassificationPublic:
		return envelope.Public
	case domain.ClassificationInternal:
		return envelope.Internal
	case domain.ClassificationRestricted:
		return envelope.Restricted
	default:
		return envelope.Confidential
	}
}

// threadScope returns the legal entity and classification of a thread, inside the caller's
// transaction, for the commands that act on a comment and so do not already hold the thread.
func threadScope(ctx context.Context, tx pgx.Tx, tenantID, threadID string) (legalEntityID, classification string, err error) {
	var class *string
	err = tx.QueryRow(ctx, `SELECT legal_entity_id, classification FROM comment_threads WHERE thread_id=$1 AND tenant_id=$2`, threadID, tenantID).Scan(&legalEntityID, &class)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", domain.ErrThreadNotFound
	}
	if err != nil {
		return "", "", err
	}
	if class != nil {
		classification = *class
	}
	return legalEntityID, classification, nil
}

// enqueueCommentEvent writes one event to eventing_outbox in the caller's transaction. An
// empty event region fails the write (and rolls the change back) rather than emitting an
// event with a guessed region.
func (s *PgStore) enqueueCommentEvent(ctx context.Context, tx pgx.Tx, k commentEventKind, correlationID, actorID, tenantID, legalEntityID, classification, aggregateID string, data map[string]any) error {
	env, err := envelope.New(envelope.Spec{
		Type:            k.canonicalType,
		LegacyType:      k.legacyType,
		Service:         "comments-collaboration-svc",
		SchemaVersion:   "1.0.0",
		OccurredAt:      time.Now().UTC(),
		TenantID:        tenantID,
		LegalEntityID:   legalEntityID,
		AggregateType:   k.aggregateType,
		AggregateID:     aggregateID,
		CorrelationID:   correlationID,
		ActorID:         actorID,
		ResidencyRegion: s.eventRegion,
		Classification:  eventClassification(classification),
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
