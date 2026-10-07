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

// LifecycleProjector is the lifecycle consumer's slice of the store: one write
// and one cache hook, and nothing else. It cannot touch roles, bundles,
// assignments, delegations or decisions — this consumer reacts to other
// services' announcements, and the only table it may author is the projection
// it owns.
type LifecycleProjector interface {
	ProjectPrincipalStatus(ctx context.Context, params domain.ProjectPrincipalStatusParams) (*domain.PrincipalStatusProjection, error)

	// InvalidateGrantSourcesForTenant drops the cached grant and delegation
	// reads for a tenant — empty means every tenant. See
	// cache.Store.InvalidateGrantSourcesForTenant for why a consumer is the
	// right caller for it.
	InvalidateGrantSourcesForTenant(tenantID string)

	// ProjectEntityStatus records an entity's standing, which /v1/authorize
	// evaluates as a negative control (SUSPENDED / DISSOLVED deny).
	ProjectEntityStatus(ctx context.Context, params domain.ProjectEntityStatusParams) error
}

// LifecycleConsumer consumes the three §8.3 event concepts that were recorded
// across four passes as having no producer.
//
// ── THE CORRECTION THIS FILE IS ────────────────────────────────────────────
//
// Doc 03 §8.3 lists four consumed events: role.assigned, authority.delegated,
// employment.changed, entity.scope.updated. authority.delegated is consumed by
// Consumer, in this package. The other three were recorded — in progress.md,
// in known-gaps.md, in the tracker, and in this package's own
// ConsumedEventTypes comment — as having "no producer anywhere on the estate",
// on the strength of a grep. The grep was for the SPEC's names.
//
// The platform publishes all three concepts. Under different names, on three
// different topics, with payloads that are not interchangeable:
//
//	SPEC NAME              REAL EVENTS                       TOPIC
//	role.assigned          role.created, role.updated,       zoiko.access-control.events
//	                       permission.bundle.updated
//	                         — access-control-svc, which holds role
//	                           DEFINITIONS and forwards each write to this
//	                           service's admin API. The event is how the other
//	                           replicas find out.
//
//	employment.changed     principal.status.changed          zoiko.identity.events
//	                         — identity-context-svc. The only one of the
//	                           candidates keyed on principal_id. See below on
//	                           employee.terminated, which is not usable.
//
//	entity.scope.updated   entity.status.changed,            zoiko.entity.events
//	                       entity.hierarchy.changed,
//	                       entity.jurisdiction.changed
//	                         — tenant-entity-registry-svc. All three change
//	                           which entities a grant resolves in.
//
// So the events were there the whole time. What was missing was reading the
// producers rather than searching for the names the spec happened to use — the
// same shape as tracker 82i, where a pass measured a symptom correctly and then
// reasoned about the remedy on paper.
//
// ── WHY employee.terminated IS NOT CONSUMED ─────────────────────────────────
//
// It is the most obvious candidate for employment.changed and it is unusable,
// for a reason worth recording rather than leaving as a silent omission:
// employee-master-svc and offboarding-severance-svc both publish it, and its
// payload names an employee_id. There is NO employee-to-principal mapping
// anywhere on this estate — employee-master-svc's schema carries no
// principal_id or user_id column at all. So the event says somebody left and
// this service cannot tell whose authority that is about.
//
// Consuming it on a guessed join would end the wrong principal's grants,
// silently, on the platform's authorization plane. That is strictly worse than
// not consuming it, and the gap is an identity-mapping gap rather than a
// missing consumer. Recorded in known-gaps.md as the former.
//
// ── WHAT EACH GROUP DOES, AND WHY THEY ARE NOT THE SAME ─────────────────────
//
// principal.status.changed is the only one that WRITES. It projects into
// principal_status_projection, which /v1/authorize reads as layer 0: a
// principal identity-context-svc has suspended is denied every action. That
// closes a hole session eviction does not — identity-context-svc evicts
// sessions, but this endpoint is called east-west on envelopes resolved before
// the suspension, and by queued work carrying a principal and no session.
//
// The other two groups only INVALIDATE. There is nothing to project: this
// service already holds the authoritative copy of what a role grants (its own
// permission_bundles, written through its own admin API by access-control-svc),
// and it holds no entity registry at all. What it does not otherwise have is
// any way to learn that ANOTHER REPLICA took that write. The cache is
// in-process, so before this consumer the staleness bound on every replica but
// the one that served an admin write was the TTL. Now it is broker latency.
//
// Deliberately NOT a projection of role assignments. Tracker 79 records that
// role assignment is duplicated between this service and identity-context-svc
// and that the delegation projection trick does not transfer: identity's
// RoleDirectory answers "who holds this role" to decide which SESSIONS a change
// invalidates, which is a different question from "what may this principal do",
// and neither service is the obvious authority. Inventing an upstream id to
// dedupe on — the mechanism migration 000008 gave delegations — would settle
// that cross-service ownership question by writing code, which is how a
// duplicate write model gets created rather than resolved.
type LifecycleConsumer struct {
	log   *zap.Logger
	store LifecycleProjector

	// seen is the same in-process event-id set Consumer carries, for the same
	// reason: the writes are idempotent (an upsert keyed on
	// (tenant, principal)) and the invalidations are trivially so, and adding
	// a Redis dependency to a Tier-0 authorization service to deduplicate
	// idempotent work is the wrong trade.
	mu   sync.Mutex
	seen map[string]time.Time
}

// LifecycleSourceService is the source_service written on every projected
// principal-status row.
//
// A constant, not the envelope's own source_service field, for the same reason
// Consumer's upstreamService is: it is provenance, and taking provenance from
// the wire lets a mislabelled producer claim to be identity-context-svc. This
// consumer subscribes to identity-context-svc's topic, so the value is known
// here.
const LifecycleSourceService = "identity-context-svc"

// The event types this consumer acts on, grouped by what each group does.
//
// Split into two maps rather than one switch because the two groups have
// genuinely different requirements: the projecting group REFUSES an event with
// no tenant (a status row with a guessed tenant would deny the wrong
// principal), while the invalidating group ACCEPTS one and invalidates across
// every tenant instead (over-invalidating costs cache hits, under-invalidating
// costs correctness — see handleInvalidation).
var (
	// principalStatusEvents is the projecting group.
	principalStatusEvents = map[string]bool{
		"principal.status.changed": true,
	}

	// grantGraphEvents is the invalidating group: every event that changes
	// what a grant resolves to without this service being the one that wrote
	// it.
	grantGraphEvents = map[string]bool{
		// access-control-svc — role definitions and permission bundles.
		// permission.bundle.updated is the important one: permitted_actions
		// changing changes what every holder of that role may do.
		"role.created":              true,
		"role.updated":              true,
		"permission.bundle.updated": true,
		// access-control-svc's governance surface (Authorization Standard
		// §9, §23). Assignments it provisions are written through this
		// service's admin API, which invalidates the writing replica; these
		// reach every OTHER replica, whose cached grant sources would
		// otherwise keep a revoked assignment live until the TTL.
		"iam.role.published":     true,
		"iam.assignment.granted": true,
		"iam.assignment.revoked": true,

		// tenant-entity-registry-svc — the entities grants are scoped to.
		// entity.status.changed matters because a DORMANT, SUSPENDED or
		// DISSOLVED entity is one nobody should be acting in;
		// entity.hierarchy.changed and entity.jurisdiction.changed matter
		// because they move an entity relative to the scopes and
		// jurisdiction-scoped SoD rules that apply to it.
		"entity.status.changed":       true,
		"entity.hierarchy.changed":    true,
		"entity.jurisdiction.changed": true,
		"entity.updated":              true,
	}
)

// LifecycleConsumedEventTypes is the union, for tests and for anybody asking
// what this consumer subscribes to without reading two maps.
func LifecycleConsumedEventTypes() map[string]bool {
	out := make(map[string]bool, len(principalStatusEvents)+len(grantGraphEvents))
	for k := range principalStatusEvents {
		out[k] = true
	}
	for k := range grantGraphEvents {
		out[k] = true
	}
	return out
}

// principalStatusPayload is identity-context-svc's principal.status.changed
// payload. Only the fields acted on are declared, so a producer adding one
// does not break consumption.
//
// StatusChangedAt is a pointer and is not in the current payload at all —
// identity-context-svc sends principal_id, tenant_id, new_status, actor and
// correlation_id. It is declared because ProjectPrincipalStatus uses it to
// refuse a STALE event, and nil means "upstream did not say", which that
// method handles by applying the write. Declaring it now means a producer that
// starts sending it gets replay protection with no change here.
type principalStatusPayload struct {
	PrincipalID     string     `json:"principal_id"`
	TenantID        string     `json:"tenant_id"`
	NewStatus       string     `json:"new_status"`
	StatusChangedAt *time.Time `json:"status_changed_at"`
}

func NewLifecycleConsumer(log *zap.Logger, store LifecycleProjector) *LifecycleConsumer {
	return &LifecycleConsumer{log: log, store: store, seen: make(map[string]time.Time)}
}

// claim reports whether this process has not already handled eventID.
// Identical in shape to Consumer.claim, and separate because the two consumers
// must not share a dedupe set: they read different topics whose producers mint
// their own event ids, and a collision between them would silently drop one.
func (c *LifecycleConsumer) claim(eventID string) bool {
	if eventID == "" {
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

// release forgets eventID, so a message whose apply failed is applied when it
// is retried rather than skipped as already handled.
func (c *LifecycleConsumer) release(eventID string) {
	if eventID == "" {
		return
	}
	c.mu.Lock()
	delete(c.seen, eventID)
	c.mu.Unlock()
}

// Run consumes until ctx is cancelled.
//
// A broker that is absent or unreachable must NOT stop the service, for the
// same reason Consumer.Run says so: this is the platform's authorization engine
// and it answers whether or not Kafka is up. An unreachable broker means layer
// 0 goes stale and cross-replica invalidation stops — the TTL becomes the bound
// again, which is where this service was before this consumer existed — and
// both are logged. Neither stops requests being evaluated.
func (c *LifecycleConsumer) Run(ctx context.Context, reader *kafka.Reader) {
	defer func() {
		if err := reader.Close(); err != nil {
			c.log.Warn("kafka reader close failed", zap.Error(err))
		}
	}()

	c.log.Info("lifecycle consumer started",
		zap.Int("event_types", len(LifecycleConsumedEventTypes())))

	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				c.log.Info("lifecycle consumer stopping")
				return
			}
			c.log.Warn("kafka fetch failed — retrying", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if !applyUntilDone(ctx, c.log, string(msg.Key), func() error { return c.Handle(ctx, msg.Value) }) {
			c.log.Info("lifecycle consumer stopping with an unapplied message — not committed, so it is redelivered")
			return
		}
		if err := reader.CommitMessages(ctx, msg); err != nil {
			c.log.Error("failed to commit offset", zap.Error(err))
		}
	}
}

// Handle applies one message. Exported so dispatch is testable without a
// broker.
//
// Returns an error only for critical failures (DB unavailable) that should
// cause the offset to NOT be committed, allowing a retry. Non-critical
// failures (unknown event type, malformed payload, missing tenant for status)
// are logged and return nil so the offset is committed and the partition keeps
// moving.
func (c *LifecycleConsumer) Handle(ctx context.Context, raw []byte) error {
	var env inbound
	if err := json.Unmarshal(raw, &env); err != nil {
		c.log.Error("lifecycle event: undecodable envelope — skipped",
			zap.Error(err), zap.Int("bytes", len(raw)))
		return nil // Malformed message, commit and move on
	}

	isStatus := principalStatusEvents[env.EventType]
	isGrantGraph := grantGraphEvents[env.EventType]
	if !isStatus && !isGrantGraph {
		// Three topics are subscribed and each carries events this service has
		// no interest in. Silently skipped, not logged: at this volume a line
		// per uninteresting event is how the log stops being readable.
		return nil
	}

	if !c.claim(env.EventID) {
		c.log.Debug("lifecycle event: already handled by this process",
			zap.String("event_id", env.EventID),
			zap.String("event_type", env.EventType))
		return nil // Already processed, commit and move on
	}

	if isStatus {
		err := c.handlePrincipalStatus(ctx, env)
		if err != nil {
			// Released so the retry in Run applies it rather than skipping it.
			c.release(env.EventID)
		}
		return err
	}
	if env.EventType == "entity.status.changed" {
		if err := c.handleEntityStatus(ctx, env); err != nil {
			c.release(env.EventID)
			return err
		}
	}
	c.handleInvalidation(env)
	return nil
}

// entityStatusPayload is tenant-entity-registry-svc's entity.status.changed.
type entityStatusPayload struct {
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	NewStatus     string `json:"new_status"`
}

// handleEntityStatus projects an entity's standing. Until this existed the
// event only invalidated the cache, so a DISSOLVED entity stayed one anybody
// holding a grant could act in. A failed write is returned so Run retries it:
// losing a dissolution would leave the entity operational indefinitely.
func (c *LifecycleConsumer) handleEntityStatus(ctx context.Context, env inbound) error {
	var p entityStatusPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		c.log.Error("entity status event: undecodable payload — skipped", zap.String("event_id", env.EventID), zap.Error(err))
		return nil
	}
	tenantID := strings.TrimSpace(p.TenantID)
	if tenantID == "" {
		tenantID = strings.TrimSpace(env.TenantID)
	}
	entityID := strings.TrimSpace(p.LegalEntityID)
	if entityID == "" {
		entityID = strings.TrimSpace(env.LegalEntityID)
	}
	status := strings.ToUpper(strings.TrimSpace(p.NewStatus))
	if tenantID == "" || entityID == "" || status == "" {
		c.log.Error("entity status event: missing tenant, entity or status — skipped", zap.String("event_id", env.EventID))
		return nil
	}
	at := time.Now().UTC()
	if env.EffectiveAt != nil && !env.EffectiveAt.IsZero() {
		at = env.EffectiveAt.UTC()
	}
	if err := c.store.ProjectEntityStatus(ctx, domain.ProjectEntityStatusParams{
		LegalEntityID: entityID, TenantID: tenantID, Status: status, StatusChangedAt: at,
	}); err != nil {
		c.log.Error("entity status event: projection failed — the entity's standing is unchanged until it is replayed",
			zap.String("event_id", env.EventID), zap.String("legal_entity_id", entityID), zap.Error(err))
		return err
	}
	c.log.Info("entity status projected", zap.String("legal_entity_id", entityID), zap.String("status", status))
	return nil
}

func (c *LifecycleConsumer) handlePrincipalStatus(ctx context.Context, env inbound) error {
	var payload principalStatusPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		c.log.Error("principal status event: undecodable payload — skipped",
			zap.String("event_id", env.EventID), zap.Error(err))
		return nil // Malformed payload, commit and move on
	}

	principalID := strings.TrimSpace(payload.PrincipalID)
	if principalID == "" {
		c.log.Error("principal status event: payload names no principal_id — cannot project",
			zap.String("event_id", env.EventID),
			zap.String("correlation_id", env.CorrelationID))
		return nil // Missing principal_id, commit and move on
	}

	status := strings.ToUpper(strings.TrimSpace(payload.NewStatus))
	if status == "" {
		// An empty status would be stored, read as not-ACTIVE by layer 0, and
		// deny that principal everything — from a malformed event. Refused
		// loudly instead. This is the one place in this consumer where the
		// fail-closed direction is the WRONG one, because the closure would be
		// caused by the defect rather than by a decision.
		c.log.Error("principal status event: payload names no new_status — refusing to project a status that would deny everything",
			zap.String("event_id", env.EventID),
			zap.String("principal_id", principalID),
			zap.String("correlation_id", env.CorrelationID))
		return nil // Empty status, commit and move on
	}

	// Payload first, envelope second: identity-context-svc puts the tenant in
	// both, and the payload is the one that names the tenant the STATUS is
	// about.
	tenantID := strings.TrimSpace(firstNonEmpty(payload.TenantID, env.TenantID))
	if tenantID == "" {
		// principal_status_projection.tenant_id is NOT NULL and every read
		// carries a tenant predicate, so a guessed tenant would either write a
		// row no policy matches or suspend a principal in the wrong tenant.
		c.log.Error("principal status event: no tenant_id in payload or envelope — cannot project",
			zap.String("event_id", env.EventID),
			zap.String("principal_id", principalID),
			zap.String("correlation_id", env.CorrelationID))
		return nil // Missing tenant, commit and move on
	}

	projected, err := c.store.ProjectPrincipalStatus(ctx, domain.ProjectPrincipalStatusParams{
		PrincipalID:     principalID,
		TenantID:        tenantID,
		Status:          status,
		SourceService:   LifecycleSourceService,
		StatusChangedAt: payload.StatusChangedAt,
	})
	if err != nil {
		if errors.Is(err, domain.ErrPrincipalStatusStale) {
			// A newer status is already projected. Benign: the state the
			// stream is converging on already holds, and re-applying an older
			// status would un-reinstate a principal who was reinstated.
			c.log.Debug("principal status event: a newer status is already projected — skipped",
				zap.String("principal_id", principalID),
				zap.String("status", status))
			return nil
		}
		// Logged at Error with the consequence spelled out, because the
		// direction of this failure matters: a SUSPENSION that fails to
		// project leaves the principal fully authorized.
		c.log.Error("principal status event: projection failed — this principal's authority is unchanged until it is replayed",
			zap.String("event_id", env.EventID),
			zap.String("principal_id", principalID),
			zap.String("status", status),
			zap.String("correlation_id", env.CorrelationID),
			zap.Error(err))
		return err // DB error - return error to allow retry
	}

	// At Warn rather than Info when the projected status is not ACTIVE: a
	// principal being stood down on the authorization plane is an event an
	// operator should see without going looking, and it is rare enough that
	// Warn does not become noise.
	fields := []zap.Field{
		zap.String("principal_id", projected.PrincipalID),
		zap.String("tenant_id", projected.TenantID),
		zap.String("status", projected.Status),
		zap.String("correlation_id", env.CorrelationID),
	}
	if projected.Status == domain.PrincipalStatusActive {
		c.log.Info("principal status projected — this principal's grants resolve normally again", fields...)
	} else {
		c.log.Warn("principal status projected — this principal is now denied EVERY action until reinstated", fields...)
	}
	return nil
}

// handleInvalidation drops the cached grant and delegation reads for the
// tenant the event names.
//
// ── AN EMPTY TENANT INVALIDATES EVERYTHING, DELIBERATELY ────────────────────
//
// Unlike the projecting path, a tenantless event here is NOT refused. The two
// failure directions are not symmetric:
//
//	over-invalidate   the tenant (or every tenant) reads the database on its
//	                  next evaluation instead of the cache. Costs latency,
//	                  once, on entries that were about to expire anyway.
//	under-invalidate  a replica keeps serving a grant set that an
//	                  authoritative service has just changed, for up to the
//	                  TTL, and answers /v1/authorize from it.
//
// The second is a correctness problem on the authorization path and the first
// is a performance one, so the tenantless case takes the expensive branch.
// cache.Store.invalidate already reads an empty tenant as "every tenant" via
// its global generation counter, which is the same treatment a platform-wide
// SoD rule gets and for the same reason.
//
// permission.bundle.updated is worth noting specifically: this consumer passes
// the event's tenant, but the CACHE invalidates bundles across every tenant on
// its own write path, because a bundle knows its role and not its tenant.
// Passing the tenant here is therefore the narrower of the two and is correct —
// access-control-svc's envelope does name the tenant the bundle belongs to.
func (c *LifecycleConsumer) handleInvalidation(env inbound) {
	tenantID := strings.TrimSpace(env.TenantID)

	c.store.InvalidateGrantSourcesForTenant(tenantID)

	if tenantID == "" {
		c.log.Warn("grant-graph event carried no tenant_id — invalidated cached grants for EVERY tenant",
			zap.String("event_id", env.EventID),
			zap.String("event_type", env.EventType),
			zap.String("correlation_id", env.CorrelationID))
		return
	}

	c.log.Info("grant-graph event: cached grants invalidated",
		zap.String("event_type", env.EventType),
		zap.String("tenant_id", tenantID),
		zap.String("correlation_id", env.CorrelationID))
}
