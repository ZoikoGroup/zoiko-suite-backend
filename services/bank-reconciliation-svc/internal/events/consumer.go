package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	svcmiddleware "zoiko.io/bank-reconciliation-svc/internal/middleware"
	"zoiko.io/eventing/inbox"
)

// paymentStatusConflictEventType is the event payment-status-svc publishes
// (domain.EventPaymentStatusConflictRaised in that service) when its own
// LinkStatementConfirmation detects a statement-vs-provider status
// mismatch. This is the one event type this consumer acts on.
const paymentStatusConflictEventType = "PAYMENT_STATUS_CONFLICT_RAISED"

// ConsumerName is the stable inbox consumer name. Renaming it forgets every
// receipt, so replayed events would be processed again.
const ConsumerName = "bank-reconciliation.evidence-conflicts"

// ConflictStore is the persistence slice the conflict consumer needs: enough
// to resolve the statement line's legal entity and to raise the cross-service
// conflict record inside the inbox's transaction.
type ConflictStore interface {
	GetStatementLine(ctx context.Context, statementLineID string) (*domain.StatementLine, error)
	RaiseEvidenceConflictInTx(ctx context.Context, tx pgx.Tx, tenantID string, req domain.RaiseEvidenceConflictRequest) (*domain.EvidenceConflict, bool, error)
}

// Processor applies one delivery exactly once in effect. *inbox.Inbox is the
// production implementation: it runs the handler and records the receipt in
// one transaction, so a crash can neither lose the effect nor repeat it
// (ZS-SVC-M-001 INT-03, INT-NP-013).
type Processor interface {
	Process(ctx context.Context, msg inbox.Message, fn inbox.Handler) (inbox.Result, error)
}

// ConflictConsumer bridges payment-status-svc's own conflict detection
// (statement-vs-provider status, raised via that service's
// LinkStatementConfirmation) into this service's evidence_conflicts —
// the cross-service join the doc's BNK-05 evidence_conflicts requirement
// actually calls for: bank-reconciliation-svc's match outcome disagreeing
// with payment-status-svc's provider-confirmed status for the same
// payment.
type ConflictConsumer struct {
	log        *zap.Logger
	store      ConflictStore
	proc       Processor
	retryDelay time.Duration
}

// NewConflictConsumer builds the consumer. proc is the shared inbox.
func NewConflictConsumer(log *zap.Logger, store ConflictStore, proc Processor) *ConflictConsumer {
	return &ConflictConsumer{log: log, store: store, proc: proc, retryDelay: time.Second}
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
//
// The Kafka offset is committed only AFTER a delivery reached a disposition
// (processed, duplicate, stale, held or quarantined). If the inbox itself
// cannot be reached the delivery is retried, not skipped: the offset stays
// uncommitted, so a database outage delays the feed instead of losing it.
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
		msg, err := reader.FetchMessage(ctx)
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

		for {
			err := c.Handle(ctx, msg.Value)
			if err == nil {
				break
			}
			c.log.Warn("evidence-conflict consumer: could not record the delivery — offset left uncommitted, retrying",
				zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			c.log.Warn("evidence-conflict consumer: offset commit failed — the delivery may be seen again and will be deduplicated", zap.Error(err))
		}
	}
}

// maxInProcessAttempts bounds the in-process retry loop independently of the
// inbox's own budget, so a Processor that never quarantines cannot spin.
const maxInProcessAttempts = 20

// Handle decodes and dispatches one event. It returns nil once the delivery
// has a disposition and is safe to acknowledge, and an error only when the
// inbox could not be reached (so the caller must not acknowledge). Exported so
// it can be driven directly in tests without a broker.
func (c *ConflictConsumer) Handle(ctx context.Context, raw []byte) error {
	var ev paymentStatusEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		c.log.Error("evidence-conflict consumer: undecodable event — dropped", zap.Error(err))
		return nil
	}
	if ev.EventType != paymentStatusConflictEventType {
		return nil
	}
	msg, err := inbox.Parse(raw)
	if err != nil {
		// No event id or tenant: there is nothing to key a receipt on, so the
		// delivery cannot be held in a tenant-scoped inbox.
		c.log.Error("evidence-conflict consumer: event lacks event_id or tenant_id — dropped, cannot be deduplicated",
			zap.String("event_id", ev.EventID), zap.Error(err))
		return nil
	}

	tenantCtx := svcmiddleware.WithTenant(ctx, msg.TenantID)

	for attempt := 1; attempt <= maxInProcessAttempts; attempt++ {
		// Read what the effect needs BEFORE the inbox transaction opens. That
		// transaction holds a pooled connection while it waits on the receipt
		// row; a handler that asked the pool for a second connection would
		// deadlock the whole pool once enough deliveries queued up behind one.
		prep := c.prepare(tenantCtx, ev)
		res, err := c.proc.Process(tenantCtx, msg, func(hctx context.Context, tx pgx.Tx, _ inbox.Receipt) (string, error) {
			return c.apply(hctx, tx, prep, ev, msg)
		})
		switch {
		case err == nil:
			c.logOutcome(msg, res)
			return nil
		case res.FailureState == inbox.StateQuarantine:
			c.log.Error("evidence-conflict consumer: event quarantined — needs an approved repair and replay",
				zap.String("event_id", msg.EventID), zap.Int("attempts", res.Attempt), zap.Error(err))
			return nil
		case res.FailureState == inbox.StateFailed:
			c.log.Warn("evidence-conflict consumer: attempt failed, retrying",
				zap.String("event_id", msg.EventID), zap.Int("attempt", res.Attempt), zap.Error(err))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.retryDelay):
			}
		default:
			// The inbox itself failed (database unavailable) or refused the
			// delivery (conflicting redelivery). Neither may be skipped.
			return err
		}
	}
	return fmt.Errorf("event %s still failing after %d attempts", msg.EventID, maxInProcessAttempts)
}

func (c *ConflictConsumer) logOutcome(msg inbox.Message, res inbox.Result) {
	switch res.Outcome {
	case inbox.Processed:
		c.log.Info("evidence conflict raised from payment-status-svc event",
			zap.String("event_id", msg.EventID), zap.String("conflict_id", res.ResultRef))
	case inbox.Duplicate:
		c.log.Debug("evidence-conflict consumer: duplicate event — skipped", zap.String("event_id", msg.EventID))
	case inbox.Stale:
		c.log.Info("evidence-conflict consumer: superseded event — skipped", zap.String("event_id", msg.EventID))
	case inbox.Held:
		c.log.Error("evidence-conflict consumer: event is quarantined and awaiting repair — not processed",
			zap.String("event_id", msg.EventID))
	}
}

// prepared is everything the effect needs that is read outside the inbox
// transaction. err is the failure to report from the effect: Poison when the
// event can never be processed, plain when a retry might succeed.
type prepared struct {
	payload conflictPayload
	line    *domain.StatementLine
	err     error
}

// prepare decodes the payload and resolves the statement line. The event
// carries no legal_entity_id — it is resolved from OUR OWN record of the
// statement line, which also confirms the line genuinely belongs to this
// tenant before a conflict is raised against it. A payload that can never be
// processed is Poison (quarantined at once); a failure that might clear — the
// line not imported yet, a transient read error — is retried and quarantined
// only when the budget is spent.
func (c *ConflictConsumer) prepare(ctx context.Context, ev paymentStatusEvent) prepared {
	var p prepared
	if err := json.Unmarshal(ev.Payload, &p.payload); err != nil {
		p.err = inbox.Poison(fmt.Errorf("undecodable payload: %w", err))
		return p
	}
	if p.payload.StatementLineID == "" || p.payload.PaymentID == "" {
		p.err = inbox.Poison(errors.New("event names no statement_line_id/payment_id"))
		return p
	}
	line, err := c.store.GetStatementLine(ctx, p.payload.StatementLineID)
	if err != nil {
		p.err = fmt.Errorf("load statement line: %w", err)
		return p
	}
	if line == nil {
		p.err = fmt.Errorf("statement line %s not found for this tenant", p.payload.StatementLineID)
		return p
	}
	p.line = line
	return p
}

// apply is the business effect, run inside the inbox transaction. It does no
// reads of its own beyond the transaction it is handed.
func (c *ConflictConsumer) apply(ctx context.Context, tx pgx.Tx, prep prepared, ev paymentStatusEvent, msg inbox.Message) (string, error) {
	if prep.err != nil {
		return "", prep.err
	}
	p, line := prep.payload, prep.line

	bankRecStatus := p.BankRecStatus
	if bankRecStatus == "" {
		bankRecStatus = string(line.Status)
	}
	reason := "bank-reconciliation status " + bankRecStatus + " disagrees with payment-status-svc's provider-confirmed status " + p.CurrentStatus
	if p.ReportedStatus != "" {
		reason += " (statement reported: " + p.ReportedStatus + ")"
	}

	conflict, _, err := c.store.RaiseEvidenceConflictInTx(ctx, tx, msg.TenantID, domain.RaiseEvidenceConflictRequest{
		TenantID:                msg.TenantID,
		LegalEntityID:           line.LegalEntityID,
		StatementLineID:         p.StatementLineID,
		PaymentID:               p.PaymentID,
		ProviderRequestID:       p.ProviderRequestID,
		BankRecStatus:           bankRecStatus,
		ProviderConfirmedStatus: p.CurrentStatus,
		ConflictReason:          reason,
		SourceEventID:           msg.EventID,
		CorrelationID:           ev.CorrelationID,
	})
	if err != nil {
		return "", fmt.Errorf("raise evidence conflict: %w", err)
	}
	return conflict.ConflictID, nil
}

// SetRetryDelay sets the pause between in-process attempts of one delivery.
func (c *ConflictConsumer) SetRetryDelay(d time.Duration) { c.retryDelay = d }
