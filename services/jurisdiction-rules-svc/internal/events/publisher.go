// Package events contains the domain event publisher for jurisdiction-rules-svc.
package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	svcenvelope "zoiko.io/jurisdiction-rules-svc/internal/envelope"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
	"zoiko.io/jurisdiction-rules-svc/internal/telemetry"
)

const (
	EventJurisdictionCreated     = "jurisdiction.created"
	EventJurisdictionDeactivated = "jurisdiction.deactivated"
	EventRuleUpdated             = "jurisdiction.rule.updated"
	EventRuleActivated           = "jurisdiction.rule.activated"
	EventLegalDriftDetected      = "legal.drift.detected"
)

type Publisher interface {
	PublishJurisdictionCreated(ctx context.Context, j domain.Jurisdiction, correlationID string) error
	PublishJurisdictionDeactivated(ctx context.Context, j domain.Jurisdiction, correlationID string) error
	PublishRuleUpdated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error
	PublishRuleActivated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error
	PublishLegalDriftDetected(ctx context.Context, r domain.JurisdictionRule, e domain.DriftEvent, correlationID string) error
	FlushPending(ctx context.Context, q store.Querier) error
}

// envelope is the event envelope every event of this service is published in.
// classification, payload_hash and request_id are the Event Catalogue §4
// fields the envelope lacked: what handling class the payload carries, a
// digest a consumer can check the payload against, and the request that
// caused the event. Tenant and entity are deliberately absent — this is
// platform reference data.
type envelope struct {
	EventID          string          `json:"event_id"`
	EventType        string          `json:"event_type"`
	EventVersion     string          `json:"event_version"`
	EmittedAt        time.Time       `json:"emitted_at"`
	SchemaVersion    string          `json:"schema_version"`
	SourceService    string          `json:"source_service"`
	JurisdictionID   string          `json:"jurisdiction_id,omitempty"`
	ActorID          string          `json:"actor_id,omitempty"`
	CorrelationID    string          `json:"correlation_id"`
	RequestID        string          `json:"request_id,omitempty"`
	AggregateVersion int64           `json:"aggregate_version,omitempty"`
	Classification   string          `json:"classification"`
	PayloadHash      string          `json:"payload_hash"`
	Payload          json.RawMessage `json:"payload"`
}

func effectiveActor(createdBy string, updatedBy *string) string {
	if updatedBy != nil && *updatedBy != "" {
		return *updatedBy
	}
	return createdBy
}

func rulePayload(r domain.JurisdictionRule) map[string]any {
	return map[string]any{
		"jurisdiction_rule_id":    r.JurisdictionRuleID,
		"jurisdiction_id":         r.JurisdictionID,
		"rule_domain":             r.RuleDomain,
		"rule_code":               r.RuleCode,
		"rule_name":               r.RuleName,
		"rule_status":             r.RuleStatus,
		"legal_drift_state":       r.LegalDriftState,
		"effective_from":          r.EffectiveFrom,
		"effective_to":            r.EffectiveTo,
		"rule_payload":            r.RulePayload,
		"source_reference":        r.SourceReference,
		"external_feed_reference": r.ExternalFeedReference,
		"data_classification":     r.DataClassification,
		"schema_version":          r.SchemaVersion,
	}
}

func partitionKey(payload map[string]any) string {
	for _, k := range []string{"aggregate_id", "jurisdiction_rule_id", "jurisdiction_id"} {
		if v, ok := payload[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// OutboxPublisher writes events to the transactional outbox table.
// The actual Kafka publish happens asynchronously via a background worker.
//
// Publish* buffers an event in the batch carried by ctx (WithBatch) and
// FlushPending writes that batch inside the caller's transaction. The buffer
// used to be one slice on the shared publisher, so concurrent requests flushed
// each other's events: an event could commit with another request's
// transaction while its own rolled back, or vanish with a rollback that was
// not its own.
type OutboxPublisher struct {
	log   *zap.Logger
	store *store.PgStore
	topic string
}

type OutboxPendingEvent struct {
	EventType    string
	Topic        string
	PartitionKey string
	Payload      []byte
}

// ErrNoBatch is returned by Publish* when ctx carries no WithBatch batch:
// there is no transaction for the event to commit with.
var ErrNoBatch = errors.New("events: publish outside a WithBatch transaction scope")

type batchKey struct{}

type batch struct {
	mu     sync.Mutex
	events []OutboxPendingEvent
}

// WithBatch returns a context that buffers the events published under it
// until FlushPending writes them. Call it once per transaction attempt.
func WithBatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, batchKey{}, &batch{})
}

func batchFrom(ctx context.Context) *batch {
	b, _ := ctx.Value(batchKey{}).(*batch)
	return b
}

func NewOutboxPublisher(log *zap.Logger, store *store.PgStore, topic string) *OutboxPublisher {
	return &OutboxPublisher{log: log, store: store, topic: topic}
}

func (p *OutboxPublisher) addPending(ctx context.Context, eventType, partitionKey string, payload []byte) error {
	b := batchFrom(ctx)
	if b == nil {
		return fmt.Errorf("event %q: %w", eventType, ErrNoBatch)
	}
	b.mu.Lock()
	b.events = append(b.events, OutboxPendingEvent{
		EventType:    eventType,
		Topic:        p.topic,
		PartitionKey: partitionKey,
		Payload:      payload,
	})
	b.mu.Unlock()
	return nil
}

// FlushPending writes the ctx batch to the outbox within the given transaction
// and empties it. This must be called within the same transaction as the
// domain change.
func (p *OutboxPublisher) FlushPending(ctx context.Context, q store.Querier) error {
	b := batchFrom(ctx)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	pending := b.events
	b.events = nil
	b.mu.Unlock()
	for _, e := range pending {
		if err := p.store.AddEventToOutbox(ctx, q, e.EventType, e.Topic, e.PartitionKey, e.Payload); err != nil {
			return err
		}
	}
	return nil
}

// buildEnvelope is the one place an event envelope is assembled, so the
// envelope's event_type is always the type the event was published as.
func buildEnvelope(ctx context.Context, eventType, jurisdictionID, actorID, correlationID, classification string, aggregateVersion int64, payload []byte) ([]byte, error) {
	sum := sha256.Sum256(payload)
	env := envelope{
		EventID:          "evt-" + uuid.New().String(),
		EventType:        eventType,
		EventVersion:     "1.0",
		EmittedAt:        time.Now().UTC(),
		SchemaVersion:    "1.0",
		SourceService:    "jurisdiction-rules-svc",
		JurisdictionID:   jurisdictionID,
		ActorID:          actorID,
		CorrelationID:    correlationID,
		Classification:   classification,
		PayloadHash:      "sha256:" + hex.EncodeToString(sum[:]),
		AggregateVersion: aggregateVersion,
		Payload:          json.RawMessage(payload),
	}
	if reqEnv, ok := svcenvelope.FromContext(ctx); ok {
		env.RequestID = reqEnv.RequestID
	}
	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}
	return data, nil
}

func (p *OutboxPublisher) emitToOutbox(ctx context.Context, q store.Querier, eventType, correlationID, jurisdictionID, actorID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	classification, _ := payload["data_classification"].(string)
	if classification == "" {
		classification = "INTERNAL"
	}
	data, err := buildEnvelope(ctx, eventType, jurisdictionID, actorID, correlationID, classification, 0, raw)
	if err != nil {
		return err
	}
	return p.store.AddEventToOutbox(ctx, q, eventType, p.topic, partitionKey(payload), data)
}

func (p *OutboxPublisher) publish(ctx context.Context, eventType, partitionKey, jurisdictionID, actorID, correlationID, classification string, aggregateVersion int64, payload []byte) error {
	data, err := buildEnvelope(ctx, eventType, jurisdictionID, actorID, correlationID, classification, aggregateVersion, payload)
	if err != nil {
		return err
	}
	return p.addPending(ctx, eventType, partitionKey, data)
}

func (p *OutboxPublisher) PublishJurisdictionCreated(ctx context.Context, j domain.Jurisdiction, correlationID string) error {
	return p.publish(ctx, EventJurisdictionCreated, j.JurisdictionID, j.JurisdictionID,
		j.CreatedByPrincipalID, correlationID, j.DataClassification, j.RowVersion, p.marshalJurisdictionCreated(j))
}

func (p *OutboxPublisher) PublishJurisdictionDeactivated(ctx context.Context, j domain.Jurisdiction, correlationID string) error {
	return p.publish(ctx, EventJurisdictionDeactivated, j.JurisdictionID, j.JurisdictionID,
		effectiveActor(j.CreatedByPrincipalID, j.UpdatedByPrincipalID), correlationID, j.DataClassification, j.RowVersion, p.marshalJurisdictionDeactivated(j))
}

func (p *OutboxPublisher) PublishRuleUpdated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error {
	return p.publish(ctx, EventRuleUpdated, r.JurisdictionRuleID, r.JurisdictionID,
		effectiveActor(r.CreatedByPrincipalID, r.UpdatedByPrincipalID), correlationID, r.DataClassification, r.RowVersion, p.marshalRule(r))
}

func (p *OutboxPublisher) PublishRuleActivated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error {
	return p.publish(ctx, EventRuleActivated, r.JurisdictionRuleID, r.JurisdictionID,
		effectiveActor(r.CreatedByPrincipalID, r.UpdatedByPrincipalID), correlationID, r.DataClassification, r.RowVersion, p.marshalRule(r))
}

func (p *OutboxPublisher) PublishLegalDriftDetected(ctx context.Context, r domain.JurisdictionRule, e domain.DriftEvent, correlationID string) error {
	return p.publish(ctx, EventLegalDriftDetected, r.JurisdictionRuleID, r.JurisdictionID,
		e.RecordedByPrincipalID, correlationID, r.DataClassification, r.RowVersion, p.marshalDrift(r, e))
}

func (p *OutboxPublisher) marshalJurisdictionCreated(j domain.Jurisdiction) []byte {
	data, _ := json.Marshal(map[string]any{
		"jurisdiction_id":         j.JurisdictionID,
		"jurisdiction_code":       j.JurisdictionCode,
		"jurisdiction_name":       j.JurisdictionName,
		"jurisdiction_type":       j.JurisdictionType,
		"parent_jurisdiction_id":  j.ParentJurisdictionID,
		"authority_type":          j.AuthorityType,
		"effective_from":          j.EffectiveFrom,
		"effective_to":            j.EffectiveTo,
		"data_classification":     j.DataClassification,
		"created_by_principal_id": j.CreatedByPrincipalID,
		"created_at":              j.CreatedAt,
	})
	return data
}

func (p *OutboxPublisher) marshalJurisdictionDeactivated(j domain.Jurisdiction) []byte {
	data, _ := json.Marshal(map[string]any{
		"jurisdiction_id":         j.JurisdictionID,
		"jurisdiction_code":       j.JurisdictionCode,
		"active_flag":             j.ActiveFlag,
		"effective_to":            j.EffectiveTo,
		"updated_at":              j.UpdatedAt,
		"updated_by_principal_id": j.UpdatedByPrincipalID,
	})
	return data
}

func (p *OutboxPublisher) marshalRule(r domain.JurisdictionRule) []byte {
	data, _ := json.Marshal(map[string]any{
		"jurisdiction_rule_id":    r.JurisdictionRuleID,
		"jurisdiction_id":         r.JurisdictionID,
		"rule_domain":             r.RuleDomain,
		"rule_code":               r.RuleCode,
		"rule_name":               r.RuleName,
		"rule_status":             r.RuleStatus,
		"legal_drift_state":       r.LegalDriftState,
		"effective_from":          r.EffectiveFrom,
		"effective_to":            r.EffectiveTo,
		"rule_payload":            r.RulePayload,
		"source_reference":        r.SourceReference,
		"external_feed_reference": r.ExternalFeedReference,
		"data_classification":     r.DataClassification,
		"schema_version":          r.SchemaVersion,
		"precedence_level":        r.PrecedenceLevel,
		"rule_version":            r.RuleVersion,
		"supersedes_rule_id":      r.SupersedesRuleID,
		"row_version":             r.RowVersion,
	})
	return data
}

func (p *OutboxPublisher) marshalDrift(r domain.JurisdictionRule, e domain.DriftEvent) []byte {
	data, _ := json.Marshal(map[string]any{
		"drift_event_id":           e.DriftEventID,
		"jurisdiction_rule_id":     r.JurisdictionRuleID,
		"jurisdiction_id":          r.JurisdictionID,
		"rule_domain":              r.RuleDomain,
		"rule_code":                r.RuleCode,
		"from_state":               e.FromState,
		"to_state":                 e.ToState,
		"reason":                   e.Reason,
		"external_feed_reference":  r.ExternalFeedReference,
		"source_reference":         r.SourceReference,
		"effective_at":             e.EffectiveAt,
		"recorded_by_principal_id": e.RecordedByPrincipalID,
	})
	return data
}

// OutboxWorker publishes events from the outbox to Kafka.
type OutboxWorker struct {
	log       *zap.Logger
	store     *store.PgStore
	producer  *kafka.Writer
	topic     string
	metrics   *telemetry.Metrics
	interval  time.Duration
	batchSize int
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewOutboxWorker(log *zap.Logger, store *store.PgStore, producer *kafka.Writer, topic string, metrics *telemetry.Metrics) *OutboxWorker {
	return &OutboxWorker{
		log:       log,
		store:     store,
		producer:  producer,
		topic:     topic,
		metrics:   metrics,
		interval:  5 * time.Second,
		batchSize: 100,
		stopCh:    make(chan struct{}),
	}
}

func (w *OutboxWorker) Start() {
	w.wg.Add(1)
	go w.run()
	w.log.Info("outbox worker started", zap.String("topic", w.topic))
}

func (w *OutboxWorker) Stop() {
	close(w.stopCh)
	w.wg.Wait()
	w.log.Info("outbox worker stopped")
}

func (w *OutboxWorker) run() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.processBatch()
		}
	}
}

func (w *OutboxWorker) processBatch() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, err := w.store.GetPendingOutboxEvents(ctx, w.batchSize)
	if err != nil {
		w.log.Error("outbox: failed to fetch pending events", zap.Error(err))
		return
	}
	if w.metrics != nil {
		w.metrics.OutboxPendingEvents.Set(float64(len(events)))
	}
	if len(events) == 0 {
		return
	}

	w.log.Debug("outbox: processing batch", zap.Int("count", len(events)))

	for _, e := range events {
		var key []byte
		if e.PartitionKey != nil {
			key = []byte(*e.PartitionKey)
		}
		msg := kafka.Message{
			Key:   key,
			Value: e.EventPayload,
			Topic: w.topic,
		}
		if err := w.producer.WriteMessages(ctx, msg); err != nil {
			w.log.Error("outbox: kafka write failed",
				zap.String("outbox_id", e.OutboxID),
				zap.String("event_type", e.EventType),
				zap.Error(err),
			)
			if w.metrics != nil {
				w.metrics.OutboxPublishFailuresTotal.WithLabelValues(e.EventType).Inc()
			}
			if markErr := w.store.MarkOutboxEventFailed(ctx, e.OutboxID, err.Error()); markErr != nil {
				w.log.Error("outbox: failed to mark event failed", zap.Error(markErr))
			} else if e.PublishAttempts+1 >= store.MaxOutboxAttempts {
				// GetPendingOutboxEvents skips it from now on, so the events
				// behind it flow again; it stays in event_outbox for a person.
				w.log.Error("outbox: event parked after repeated publish failures",
					zap.String("outbox_id", e.OutboxID), zap.String("event_type", e.EventType),
					zap.Int("attempts", store.MaxOutboxAttempts))
				if w.metrics != nil {
					w.metrics.OutboxParkedTotal.WithLabelValues(e.EventType).Inc()
				}
			}
			// Stop processing this batch on first failure to preserve order
			return
		}
		if err := w.store.MarkOutboxEventPublished(ctx, e.OutboxID); err != nil {
			w.log.Error("outbox: failed to mark event published", zap.Error(err))
		}
	}

	w.log.Debug("outbox: batch processed", zap.Int("count", len(events)))
}

// NoopPublisher drops every event. Used when KAFKA_BROKERS is unset in local
// development, so the service runs standalone without a broker while still
// exercising the same call sites. Never selected in production or staging —
// main.go refuses to start without brokers there.
type NoopPublisher struct {
	log *zap.Logger
}

func NewNoopPublisher(log *zap.Logger) *NoopPublisher { return &NoopPublisher{log: log} }

func (p *NoopPublisher) publish(eventType string) error {
	p.log.Debug("event dropped — no Kafka brokers configured", zap.String("event_type", eventType))
	return nil
}

func (p *NoopPublisher) PublishJurisdictionCreated(context.Context, domain.Jurisdiction, string) error {
	return p.publish(EventJurisdictionCreated)
}

func (p *NoopPublisher) PublishJurisdictionDeactivated(context.Context, domain.Jurisdiction, string) error {
	return p.publish(EventJurisdictionDeactivated)
}

func (p *NoopPublisher) PublishRuleUpdated(context.Context, domain.JurisdictionRule, string) error {
	return p.publish(EventRuleUpdated)
}

func (p *NoopPublisher) PublishRuleActivated(context.Context, domain.JurisdictionRule, string) error {
	return p.publish(EventRuleActivated)
}

func (p *NoopPublisher) PublishLegalDriftDetected(context.Context, domain.JurisdictionRule, domain.DriftEvent, string) error {
	return p.publish(EventLegalDriftDetected)
}

func (p *NoopPublisher) FlushPending(context.Context, store.Querier) error {
	return nil
}
