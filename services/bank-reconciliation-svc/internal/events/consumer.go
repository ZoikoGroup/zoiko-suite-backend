package events

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
)

// paymentStatusConflictEventType is the event payment-status-svc publishes
// (domain.EventPaymentStatusConflictRaised in that service) when its own
// LinkStatementConfirmation detects a statement-vs-provider status
// mismatch. This is the one event type this consumer acts on.
const paymentStatusConflictEventType = "PAYMENT_STATUS_CONFLICT_RAISED"

// ConflictStore is the persistence slice the conflict consumer needs:
// enough to resolve the statement line's legal entity, raise the
// cross-service conflict record, and dedupe on the source Kafka event.
type ConflictStore interface {
	GetStatementLine(ctx context.Context, statementLineID string) (*domain.StatementLine, error)
	RaiseEvidenceConflict(ctx context.Context, tenantID string, req domain.RaiseEvidenceConflictRequest) (*domain.EvidenceConflict, bool, error)
	IsEventProcessed(ctx context.Context, tenantID, eventID string) (bool, error)
	MarkEventProcessed(ctx context.Context, tenantID, eventID string) error
}

// ConflictConsumer bridges payment-status-svc's own conflict detection
// (statement-vs-provider status, raised via that service's
// LinkStatementConfirmation) into this service's evidence_conflicts —
// the cross-service join the doc's BNK-05 evidence_conflicts requirement
// actually calls for: bank-reconciliation-svc's match outcome disagreeing
// with payment-status-svc's provider-confirmed status for the same
// payment. Until this consumer existed, POST /v1/evidence-conflicts was
// reachable only by a manual caller — payment-status-svc's own published
// event had nothing subscribed to it.
type ConflictConsumer struct {
	log   *zap.Logger
	store ConflictStore
}

func NewConflictConsumer(log *zap.Logger, store ConflictStore) *ConflictConsumer {
	return &ConflictConsumer{log: log, store: store}
}

// paymentStatusEvent mirrors payment-status-svc's own events.Event shape
// (see that service's internal/events/publisher.go) — only the fields
// this consumer acts on are declared, so a field payment-status-svc adds
// later doesn't break decoding here.
type paymentStatusEvent struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// conflictPayload mirrors the payload payment-status-svc's
// LinkStatementConfirmation attaches to PAYMENT_STATUS_CONFLICT_RAISED.
type conflictPayload struct {
	PaymentID          string `json:"payment_id"`
	StatementLineID    string `json:"statement_line_id"`
	ProviderRequestID  string `json:"provider_request_id"`
	BankRecStatus      string `json:"bank_rec_status"`
	ReportedStatus     string `json:"reported_status"`
	CurrentStatus      string `json:"current_status"`
	StatementReference string `json:"statement_reference"`
}

// Run consumes until ctx is cancelled. A broker that is absent or
// unreachable must not stop the service — reconciliation matching does
// not depend on this cross-service conflict feed, only the feed itself
// does. Read errors are logged and retried rather than fatal.
func (c *ConflictConsumer) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() {
		if err := reader.Close(); err != nil {
			c.log.Warn("evidence-conflict consumer: kafka reader close failed", zap.Error(err))
		}
	}()

	c.log.Info("evidence-conflict consumer started",
		zap.Strings("brokers", reader.Config().Brokers),
		zap.String("topic", reader.Config().Topic),
		zap.String("group_id", reader.Config().GroupID),
	)

	consecutiveFailures := 0
	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				c.log.Info("evidence-conflict consumer stopped")
				return
			}
			consecutiveFailures++
			if consecutiveFailures == 1 {
				c.log.Warn("evidence-conflict consumer: kafka read failed — retrying until it recovers", zap.Error(err))
			} else {
				c.log.Debug("evidence-conflict consumer: kafka read still failing",
					zap.Int("consecutive_failures", consecutiveFailures), zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if consecutiveFailures > 0 {
			c.log.Info("evidence-conflict consumer: kafka read recovered", zap.Int("after_failures", consecutiveFailures))
			consecutiveFailures = 0
		}
		c.Handle(ctx, msg.Value)
	}
}

// Handle decodes and dispatches one event. Exported so it can be driven
// directly in tests without a broker.
func (c *ConflictConsumer) Handle(ctx context.Context, raw []byte) {
	var ev paymentStatusEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		c.log.Error("evidence-conflict consumer: undecodable event — dropped", zap.Error(err))
		return
	}
	if ev.EventType != paymentStatusConflictEventType {
		return
	}
	if ev.EventID == "" {
		c.log.Error("evidence-conflict consumer: event has no event_id — dropped, cannot be deduplicated")
		return
	}
	if ev.TenantID == "" {
		c.log.Error("evidence-conflict consumer: event names no tenant_id — dropped",
			zap.String("event_id", ev.EventID))
		return
	}

	tenantCtx := svcmiddleware.WithTenant(ctx, ev.TenantID)

	processed, err := c.store.IsEventProcessed(tenantCtx, ev.TenantID, ev.EventID)
	if err != nil {
		c.log.Error("evidence-conflict consumer: inbox check failed — processing anyway rather than losing the conflict",
			zap.String("event_id", ev.EventID), zap.Error(err))
	} else if processed {
		c.log.Debug("evidence-conflict consumer: duplicate event — skipped", zap.String("event_id", ev.EventID))
		return
	}

	var p conflictPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		c.log.Error("evidence-conflict consumer: undecodable payload — dropped",
			zap.String("event_id", ev.EventID), zap.Error(err))
		return
	}
	if p.StatementLineID == "" || p.PaymentID == "" {
		c.log.Error("evidence-conflict consumer: event names no statement_line_id/payment_id — dropped",
			zap.String("event_id", ev.EventID))
		return
	}

	// The event carries no legal_entity_id — resolved from OUR OWN record
	// of the statement line, which also confirms the line genuinely
	// belongs to this tenant before a conflict is raised against it.
	line, err := c.store.GetStatementLine(tenantCtx, p.StatementLineID)
	if err != nil {
		c.log.Error("evidence-conflict consumer: failed to load statement line — dropped",
			zap.String("event_id", ev.EventID), zap.String("statement_line_id", p.StatementLineID), zap.Error(err))
		return
	}
	if line == nil {
		c.log.Error("evidence-conflict consumer: statement line not found for this tenant — dropped",
			zap.String("event_id", ev.EventID), zap.String("statement_line_id", p.StatementLineID))
		return
	}

	bankRecStatus := p.BankRecStatus
	if bankRecStatus == "" {
		bankRecStatus = string(line.Status)
	}
	reason := "bank-reconciliation status " + bankRecStatus + " disagrees with payment-status-svc's provider-confirmed status " + p.CurrentStatus
	if p.ReportedStatus != "" {
		reason += " (statement reported: " + p.ReportedStatus + ")"
	}

	_, _, err = c.store.RaiseEvidenceConflict(tenantCtx, ev.TenantID, domain.RaiseEvidenceConflictRequest{
		TenantID:                ev.TenantID,
		LegalEntityID:           line.LegalEntityID,
		StatementLineID:         p.StatementLineID,
		PaymentID:               p.PaymentID,
		ProviderRequestID:       p.ProviderRequestID,
		BankRecStatus:           bankRecStatus,
		ProviderConfirmedStatus: p.CurrentStatus,
		ConflictReason:          reason,
		SourceEventID:           ev.EventID,
		CorrelationID:           ev.CorrelationID,
	})
	if err != nil {
		c.log.Error("evidence-conflict consumer: failed to raise evidence conflict",
			zap.String("event_id", ev.EventID), zap.String("statement_line_id", p.StatementLineID), zap.Error(err))
		return
	}

	if err := c.store.MarkEventProcessed(tenantCtx, ev.TenantID, ev.EventID); err != nil {
		c.log.Warn("evidence-conflict consumer: failed to record inbox entry — a replay may reprocess this event",
			zap.String("event_id", ev.EventID), zap.Error(err))
	}

	c.log.Info("evidence conflict raised from payment-status-svc event",
		zap.String("event_id", ev.EventID),
		zap.String("statement_line_id", p.StatementLineID),
		zap.String("payment_id", p.PaymentID),
	)
}
