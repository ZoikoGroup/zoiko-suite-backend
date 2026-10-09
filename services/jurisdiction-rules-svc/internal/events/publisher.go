// Package events contains the domain event publisher for jurisdiction-rules-svc.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
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

type envelope struct {
	EventID        string          `json:"event_id"`
	EventType      string          `json:"event_type"`
	EventVersion   string          `json:"event_version"`
	EmittedAt      time.Time       `json:"emitted_at"`
	SchemaVersion  string          `json:"schema_version"`
	SourceService  string          `json:"source_service"`
	JurisdictionID string          `json:"jurisdiction_id,omitempty"`
	ActorID        string          `json:"actor_id,omitempty"`
	CorrelationID  string          `json:"correlation_id"`
	Payload        json.RawMessage `json:"payload"`
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
type OutboxPublisher struct {
	log   *zap.Logger
	store *store.PgStore
	topic string
	mu    sync.Mutex
	// pending tracks events in the current transaction for testing
	pending []OutboxPendingEvent
}

type OutboxPendingEvent struct {
	EventType    string
	Topic        string
	PartitionKey string
	Payload      []byte
}

func NewOutboxPublisher(log *zap.Logger, store *store.PgStore, topic string) *OutboxPublisher {
	return &OutboxPublisher{log: log, store: store, topic: topic}
}

func (p *OutboxPublisher) addPending(eventType, topic, partitionKey string, payload []byte) {
	p.mu.Lock()
	p.pending = append(p.pending, OutboxPendingEvent{
		EventType:    eventType,
		Topic:        topic,
		PartitionKey: partitionKey,
		Payload:      payload,
	})
	p.mu.Unlock()
}

func (p *OutboxPublisher) takePending() []OutboxPendingEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending := p.pending
	p.pending = nil
	return pending
}

// FlushPending writes all pending events to the outbox within the given transaction.
// This must be called within the same transaction as the domain change.
func (p *OutboxPublisher) FlushPending(ctx context.Context, q store.Querier) error {
	pending := p.takePending()
	for _, e := range pending {
		if err := p.store.AddEventToOutbox(ctx, q, e.EventType, e.Topic, e.PartitionKey, e.Payload); err != nil {
			return err
		}
	}
	return nil
}

func (p *OutboxPublisher) emitToOutbox(ctx context.Context, q store.Querier, eventType, correlationID, jurisdictionID, actorID string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("event %q: marshal payload: %w", eventType, err)
	}
	env := envelope{
		EventID:        "evt-" + uuid.New().String(),
		EventType:      eventType,
		EventVersion:   "1.0",
		EmittedAt:      time.Now().UTC(),
		SchemaVersion:  "1.0",
		SourceService:  "jurisdiction-rules-svc",
		JurisdictionID: jurisdictionID,
		ActorID:        actorID,
		CorrelationID:  correlationID,
		Payload:        json.RawMessage(raw),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("event %q: marshal envelope: %w", eventType, err)
	}

	pk := partitionKey(payload)
	return p.store.AddEventToOutbox(ctx, q, eventType, p.topic, pk, data)
}

func (p *OutboxPublisher) PublishJurisdictionCreated(ctx context.Context, j domain.Jurisdiction, correlationID string) error {
	p.addPending(EventJurisdictionCreated, p.topic, j.JurisdictionID, p.buildJurisdictionCreatedPayload(j, correlationID))
	return nil
}

func (p *OutboxPublisher) PublishJurisdictionDeactivated(ctx context.Context, j domain.Jurisdiction, correlationID string) error {
	p.addPending(EventJurisdictionDeactivated, p.topic, j.JurisdictionID, p.buildJurisdictionDeactivatedPayload(j, correlationID))
	return nil
}

func (p *OutboxPublisher) PublishRuleUpdated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error {
	p.addPending(EventRuleUpdated, p.topic, r.JurisdictionRuleID, p.buildRulePayload(r, correlationID))
	return nil
}

func (p *OutboxPublisher) PublishRuleActivated(ctx context.Context, r domain.JurisdictionRule, correlationID string) error {
	p.addPending(EventRuleActivated, p.topic, r.JurisdictionRuleID, p.buildRulePayload(r, correlationID))
	return nil
}

func (p *OutboxPublisher) PublishLegalDriftDetected(ctx context.Context, r domain.JurisdictionRule, e domain.DriftEvent, correlationID string) error {
	p.addPending(EventLegalDriftDetected, p.topic, r.JurisdictionRuleID, p.buildDriftPayload(r, e, correlationID))
	return nil
}

func (p *OutboxPublisher) buildJurisdictionCreatedPayload(j domain.Jurisdiction, correlationID string) []byte {
	env := envelope{
		EventID:        "evt-" + uuid.New().String(),
		EventType:      EventJurisdictionCreated,
		EventVersion:   "1.0",
		EmittedAt:      time.Now().UTC(),
		SchemaVersion:  "1.0",
		SourceService:  "jurisdiction-rules-svc",
		JurisdictionID: j.JurisdictionID,
		ActorID:        j.CreatedByPrincipalID,
		CorrelationID:  correlationID,
		Payload:        json.RawMessage(p.marshalJurisdictionCreated(j)),
	}
	data, _ := json.Marshal(env)
	return data
}

func (p *OutboxPublisher) buildJurisdictionDeactivatedPayload(j domain.Jurisdiction, correlationID string) []byte {
	env := envelope{
		EventID:        "evt-" + uuid.New().String(),
		EventType:      EventJurisdictionDeactivated,
		EventVersion:   "1.0",
		EmittedAt:      time.Now().UTC(),
		SchemaVersion:  "1.0",
		SourceService:  "jurisdiction-rules-svc",
		JurisdictionID: j.JurisdictionID,
		ActorID:        effectiveActor(j.CreatedByPrincipalID, j.UpdatedByPrincipalID),
		CorrelationID:  correlationID,
		Payload:        json.RawMessage(p.marshalJurisdictionDeactivated(j)),
	}
	data, _ := json.Marshal(env)
	return data
}

func (p *OutboxPublisher) buildRulePayload(r domain.JurisdictionRule, correlationID string) []byte {
	env := envelope{
		EventID:        "evt-" + uuid.New().String(),
		EventType:      EventRuleUpdated,
		EventVersion:   "1.0",
		EmittedAt:      time.Now().UTC(),
		SchemaVersion:  "1.0",
		SourceService:  "jurisdiction-rules-svc",
		JurisdictionID: r.JurisdictionID,
		ActorID:        effectiveActor(r.CreatedByPrincipalID, r.UpdatedByPrincipalID),
		CorrelationID:  correlationID,
		Payload:        json.RawMessage(p.marshalRule(r)),
	}
	data, _ := json.Marshal(env)
	return data
}

func (p *OutboxPublisher) buildDriftPayload(r domain.JurisdictionRule, e domain.DriftEvent, correlationID string) []byte {
	env := envelope{
		EventID:        "evt-" + uuid.New().String(),
		EventType:      EventLegalDriftDetected,
		EventVersion:   "1.0",
		EmittedAt:      time.Now().UTC(),
		SchemaVersion:  "1.0",
		SourceService:  "jurisdiction-rules-svc",
		JurisdictionID: r.JurisdictionID,
		ActorID:        e.RecordedByPrincipalID,
		CorrelationID:  correlationID,
		Payload:        json.RawMessage(p.marshalDrift(r, e)),
	}
	data, _ := json.Marshal(env)
	return data
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
