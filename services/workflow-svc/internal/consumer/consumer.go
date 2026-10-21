// Package consumer applies the events Doc 03 §8.4 lists as consumed by the
// workflow service (Governance audit workflow row 33, 21 Oct 2026): a change
// to policy or delegated authority, or an authorization denial, may mean a
// pending approver can no longer approve. Nothing consumed them, so a
// workflow kept routing to an approver who had lost the authority until that
// approver tried to act.
//
// Each event flags the affected PENDING instances (store
// MarkApproverRecheck); the timer pass re-authorizes their approvers and
// escalates an instance whose approver may no longer approve. Delivery is
// at-least-once: an event is claimed in workflow_consumed_events, applied,
// and only then committed; a failed apply releases the claim so the
// redelivery is applied.
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// Store is what the consumer needs.
type Store interface {
	RecordConsumedEvent(ctx context.Context, eventID, eventType, tenantID string) (bool, error)
	ForgetConsumedEvent(ctx context.Context, eventID string) error
	MarkApproverRecheck(ctx context.Context, tenantID, principalID string) (int, error)
	// ListTenantsWithPending names every tenant with a PENDING instance, for an
	// event that applies platform-wide (a global policy change).
	ListTenantsWithPending(ctx context.Context) ([]string, error)
}

// Default topics, with the producers' wire names.
var DefaultTopics = []string{"zoiko.policy.events", "zoiko.delegated-authority.events", "zoiko.authorization.events"}

// envelope is the platform event envelope, as far as this consumer reads it.
type envelope struct {
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	TenantID  string          `json:"tenant_id"`
	ActorID   string          `json:"actor_id"`
	Payload   json.RawMessage `json:"payload"`
}

type principals struct {
	PrincipalID          string `json:"principal_id"`
	DelegatorPrincipalID string `json:"delegator_principal_id"`
	DelegatePrincipalID  string `json:"delegate_principal_id"`
}

// Affected returns the principals whose pending approvals an event puts in
// question, ([""] = every pending instance of the tenant), and false when the
// event is not one this service acts on.
func Affected(eventType string, payload json.RawMessage) ([]string, bool) {
	var p principals
	_ = json.Unmarshal(payload, &p)
	switch eventType {
	case "policy.updated", "policy.version.activated", "policy.version.suspended", "policy.rule.retired":
		// A policy can change what any approver may do.
		return []string{""}, true
	case "authority.delegated", "authority.revoked", "authority.expired", "authority.suspended":
		var out []string
		for _, id := range []string{p.DelegatorPrincipalID, p.DelegatePrincipalID} {
			if id != "" {
				out = append(out, id)
			}
		}
		return out, len(out) > 0
	case "authorization.denied":
		if p.PrincipalID == "" {
			return nil, false
		}
		return []string{p.PrincipalID}, true
	}
	return nil, false
}

// Apply applies one message. It returns nil for a message it skips (another
// event type, a malformed envelope, a redelivery already applied), so the
// caller commits it; an error leaves it uncommitted for redelivery.
func Apply(ctx context.Context, st Store, value []byte, log *zap.Logger) error {
	var e envelope
	if err := json.Unmarshal(value, &e); err != nil || e.EventID == "" {
		log.Warn("workflow consumer: skipping a message without a decodable envelope", zap.Error(err))
		return nil
	}
	affected, ok := Affected(e.EventType, e.Payload)
	if !ok {
		return nil
	}
	// A tenantless event is a platform-wide change: a global policy applies to
	// every tenant, so every tenant with a pending approval is re-checked. It
	// used to be skipped, and a global policy change re-checked nobody. Other
	// tenantless events name a principal without a tenant to look it up in.
	global := e.TenantID == ""
	if global && (len(affected) != 1 || affected[0] != "") {
		log.Warn("workflow consumer: skipping a tenantless event", zap.String("event_id", e.EventID), zap.String("event_type", e.EventType))
		return nil
	}
	first, err := st.RecordConsumedEvent(ctx, e.EventID, e.EventType, e.TenantID)
	if err != nil {
		return err
	}
	if !first {
		return nil
	}
	tenants := []string{e.TenantID}
	if global {
		if tenants, err = st.ListTenantsWithPending(ctx); err != nil {
			if ferr := st.ForgetConsumedEvent(ctx, e.EventID); ferr != nil {
				log.Error("workflow consumer: releasing a failed claim failed — the event will not be re-applied",
					zap.String("event_id", e.EventID), zap.Error(ferr))
			}
			return err
		}
	}
	flagged := 0
	for _, tenant := range tenants {
		for _, principal := range affected {
			n, err := st.MarkApproverRecheck(ctx, tenant, principal)
			if err != nil {
				if ferr := st.ForgetConsumedEvent(ctx, e.EventID); ferr != nil {
					log.Error("workflow consumer: releasing a failed claim failed — the event will not be re-applied",
						zap.String("event_id", e.EventID), zap.Error(ferr))
				}
				return err
			}
			flagged += n
		}
	}
	if flagged > 0 {
		log.Info("workflow consumer: pending approvals flagged for re-check",
			zap.String("event_id", e.EventID), zap.String("event_type", e.EventType), zap.Int("instances", flagged))
	}
	return nil
}

// Consumer reads the topics, one reader each.
type Consumer struct {
	readers []*kafka.Reader
	store   Store
	log     *zap.Logger
}

// New builds a consumer in groupID over topics.
func New(brokers []string, groupID string, topics []string, st Store, log *zap.Logger) *Consumer {
	c := &Consumer{store: st, log: log}
	for _, topic := range topics {
		topic := topic
		c.readers = append(c.readers, kafka.NewReader(kafka.ReaderConfig{
			Brokers:  brokers,
			GroupID:  groupID,
			Topic:    topic,
			MinBytes: 1,
			MaxBytes: 10 << 20,
			// A group with no committed offset starts at the newest message:
			// re-checks are about current authority, not history.
			StartOffset: kafka.LastOffset,
			// A partition added later is joined, not silently ignored.
			WatchPartitionChanges: true,
			// Without an ErrorLogger kafka-go swallows a dead connection and
			// the consumer just looks idle.
			ErrorLogger: kafka.LoggerFunc(func(s string, args ...interface{}) {
				log.Error("workflow consumer: kafka error", zap.String("topic", topic), zap.String("detail", fmt.Sprintf(s, args...)))
			}),
		}))
	}
	return c
}

// Start runs every reader until ctx is cancelled.
func (c *Consumer) Start(ctx context.Context) {
	for _, r := range c.readers {
		go c.run(ctx, r)
	}
}

// Close closes the readers.
func (c *Consumer) Close() {
	for _, r := range c.readers {
		_ = r.Close()
	}
}

func (c *Consumer) run(ctx context.Context, r *kafka.Reader) {
	topic := r.Config().Topic
	c.log.Info("workflow consumer started", zap.String("topic", topic))
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return
			}
			c.log.Error("workflow consumer: fetch failed", zap.String("topic", topic), zap.Error(err))
			sleep(ctx, 2*time.Second)
			continue
		}
		// Retry the apply until it succeeds or the service stops: committing
		// past a failed apply would lose the re-check for good.
		for attempt := 1; ; attempt++ {
			if err := Apply(ctx, c.store, m.Value, c.log); err == nil {
				break
			} else {
				c.log.Warn("workflow consumer: apply failed, retrying", zap.String("topic", topic),
					zap.Int64("offset", m.Offset), zap.Int("attempt", attempt), zap.Error(err))
			}
			if !sleep(ctx, backoff(attempt)) {
				return
			}
		}
		if err := r.CommitMessages(ctx, m); err != nil && ctx.Err() == nil {
			c.log.Error("workflow consumer: commit failed (the message will be redelivered and skipped)",
				zap.String("topic", topic), zap.Error(err))
		}
	}
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<min(attempt, 6)) * time.Second
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
