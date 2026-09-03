package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// DelegationProjector is the consumer's slice of the store: the two writes
// that maintain the delegation projection, and nothing else. The consumer
// cannot touch roles, assignments or decisions.
type DelegationProjector interface {
	ProjectDelegation(ctx context.Context, params domain.ProjectDelegationParams) (*domain.DelegatedAuthority, error)
	RevokeProjectedDelegation(ctx context.Context, sourceService, sourceDelegationID, tenantID string) (*domain.DelegatedAuthority, error)
}

// upstreamService is the source_service value written on every projected row.
//
// A constant rather than the event envelope's own source_service field:
// source_service is half of the projection's dedupe key, so taking it from the
// message would let a mislabelled producer write a second row for a delegation
// this service already holds — and /v1/authorize would union both. This
// consumer only subscribes to delegated-authority-svc's topic, so the value is
// known here and does not need to be trusted from the wire.
const upstreamService = "delegated-authority-svc"

// ConsumedEventTypes is what this consumer acts on. Everything else on the
// topic is acknowledged and skipped — a producer adding an event type must not
// stall this consumer's offset.
//
// ── WHY THESE THREE, AND WHY NOT THE OTHER FOUR ─────────────────────────────
//
// progress.md's v1 note listed four events as deliberately unconsumed —
// role.assigned, authority.delegated, employment.changed,
// entity.scope.updated — on the grounds that "none of these are actually
// published by any built service today [so] building a consumer with no real
// producer would be dead infrastructure."
//
// That was right about three of them and wrong about authority.delegated.
// delegated-authority-svc publishes it, plus authority.revoked and
// authority.expired, with a full payload (delegation_id, delegator, delegate,
// action_type, effective dates) on topic zoiko.delegated-authority.events. So
// the authoritative owner of the delegated-authority concept — Doc 03 §9.3
// names it as a separate service, which is tracker item 81's whole complaint —
// has been announcing every grant and revocation it makes, to nobody.
//
// role.assigned, employment.changed and entity.scope.updated still have no
// producer anywhere on the estate. A consumer for them would still be dead
// infrastructure, and they are still deliberately not consumed.
//
// ── WHAT THIS DOES TO TRACKER ITEM 81 ───────────────────────────────────────
//
// Item 81: this service owns a delegated_authorities table that duplicates
// delegated-authority-svc's ownership of the same concept, and "one is
// decorative." Consuming these events is what stops them being rivals:
// delegated-authority-svc stays authoritative for the LIFECYCLE (who may
// delegate, approval, expiry), and this table becomes the EVALUATION
// read-model /v1/authorize resolves against. Projected rows carry
// source_service / source_delegation_id and are only ever written by this
// consumer; locally-authored rows keep working unchanged, so the admin API is
// not broken for the console that uses it.
//
// It does not by itself close item 81 — a full consolidation would retire this
// service's own delegation write API, which is a cross-service decision and
// needs the console migrated first. It does mean neither store is decorative.
var ConsumedEventTypes = map[string]bool{
	"authority.delegated": true,
	"authority.revoked":   true,
	"authority.expired":   true,
}

// inbound is the read side of the platform event contract (Doc 03 §19). Only
// the fields this consumer acts on are declared, so a producer adding one does
// not break consumption.
type inbound struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	TenantID      string          `json:"tenant_id"`
	LegalEntityID string          `json:"legal_entity_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// delegationPayload is delegated-authority-svc's authority.* payload.
//
// EffectiveFrom/EffectiveTo are *time.Time: upstream sends effective_to as
// null for an open-ended delegation, and a non-pointer time.Time would decode
// that to the zero time — which, written to effective_to, would mark the
// delegation as having expired in year 1.
type delegationPayload struct {
	DelegationID  string     `json:"delegation_id"`
	LegalEntityID string     `json:"legal_entity_id"`
	Delegator     string     `json:"delegator_principal_id"`
	Delegate      string     `json:"delegate_principal_id"`
	ActionType    string     `json:"action_type"`
	EffectiveFrom *time.Time `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`
}

// dedupeTTL bounds how long an event id is remembered — long enough to cover
// a broker redelivery or a consumer restart's replay, short enough that the
// key space does not grow without limit.
const dedupeTTL = 24 * time.Hour

// Consumer projects delegated-authority-svc's authority.* events into this
// service's delegated_authorities table, so /v1/authorize's delegation layer
// resolves grants the authoritative service made.
//
// EVERY handler is IDEMPOTENT, twice over: on event_id before any side effect,
// and in the store itself, where ProjectDelegation is an UPSERT on the
// upstream delegation id and RevokeProjectedDelegation is a status transition
// that is already-satisfied on a redelivery. The dedupe is an optimisation;
// the store's idempotency is the guarantee. That matters because the dedupe is
// per process — see seen below.
type Consumer struct {
	log   *zap.Logger
	store DelegationProjector

	// seen is an in-process event-id set. Deliberately NOT the Redis-backed
	// deduper identity-context-svc uses: this service has no Redis dependency
	// and adding a Tier-0 one to an authorization service to deduplicate an
	// already-idempotent write is the wrong trade. Two replicas both applying
	// the same upsert converge on the same row.
	mu   sync.Mutex
	seen map[string]time.Time
}

func NewConsumer(log *zap.Logger, store DelegationProjector) *Consumer {
	return &Consumer{log: log, store: store, seen: make(map[string]time.Time)}
}

// claim reports whether this process has not already handled eventID, and
// records it. Expired ids are swept on the way through so the map stays
// bounded without a background goroutine.
func (c *Consumer) claim(eventID string) bool {
	if eventID == "" {
		// An event with no id cannot be deduplicated. Handle it — the store
		// writes are idempotent — rather than drop a real delegation because
		// its producer omitted a field.
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if len(c.seen) > 10_000 {
		for id, at := range c.seen {
			if now.Sub(at) > dedupeTTL {
				delete(c.seen, id)
			}
		}
	}
	if at, ok := c.seen[eventID]; ok && now.Sub(at) <= dedupeTTL {
		return false
	}
	c.seen[eventID] = now
	return true
}

// Run consumes until ctx is cancelled.
//
// A broker that is absent or unreachable must NOT stop the service. This is
// the platform's authorization engine — ~60 services call it on nearly every
// mutating request — and it does not need Kafka to answer. Read errors are
// logged and retried after a fixed delay rather than returned, so a broker
// that appears later is picked up without a restart, and every request keeps
// being evaluated against whatever the table already holds in the meantime.
// ctx cancellation and a closed reader are the only exits.
func (c *Consumer) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() {
		if err := reader.Close(); err != nil {
			c.log.Warn("kafka reader close failed", zap.Error(err))
		}
	}()

	c.log.Info("delegation projection consumer started", zap.String("upstream", upstreamService))

	for {
		msg, err := reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				c.log.Info("delegation projection consumer stopping")
				return
			}
			c.log.Warn("kafka read failed — retrying", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		c.Handle(ctx, msg.Value)
	}
}

// Handle applies one message. Exported so the dispatch logic is testable
// without a broker — the shape identity-context-svc's consumer uses.
//
// It never returns an error, and that is deliberate: this consumer commits its
// offset by reading the next message, so a returned error would either stall
// the partition on one bad event or be discarded anyway. A message that cannot
// be applied is logged with enough detail to replay it by hand, and the stream
// keeps moving. The alternative — one malformed event blocking every
// subsequent delegation — is worse for a projection whose staleness silently
// denies people access.
func (c *Consumer) Handle(ctx context.Context, raw []byte) {
	var env inbound
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Error("delegation event: undecodable envelope — skipped",
			zap.Error(err), zap.Int("bytes", len(raw)))
		return
	}

	if !ConsumedEventTypes[env.EventType] {
		return
	}

	// A tenantless authority event cannot be projected: since 000006
	// delegated_authorities.tenant_id is NOT NULL, and a row written under a
	// guessed tenant is a delegation that grants authority in the wrong place.
	// Refused loudly rather than defaulted.
	if strings.TrimSpace(env.TenantID) == "" {
		c.log.Error("delegation event: no tenant_id in envelope — cannot project",
			zap.String("event_id", env.EventID),
			zap.String("event_type", env.EventType),
			zap.String("correlation_id", env.CorrelationID))
		return
	}

	var payload delegationPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		c.log.Error("delegation event: undecodable payload — skipped",
			zap.String("event_id", env.EventID),
			zap.String("event_type", env.EventType),
			zap.Error(err))
		return
	}
	if payload.DelegationID == "" {
		c.log.Error("delegation event: payload names no delegation_id — cannot project or dedupe",
			zap.String("event_id", env.EventID),
			zap.String("event_type", env.EventType))
		return
	}

	if !c.claim(env.EventID) {
		c.log.Debug("delegation event: already handled by this process",
			zap.String("event_id", env.EventID))
		return
	}

	switch env.EventType {
	case "authority.delegated":
		c.applyDelegated(ctx, env, payload)
	case "authority.revoked", "authority.expired":
		c.applyEnded(ctx, env, payload)
	}
}

func (c *Consumer) applyDelegated(ctx context.Context, env inbound, payload delegationPayload) {
	// The entity comes from the payload, falling back to the envelope. Nil —
	// meaning tenant-wide — only when neither names one, which is a legitimate
	// upstream state and not an error.
	var legalEntityID *string
	if e := firstNonEmpty(payload.LegalEntityID, env.LegalEntityID); e != "" {
		legalEntityID = &e
	}

	// Upstream delegates ONE action per grant, so this is the ACTION_SUBSET
	// case that had no representation in this table before 000008 — before
	// that column existed, projecting this event would have conferred the
	// delegator's entire grant set instead of the one action upstream
	// authorised.
	var actions []string
	if payload.ActionType != "" {
		actions = []string{payload.ActionType}
	}

	effectiveFrom := time.Now().UTC()
	if payload.EffectiveFrom != nil {
		effectiveFrom = *payload.EffectiveFrom
	}

	d, err := c.store.ProjectDelegation(ctx, domain.ProjectDelegationParams{
		SourceService:        upstreamService,
		SourceDelegationID:   payload.DelegationID,
		TenantID:             env.TenantID,
		DelegatorPrincipalID: payload.Delegator,
		DelegatePrincipalID:  payload.Delegate,
		LegalEntityID:        legalEntityID,
		DelegatedActions:     actions,
		EffectiveFrom:        effectiveFrom,
		EffectiveTo:          payload.EffectiveTo,
	})
	if err != nil {
		c.log.Error("delegation event: projection failed — the delegation will not grant anything until this is replayed",
			zap.String("event_id", env.EventID),
			zap.String("delegation_id", payload.DelegationID),
			zap.String("correlation_id", env.CorrelationID),
			zap.Error(err))
		return
	}

	c.log.Info("delegation projected",
		zap.String("delegated_authority_id", d.DelegatedAuthorityID),
		zap.String("source_delegation_id", payload.DelegationID),
		zap.String("delegate_principal_id", payload.Delegate),
		zap.Strings("delegated_actions", actions),
		zap.String("correlation_id", env.CorrelationID))
}

func (c *Consumer) applyEnded(ctx context.Context, env inbound, payload delegationPayload) {
	d, err := c.store.RevokeProjectedDelegation(ctx, upstreamService, payload.DelegationID, env.TenantID)
	if err != nil {
		if errors.Is(err, domain.ErrDelegatedAuthorityNotFound) {
			// Nothing to end: either a redelivery of an event already applied,
			// or a delegation upstream created before this projection existed.
			// Both are benign, and the outcome the event asked for — this
			// delegation grants nothing — already holds.
			c.log.Debug("delegation event: no projected row to end",
				zap.String("event_type", env.EventType),
				zap.String("delegation_id", payload.DelegationID))
			return
		}
		c.log.Error("delegation event: ending the projection failed — the delegation may still grant access",
			zap.String("event_id", env.EventID),
			zap.String("event_type", env.EventType),
			zap.String("delegation_id", payload.DelegationID),
			zap.String("correlation_id", env.CorrelationID),
			zap.Error(err))
		return
	}

	c.log.Info("delegation projection ended",
		zap.String("delegated_authority_id", d.DelegatedAuthorityID),
		zap.String("source_delegation_id", payload.DelegationID),
		zap.String("event_type", env.EventType),
		zap.String("correlation_id", env.CorrelationID))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
