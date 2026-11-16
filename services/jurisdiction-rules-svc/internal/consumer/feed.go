package consumer

// Automatic legal drift detection from a regulatory feed (Doc 03 §8.2: consumes
// "external regulatory feed changes"; "Legal Drift Detection identifies when
// external regulatory updates create divergence between stored platform rules
// and applicable legal reality"; Governance audit J-03/J-04).
//
// Contract (asyncapi.yaml, regulatory.source.changed): an event in the platform
// envelope whose payload names the external_feed_reference that changed, the
// authority and a change summary. Every ACTIVE rule citing that reference moves
// to legal_drift_state UNDER_REVIEW with a drift event whose reason quotes the
// feed, and legal.drift.detected is published, in one transaction with the
// event's de-duplication row: a redelivery changes nothing. The drift never
// changes the rule's content or status; review and any new rule version stay a
// person's decision (ZS-JUR-001 s29 "source-change watch triggers controlled
// review; never auto-publishes rules").
//
// The producer is external-data-feed-svc, which does not publish this event yet
// (tracker D2-1.F05, ⛔). The consumer is off until KAFKA_REGULATORY_FEED_TOPIC
// names the topic.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// EventRegulatorySourceChanged is the consumed event type.
const EventRegulatorySourceChanged = "regulatory.source.changed"

// FeedActor is the principal the automatic drift is recorded as.
const FeedActor = "jurisdiction-rules-svc:regulatory-feed"

// FeedStore is what the consumer writes through (store.PgStore).
type FeedStore interface {
	WithTransaction(ctx context.Context, fn func(q store.Querier) error) error
	ClaimConsumedEvent(ctx context.Context, q store.Querier, eventID, eventType string) (bool, error)
	ActiveRuleIDsByFeedReference(ctx context.Context, q store.Querier, ref string) ([]string, error)
	RecordDriftWithQuerier(ctx context.Context, q store.Querier, p domain.RecordDriftParams) (*domain.JurisdictionRule, *domain.DriftEvent, bool, error)
}

// FeedConsumer applies regulatory source changes.
type FeedConsumer struct {
	log   *zap.Logger
	store FeedStore
	pub   events.Publisher
}

// NewFeedConsumer returns a consumer writing through st and publishing with pub.
func NewFeedConsumer(log *zap.Logger, st FeedStore, pub events.Publisher) *FeedConsumer {
	return &FeedConsumer{log: log, store: st, pub: pub}
}

type feedEnvelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

type sourceChangedPayload struct {
	ExternalFeedReference string     `json:"external_feed_reference"`
	Authority             string     `json:"authority"`
	ChangeSummary         string     `json:"change_summary"`
	ObservedAt            *time.Time `json:"observed_at"`
	SourceURL             string     `json:"source_url"`
}

// FeedResult says what one event did.
type FeedResult struct {
	Duplicate   bool
	RulesMarked int
}

// Handle applies one message. A malformed or irrelevant message is skipped (nil);
// a store failure is returned so the message is retried.
func (c *FeedConsumer) Handle(ctx context.Context, raw []byte) (FeedResult, error) {
	var env feedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Error("regulatory feed: undecodable envelope; skipped", zap.Error(err))
		return FeedResult{}, nil
	}
	if env.EventType != EventRegulatorySourceChanged {
		return FeedResult{}, nil
	}
	var p sourceChangedPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil || strings.TrimSpace(env.EventID) == "" || strings.TrimSpace(p.ExternalFeedReference) == "" {
		c.log.Error("regulatory feed: event without event_id or external_feed_reference; skipped", zap.String("event_id", env.EventID))
		return FeedResult{}, nil
	}
	reason := fmt.Sprintf("regulatory feed reported a change to %s", p.ExternalFeedReference)
	if a := strings.TrimSpace(p.Authority); a != "" {
		reason += " by " + a
	}
	if s := strings.TrimSpace(p.ChangeSummary); s != "" {
		reason += ": " + s
	}
	if u := strings.TrimSpace(p.SourceURL); u != "" {
		reason += " (" + u + ")"
	}
	reason += " [event " + env.EventID + "]"
	if len(reason) > 2000 {
		reason = reason[:2000]
	}

	var res FeedResult
	err := c.store.WithTransaction(ctx, func(q store.Querier) error {
		fresh, err := c.store.ClaimConsumedEvent(ctx, q, env.EventID, env.EventType)
		if err != nil {
			return err
		}
		if !fresh {
			res.Duplicate = true
			return nil
		}
		ids, err := c.store.ActiveRuleIDsByFeedReference(ctx, q, p.ExternalFeedReference)
		if err != nil {
			return err
		}
		bctx := events.WithBatch(ctx)
		for _, id := range ids {
			r := reason
			rule, ev, changed, err := c.store.RecordDriftWithQuerier(ctx, q, domain.RecordDriftParams{
				JurisdictionRuleID: id, ToState: "UNDER_REVIEW", Reason: &r,
				RecordedByPrincipalID: FeedActor, CorrelationID: env.CorrelationID})
			if err != nil {
				return err
			}
			if !changed {
				continue // already under review
			}
			res.RulesMarked++
			if err := c.pub.PublishLegalDriftDetected(bctx, *rule, *ev, env.CorrelationID); err != nil {
				return err
			}
		}
		return c.pub.FlushPending(bctx, q)
	})
	if err != nil {
		return FeedResult{}, err
	}
	return res, nil
}

// Run consumes until ctx ends. A message that fails to apply is retried until it
// applies (with backoff), and committed only after: it is never skipped.
func (c *FeedConsumer) Run(ctx context.Context, r *kafka.Reader) {
	defer func() { _ = r.Close() }()
	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Warn("regulatory feed: fetch failed; retrying", zap.Error(err))
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}
		backoff := time.Second
		for {
			res, err := c.Handle(ctx, m.Value)
			if err == nil {
				if res.RulesMarked > 0 {
					c.log.Info("regulatory feed: rules moved to UNDER_REVIEW", zap.Int("rules", res.RulesMarked), zap.Int64("offset", m.Offset))
				}
				break
			}
			c.log.Error("regulatory feed: apply failed; retrying the same message", zap.Int64("offset", m.Offset), zap.Error(err))
			if !sleep(ctx, backoff) {
				return // not committed: redelivered on restart
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
		if err := r.CommitMessages(ctx, m); err != nil && ctx.Err() == nil {
			c.log.Error("regulatory feed: commit failed (redelivered and de-duplicated)", zap.Error(err))
		}
	}
}
