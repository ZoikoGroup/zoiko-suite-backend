package whout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/eventing/inbox"
)

// ConsumerName is the stable inbox consumer name. Renaming it forgets every receipt,
// so replayed events would be fanned out again.
const ConsumerName = "notification.webhook-fanout"

// Processor applies one delivery exactly once in effect. *inbox.Inbox is the
// production implementation: it runs the handler and records the receipt in one
// transaction, so a crash can neither lose the fan-out nor repeat it (INT-03).
type Processor interface {
	Process(ctx context.Context, msg inbox.Message, fn inbox.Handler) (inbox.Result, error)
}

// Consumer turns platform events into webhook deliveries.
type Consumer struct {
	log   *zap.Logger
	store Store
	proc  Processor
	retry time.Duration
}

// NewConsumer builds the consumer.
func NewConsumer(log *zap.Logger, store Store, proc Processor) *Consumer {
	if log == nil {
		log = zap.NewNop()
	}
	return &Consumer{log: log, store: store, proc: proc, retry: 5 * time.Second}
}

// maxInProcessAttempts bounds the retry loop independently of the inbox's own budget,
// so a Processor that never quarantines cannot spin.
const maxInProcessAttempts = 20

// Handle fans one event out. It returns nil once the event has a disposition (fanned
// out, duplicate, nothing subscribed, quarantined or undecodable) and is safe to
// acknowledge, and an error only when the inbox could not be reached, in which case the
// caller must not acknowledge. Exported so tests can drive it without a broker.
func (c *Consumer) Handle(ctx context.Context, raw []byte) error {
	ev, err := ParseSource(raw)
	if err != nil {
		c.log.Warn("webhook fan-out: undecodable event dropped", zap.Error(err))
		return nil
	}
	if IsOwnEvent(ev.EventType) {
		return nil // never fan an event about a webhook back out to webhooks
	}
	// Events nobody subscribed to create no receipt: this consumer sees every event on
	// every source topic, and a row per event for a tenant with no webhooks would be pure
	// write amplification.
	match, err := c.store.HasMatchingActive(ctx, ev.TenantID, ev.EventType)
	if err != nil {
		return fmt.Errorf("check subscriptions: %w", err)
	}
	if !match {
		return nil
	}
	msg, err := inbox.Parse(raw)
	if err != nil {
		c.log.Warn("webhook fan-out: event lacks an identity for a receipt; dropped", zap.String("event_id", ev.EventID), zap.Error(err))
		return nil
	}
	for attempt := 1; attempt <= maxInProcessAttempts; attempt++ {
		res, err := c.proc.Process(ctx, msg, func(hctx context.Context, tx pgx.Tx, _ inbox.Receipt) (string, error) {
			out, err := c.store.FanOut(hctx, tx, ev)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("matched=%d created=%d duplicates=%d blocked=%d", out.Matched, out.Created, out.Duplicates, out.Blocked), nil
		})
		switch {
		case err == nil:
			return nil
		case res.FailureState == inbox.StateQuarantine:
			c.log.Error("webhook fan-out: event quarantined; it needs an approved repair and replay",
				zap.String("event_id", ev.EventID), zap.String("tenant_id", ev.TenantID), zap.Error(err))
			return nil
		}
		c.log.Warn("webhook fan-out: delivery failed; retrying", zap.String("event_id", ev.EventID), zap.Int("attempt", attempt), zap.Error(err))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.retry):
		}
	}
	return fmt.Errorf("webhook fan-out: gave up after %d attempts", maxInProcessAttempts)
}

// Run consumes until ctx is cancelled. A broker that is absent must not stop the
// service: webhook fan-out is a feed, not a precondition of anything else. The offset
// is committed only after a disposition, so a database outage delays the feed and never
// loses it.
func (c *Consumer) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() { _ = reader.Close() }()
	c.log.Info("webhook fan-out consumer started", zap.String("topic", reader.Config().Topic), zap.String("group_id", reader.Config().GroupID))
	failures := 0
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				return
			}
			failures++
			if failures == 1 {
				c.log.Warn("webhook fan-out: kafka read failed; retrying until it recovers", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		failures = 0
		for {
			if err := c.Handle(ctx, msg.Value); err == nil {
				break
			} else if ctx.Err() != nil {
				return
			} else {
				c.log.Warn("webhook fan-out: not acknowledged; retrying", zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
		if err := reader.CommitMessages(ctx, msg); err != nil && ctx.Err() == nil {
			c.log.Warn("webhook fan-out: offset commit failed; the event may be seen again and will be deduplicated", zap.Error(err))
		}
	}
}
