package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
)

// SessionRevoker is the consumer's slice of session storage.
//
// Every method takes a tenant, and the tenant comes from the inbound event's
// envelope rather than from anything the consumer infers. An event that names
// no tenant cannot be acted on — see requireTenant below.
type SessionRevoker interface {
	EvictAllForPrincipal(ctx context.Context, principalID, tenantID string, reason domain.InvalidationReason) (int, error)
	EvictAllForEntity(ctx context.Context, legalEntityID, tenantID string, reason domain.InvalidationReason) (int, error)
}

// RoleDirectory answers "who holds this role", so a role change can be turned
// into the set of sessions it invalidates.
type RoleDirectory interface {
	FindPrincipalIDsByRole(ctx context.Context, roleID, tenantID string, now time.Time) ([]string, error)
}

// RiskSignalWriter is the write half of the risk cache. Kept separate from the
// read interface the resolver holds, so the architectural invariant that
// Resolve() never writes a signal is expressed in the types.
type RiskSignalWriter interface {
	UpsertSignal(ctx context.Context, signal domain.RiskSignalCache) error
}

// dedupeTTL bounds how long an event id is remembered. Long enough to cover a
// broker redelivery or a consumer restart, short enough that the keyspace does
// not grow without limit.
const dedupeTTL = 24 * time.Hour

// Deduper reserves an event id, reporting whether this consumer won it.
//
// An interface rather than a *redis.Client because what the consumer needs is
// the claim, not the store — which keeps the dispatch logic testable without a
// broker or a Redis, and leaves room for a different backing store later.
type Deduper interface {
	Claim(ctx context.Context, eventID string) (bool, error)
}

// RedisDeduper claims event ids with SET NX EX.
//
// Distributed on purpose. The in-memory map this replaced was per process, so
// two replicas both acted on every event and a restart re-processed everything
// the broker redelivered — both visible as duplicate revocations.
type RedisDeduper struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewRedisDeduper(rdb *redis.Client) *RedisDeduper {
	return &RedisDeduper{rdb: rdb, ttl: dedupeTTL}
}

func (d *RedisDeduper) Claim(ctx context.Context, eventID string) (bool, error) {
	return d.rdb.SetNX(ctx, fmt.Sprintf("event:seen:%s", eventID), 1, d.ttl).Result()
}

// Consumer handles inbound domain events that require identity-context-svc to
// revoke sessions or update local state.
//
// ALL handlers are IDEMPOTENT — duplicate events are deduplicated on event_id
// before any side-effect (doctrine §3.7).
//
// Consumed events:
//
//	authority.revoked   → revoke ALL sessions for the delegate (hard revoke)
//	authority.expired   → same; an expired authority grants nothing
//	authority.delegated → no revocation; new delegations ADD access
//	role.updated        → revoke sessions of every principal holding the role
//	entity.updated      → revoke sessions scoped to the entity
//	session.risk.changed→ write the signal into the risk cache
//	tenant.created      → acknowledged, nothing cached to pre-warm
//
// This type used to exist but was never constructed anywhere: main.go wired the
// Kafka *writer* only. Every handler below was therefore unreachable, which is
// why revocation events changed nothing and the risk cache — whose only writer
// is HandleRiskSignalUpdate — was permanently empty, pinning every resolved
// session to STANDARD posture with signal source UNAVAILABLE.
type Consumer struct {
	log      *zap.Logger
	sessions SessionRevoker
	roles    RoleDirectory
	risk     RiskSignalWriter
	dedupe   Deduper
}

func NewConsumer(
	log *zap.Logger,
	sessions SessionRevoker,
	roles RoleDirectory,
	risk RiskSignalWriter,
	dedupe Deduper,
) *Consumer {
	return &Consumer{log: log, sessions: sessions, roles: roles, risk: risk, dedupe: dedupe}
}

// inbound is the read side of the platform event contract the publisher emits.
// Only the fields this service acts on are declared; unknown fields are ignored
// so a producer adding one does not break consumption.
type inbound struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// Run consumes until ctx is cancelled.
//
// A broker that is absent or unreachable must not stop the service: this is a
// Tier 0 dependency for authentication, and authentication does not need Kafka.
// Read errors are logged and retried with a fixed delay rather than returned,
// so a broker that appears later is picked up without a restart. ctx
// cancellation and a closed reader are the only exits.
func (c *Consumer) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() {
		if err := reader.Close(); err != nil {
			c.log.Warn("kafka reader close failed", zap.Error(err))
		}
	}()

	c.log.Info("event consumer started",
		zap.Strings("brokers", reader.Config().Brokers),
		zap.String("topic", reader.Config().Topic),
		zap.String("group_id", reader.Config().GroupID),
	)

	// consecutiveFailures drives the log level. The first failure in a run is
	// reported at WARN and the rest at DEBUG, resetting on any success.
	//
	// Logging every failure at DEBUG — which this did — means a broker that is
	// merely absent stays quiet, but so does a consumer that is genuinely
	// broken. Under zap.NewProduction() DEBUG is dropped entirely, so a reader
	// failing every five seconds forever produced no output at all and looked
	// exactly like a healthy idle consumer.
	consecutiveFailures := 0

	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, io.EOF) {
				c.log.Info("event consumer stopped")
				return
			}
			consecutiveFailures++
			if consecutiveFailures == 1 {
				c.log.Warn("kafka read failed — retrying until it recovers",
					zap.Error(err))
			} else {
				c.log.Debug("kafka read still failing",
					zap.Int("consecutive_failures", consecutiveFailures),
					zap.Error(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if consecutiveFailures > 0 {
			c.log.Info("kafka read recovered",
				zap.Int("after_failures", consecutiveFailures))
			consecutiveFailures = 0
		}

		c.log.Debug("event received",
			zap.Int64("offset", msg.Offset),
			zap.Int("bytes", len(msg.Value)))
		c.Handle(ctx, msg.Value)
	}
}

// Handle decodes one event and dispatches it. Exported so it can be driven
// directly in tests without a broker.
func (c *Consumer) Handle(ctx context.Context, raw []byte) {
	// A leading UTF-8 BOM is not valid JSON and encoding/json rejects it.
	// Producers on Windows emit one readily — PowerShell adds it to piped
	// output — and dropping an otherwise well-formed revocation over three
	// invisible bytes is not a trade worth making.
	raw = bytes.TrimPrefix(raw, []byte("\uFEFF"))

	var ev inbound
	if err := json.Unmarshal(raw, &ev); err != nil {
		// Not retryable — the same bytes will fail the same way. Logged and
		// dropped rather than blocking the partition behind it.
		c.log.Error("undecodable event — dropped", zap.Error(err))
		return
	}
	if ev.EventID == "" {
		c.log.Error("event has no event_id — dropped, cannot be deduplicated",
			zap.String("event_type", ev.EventType))
		return
	}

	switch ev.EventType {
	case "authority.revoked", "authority.expired":
		c.handleAuthorityEnded(ctx, ev)
	case "authority.delegated":
		c.handleAuthorityDelegated(ctx, ev)
	case "role.updated":
		c.handleRoleUpdated(ctx, ev)
	case "entity.updated":
		c.handleEntityUpdated(ctx, ev)
	case "session.risk.changed", "risk.signal.updated":
		c.handleRiskSignal(ctx, ev)
	case "tenant.created":
		// Acknowledged deliberately. There is no tenant cache to pre-warm —
		// tenant validity is read live from the registry on every resolve.
		c.log.Debug("tenant.created acknowledged", zap.String("tenant_id", ev.TenantID))
	default:
		c.log.Debug("unhandled event type", zap.String("event_type", ev.EventType))
	}
}

// ── Handlers ─────────────────────────────────────────────────────────────────

// handleAuthorityEnded revokes every session held by the delegate. This is the
// hardest case: an active session must stop carrying a delegation that no
// longer exists, and it must stop now rather than at envelope expiry.
func (c *Consumer) handleAuthorityEnded(ctx context.Context, ev inbound) {
	var p struct {
		DelegatePrincipalID string `json:"delegate_principal_id"`
		PrincipalID         string `json:"principal_id"`
	}
	_ = json.Unmarshal(ev.Payload, &p)

	principalID := firstNonEmpty(p.DelegatePrincipalID, p.PrincipalID, ev.ActorID)
	if principalID == "" {
		c.log.Error("authority event names no delegate — cannot revoke",
			zap.String("event_id", ev.EventID), zap.String("event_type", ev.EventType))
		return
	}
	if !c.requireTenant(ev) || !c.claim(ctx, ev.EventID) {
		return
	}

	n, err := c.sessions.EvictAllForPrincipal(ctx, principalID, ev.TenantID, domain.InvalidationReasonDelegationRevoked)
	if err != nil {
		c.log.Error("failed to revoke sessions for delegate",
			zap.String("principal_id", principalID),
			zap.String("correlation_id", ev.CorrelationID),
			zap.Error(err),
		)
		return
	}
	c.log.Warn("authority ended — sessions revoked",
		zap.String("event_type", ev.EventType),
		zap.String("delegate_principal_id", principalID),
		zap.Int("sessions_revoked", n),
		zap.String("correlation_id", ev.CorrelationID),
	)
}

// handleAuthorityDelegated records the grant and revokes nothing.
//
// A new delegation only ADDS authority. Revoking live sessions would log
// everyone out to hand them a privilege they did not ask for yet, and the next
// resolve picks the delegation up regardless because delegations are read from
// the store on every resolution rather than cached.
func (c *Consumer) handleAuthorityDelegated(ctx context.Context, ev inbound) {
	if !c.claim(ctx, ev.EventID) {
		return
	}
	c.log.Info("authority.delegated — no revocation required",
		zap.String("correlation_id", ev.CorrelationID))
}

// handleRoleUpdated revokes the sessions of every principal holding the role.
//
// A role's permission bundles are copied into the envelope at resolve time, so
// changing what a role grants leaves every live envelope asserting the old
// grant. Revoking forces the next request to re-resolve against current state.
func (c *Consumer) handleRoleUpdated(ctx context.Context, ev inbound) {
	var p struct {
		RoleID string `json:"role_id"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	if p.RoleID == "" {
		c.log.Error("role.updated names no role_id — cannot revoke",
			zap.String("event_id", ev.EventID))
		return
	}
	if !c.requireTenant(ev) || !c.claim(ctx, ev.EventID) {
		return
	}

	principals, err := c.roles.FindPrincipalIDsByRole(ctx, p.RoleID, ev.TenantID, time.Now().UTC())
	if err != nil {
		c.log.Error("failed to resolve principals for updated role",
			zap.String("role_id", p.RoleID), zap.Error(err))
		return
	}

	total := 0
	for _, principalID := range principals {
		n, err := c.sessions.EvictAllForPrincipal(ctx, principalID, ev.TenantID, domain.InvalidationReasonAdminRevoke)
		if err != nil {
			c.log.Error("failed to revoke sessions after role update",
				zap.String("principal_id", principalID),
				zap.String("role_id", p.RoleID),
				zap.Error(err),
			)
			continue
		}
		total += n
	}
	c.log.Info("role.updated — sessions revoked",
		zap.String("role_id", p.RoleID),
		zap.Int("principals_affected", len(principals)),
		zap.Int("sessions_revoked", total),
		zap.String("correlation_id", ev.CorrelationID),
	)
}

// handleEntityUpdated revokes sessions scoped to the changed legal entity.
func (c *Consumer) handleEntityUpdated(ctx context.Context, ev inbound) {
	var p struct {
		LegalEntityID string `json:"legal_entity_id"`
	}
	_ = json.Unmarshal(ev.Payload, &p)

	legalEntityID := firstNonEmpty(p.LegalEntityID, ev.LegalEntityID)
	if legalEntityID == "" {
		c.log.Error("entity.updated names no legal_entity_id — cannot revoke",
			zap.String("event_id", ev.EventID))
		return
	}
	if !c.requireTenant(ev) || !c.claim(ctx, ev.EventID) {
		return
	}

	n, err := c.sessions.EvictAllForEntity(ctx, legalEntityID, ev.TenantID, domain.InvalidationReasonAdminRevoke)
	if err != nil {
		c.log.Error("failed to revoke sessions for entity",
			zap.String("legal_entity_id", legalEntityID), zap.Error(err))
		return
	}
	c.log.Info("entity.updated — sessions revoked",
		zap.String("legal_entity_id", legalEntityID),
		zap.Int("sessions_revoked", n),
		zap.String("correlation_id", ev.CorrelationID),
	)
}

// handleRiskSignal writes a signal into the cache the resolver reads.
//
// This is the ONLY writer. Resolve() reads the cache and never calls out, so
// without this path every session resolves at STANDARD posture with source
// UNAVAILABLE and HIGH_RISK/BLOCKED are unreachable.
func (c *Consumer) handleRiskSignal(ctx context.Context, ev inbound) {
	var signal domain.RiskSignalCache
	if err := json.Unmarshal(ev.Payload, &signal); err != nil {
		c.log.Error("undecodable risk signal payload — dropped",
			zap.String("event_id", ev.EventID), zap.Error(err))
		return
	}
	if signal.PrincipalID == "" {
		c.log.Error("risk signal names no principal — dropped",
			zap.String("event_id", ev.EventID))
		return
	}
	if signal.TenantID == "" {
		signal.TenantID = ev.TenantID
	}
	if !c.claim(ctx, ev.EventID) {
		return
	}

	if err := c.risk.UpsertSignal(ctx, signal); err != nil {
		c.log.Error("failed to write risk signal",
			zap.String("principal_id", signal.PrincipalID), zap.Error(err))
		return
	}
	c.log.Info("risk signal cached",
		zap.String("principal_id", signal.PrincipalID),
		zap.Int("signal_value", signal.SignalValue),
		zap.String("signal_source", signal.SignalSource),
	)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// claim reserves an event id and reports whether this consumer won it.
//
// A dedupe-store failure returns TRUE — the event is processed. Every handler
// is idempotent, so acting twice costs a duplicate revocation; refusing to act
// because the dedupe store is down means a revoked authority keeps working.
func (c *Consumer) claim(ctx context.Context, eventID string) bool {
	ok, err := c.dedupe.Claim(ctx, eventID)
	if err != nil {
		c.log.Warn("dedupe store unavailable — processing event anyway",
			zap.String("event_id", eventID), zap.Error(err))
		return true
	}
	if !ok {
		c.log.Debug("duplicate event — skipped", zap.String("event_id", eventID))
	}
	return ok
}

// requireTenant refuses an event that names no tenant.
//
// Session storage is tenant-scoped, and there is no safe default: guessing
// would either revoke nothing (silently failing to enforce a revocation) or
// require an unscoped sweep across every tenant. Dropping loudly is the honest
// outcome, and names the producer's omission.
func (c *Consumer) requireTenant(ev inbound) bool {
	if ev.TenantID == "" {
		c.log.Error("event names no tenant_id — dropped, session storage is tenant-scoped",
			zap.String("event_id", ev.EventID),
			zap.String("event_type", ev.EventType),
		)
		return false
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
