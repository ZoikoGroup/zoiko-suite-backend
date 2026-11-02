// Package outbox drains the transactional outbox: Kafka lifecycle events and
// governance decision log records written in the same transaction as the
// recommendation change they describe.
//
// Delivery is at-least-once. Kafka consumers dedupe on event_id; the
// governance decision log dedupes on decision_id (sent as Idempotency-Key).
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/clients"
	"zoiko.io/decision-support-svc/internal/domain"
	"zoiko.io/decision-support-svc/internal/store"
)

const (
	pollInterval   = time.Second
	batchSize      = 20
	deliverTimeout = 5 * time.Second
)

// Store is the outbox persistence the relay needs.
type Store interface {
	DeliverPendingOutbox(ctx context.Context, limit int, deliver func(context.Context, domain.OutboxMessage) error) (int, error)
}

// KafkaWriter sends an already-built event.
type KafkaWriter interface {
	WriteRaw(ctx context.Context, key string, value []byte) error
}

// GovernanceRecorder appends a decision to the governance decision log.
type GovernanceRecorder interface {
	RecordDecision(ctx context.Context, rec clients.DecisionRecord) error
}

// Relay polls the outbox and delivers each message to its destination.
type Relay struct {
	store      Store
	kafka      KafkaWriter
	governance GovernanceRecorder
	log        *zap.Logger
}

// NewRelay builds a Relay.
func NewRelay(st Store, kafka KafkaWriter, governance GovernanceRecorder, log *zap.Logger) *Relay {
	return &Relay{store: st, kafka: kafka, governance: governance, log: log}
}

// Run polls until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.drainOnce(ctx)
		}
	}
}

func (r *Relay) drainOnce(ctx context.Context) {
	delivered, err := r.store.DeliverPendingOutbox(ctx, batchSize, r.Deliver)
	if err != nil && ctx.Err() == nil {
		r.log.Error("outbox relay pass failed", zap.Error(err))
		return
	}
	if delivered > 0 {
		r.log.Debug("outbox messages delivered", zap.Int("count", delivered))
	}
}

// Deliver sends one message to its destination. Exported for tests.
func (r *Relay) Deliver(ctx context.Context, message domain.OutboxMessage) error {
	ctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()

	switch message.Destination {
	case domain.OutboxDestinationKafka:
		return r.kafka.WriteRaw(ctx, message.MessageKey, message.Payload)

	case domain.OutboxDestinationGovernanceLog:
		var record clients.DecisionRecord
		if err := json.Unmarshal(message.Payload, &record); err != nil {
			return fmt.Errorf("%w: undecodable governance record: %v", store.ErrPermanentDelivery, err)
		}
		err := r.governance.RecordDecision(ctx, record)
		if errors.Is(err, clients.ErrDecisionRefusedPermanently) {
			return fmt.Errorf("%w: %v", store.ErrPermanentDelivery, err)
		}
		return err

	default:
		return fmt.Errorf("%w: unknown destination %q", store.ErrPermanentDelivery, message.Destination)
	}
}
