package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// TenantCache is the cache this consumer keeps fresh.
type TenantCache interface {
	InvalidateTenant(tenantID string)
}

// CacheInvalidator drops this replica's cached reads when any replica — or
// an upstream service — changes what a decision depends on.
//
// WHY A SEPARATE CONSUMER. The lifecycle consumer runs in ONE consumer group
// shared by every replica, which is right for its projections (each write
// happens once) and wrong for invalidation: Kafka hands each message to one
// member of a group, so a revocation invalidated one replica's cache and every
// other replica kept serving the revoked grant until its TTL (ZS-IAM-001 §19
// "invalidation", A11/A12). This consumer runs in a group of its own per
// replica — a broadcast — and only invalidates; it projects nothing.
type CacheInvalidator struct {
	log   *zap.Logger
	cache TenantCache
}

func NewCacheInvalidator(log *zap.Logger, cache TenantCache) *CacheInvalidator {
	return &CacheInvalidator{log: log, cache: cache}
}

// invalidatingEvents are the events after which a cached read may be wrong:
// everything the lifecycle consumer treats as grant-graph, plus this
// service's own IAM events (000026), principal and entity standing, and the
// explicit cache invalidation command.
var invalidatingEvents = func() map[string]bool {
	m := map[string]bool{
		"iam.delegation.granted":          true,
		"iam.delegation.revoked":          true,
		"iam.sod_policy.published":        true,
		"iam.policy_set.published":        true,
		"authorization.cache.invalidated": true,
		"principal.status.changed":        true,
		"authority.delegated":             true,
		"authority.revoked":               true,
		"authority.expired":               true,
		"authority.suspended":             true,
		"authority.resumed":               true,
		"authority.extended":              true,
	}
	for k := range grantGraphEvents {
		m[k] = true
	}
	return m
}()

// Handle invalidates for one message. Never fails: an undecodable or
// uninteresting message changes nothing, and the cache TTL still bounds
// staleness if a message is somehow missed.
func (c *CacheInvalidator) Handle(raw []byte) {
	var env inbound
	if err := json.Unmarshal(raw, &env); err != nil || !invalidatingEvents[env.EventType] {
		return
	}
	c.cache.InvalidateTenant(strings.TrimSpace(env.TenantID))
	c.log.Debug("cache invalidated by event", zap.String("event_type", env.EventType), zap.String("tenant_id", env.TenantID))
}

// Run consumes until ctx is cancelled; a broker that is down is retried, never
// fatal, for the reason Consumer.Run gives.
func (c *CacheInvalidator) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() { _ = reader.Close() }()
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			c.log.Warn("cache invalidator: kafka fetch failed — retrying", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		c.Handle(msg.Value)
		if err := reader.CommitMessages(ctx, msg); err != nil {
			c.log.Warn("cache invalidator: commit failed", zap.Error(err))
		}
	}
}
