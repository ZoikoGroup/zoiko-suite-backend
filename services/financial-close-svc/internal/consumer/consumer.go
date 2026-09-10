// Package consumer holds financial-close-svc's own event-handling logic
// for its ACC-18 lineage Kafka consumer — kept separate from
// internal/kafka's I/O shell (Runner) so it can be unit-tested without a
// real broker, the same split workflow-history-svc's and
// audit-event-store-svc's own consumers already use.
//
// Why a consumer, not an HTTP push: closing the AST/INV/PRJ domain
// spec's own §9 "source-to-report" assertion means asset-management-svc,
// inventory-management-svc and project-accounting-svc's own
// accounting-event-emitted moments need to reach financial-close-svc's
// lineage register. A synchronous "call financial-close-svc right after
// posting" HTTP call is not atomic with the GL post it's recording — a
// network blip or a financial-close-svc outage in that instant would
// silently lose the lineage edge forever, with nothing to retry it. This
// consumer instead derives edges from the SAME domain events those three
// services already publish to Kafka for other purposes — Kafka's own
// durable, at-least-once delivery means a financial-close-svc outage
// delays lineage recording, never loses it: the events sit in the topic
// until this consumer catches up.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

// Store is the narrow persistence contract this consumer depends on —
// deliberately just the one method, not the full financial-close-svc
// Store interface, so a fake is trivial in tests.
type Store interface {
	RecordLineageEdge(ctx context.Context, edge *domain.LineageEdge) error
}

// Consumer turns one raw Kafka message from asset-management-svc's,
// inventory-management-svc's or project-accounting-svc's own events
// topic into a lineage edge, when (and only when) that message is one of
// their four "accounting event emitted" signals. Every other event type
// on these topics (AssetRegistered, ItemActivated, ProjectCreated, ...)
// is real, ordinary traffic this consumer is expected to see and ignore.
type Consumer struct {
	store Store
	log   *zap.Logger
}

func New(store Store, log *zap.Logger) *Consumer {
	return &Consumer{store: store, log: log}
}

// envelope is this platform's own event contract (Doc 03 §19) — the
// exact shape asset-management-svc/inventory-management-svc/
// project-accounting-svc's own internal/events/publisher.go all publish,
// confirmed byte-for-byte identical across all three before this
// consumer was written.
type envelope struct {
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	Payload       json.RawMessage `json:"payload"`
}

// accountingEventEmittedPayload covers all four source events' own
// payload shape — each carries either run_id (AST-02/INV-04/PRJ-03) or
// event_id (AST-03), plus journal_id.
type accountingEventEmittedPayload struct {
	RunID     string `json:"run_id"`
	EventID   string `json:"event_id"`
	JournalID string `json:"journal_id"`
}

// lineageFromType maps each source's own "accounting event emitted"
// event_type to the from_type this lineage edge is recorded under.
// Every other event_type on these same topics is not in this map and is
// silently skipped — see the package doc comment.
var lineageFromType = map[string]string{
	"depreciation.run.accounting_event.emitted":    "depreciation_run",
	"asset.event.accounting_event.emitted":         "asset_event",
	"inventory.accounting_event.emitted":           "inventory_valuation_run",
	"project.recognition.accounting_event.emitted": "project_recognition_run",
}

// Handle is called once per Kafka message by internal/kafka.Runner. A
// nil return commits the message (handled, or correctly ignored); a
// non-nil return triggers Runner's own bounded retry, then dead-letters
// the message rather than blocking the partition — see internal/kafka's
// own doc comment for that policy.
func (c *Consumer) Handle(ctx context.Context, eventID string, raw []byte) error {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("lineage consumer: malformed envelope (event_id=%s): %w", eventID, err)
	}

	fromType, relevant := lineageFromType[env.EventType]
	if !relevant {
		return nil
	}

	var payload accountingEventEmittedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("lineage consumer: malformed payload for %s (event_id=%s): %w", env.EventType, eventID, err)
	}

	fromID := payload.RunID
	if fromID == "" {
		fromID = payload.EventID
	}
	if fromID == "" || payload.JournalID == "" || env.LegalEntityID == "" || env.TenantID == "" {
		return fmt.Errorf("lineage consumer: %s (event_id=%s) missing a required field (from_id/journal_id/legal_entity_id/tenant_id)", env.EventType, eventID)
	}

	// A Kafka consumer goroutine has no HTTP request, so the tenant
	// context RecordLineageEdge's own store layer requires must be
	// injected manually from the envelope's own tenant_id — the same
	// value that field carries for every other event on this topic.
	tenantCtx := svcmiddleware.WithTenant(ctx, env.TenantID)

	edge := &domain.LineageEdge{
		EdgeID:        uuid.NewString(),
		LegalEntityID: env.LegalEntityID,
		FromType:      fromType,
		FromID:        fromID,
		ToType:        "journal",
		ToID:          payload.JournalID,
		RecordedAt:    time.Now().UTC(),
	}
	if err := c.store.RecordLineageEdge(tenantCtx, edge); err != nil {
		return fmt.Errorf("lineage consumer: record edge for %s:%s -> journal:%s: %w", fromType, fromID, payload.JournalID, err)
	}
	c.log.Info("lineage edge recorded from consumed event",
		zap.String("event_type", env.EventType), zap.String("from_type", fromType), zap.String("from_id", fromID), zap.String("journal_id", payload.JournalID))
	return nil
}
