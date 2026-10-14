package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"zoiko.io/policy-svc/internal/domain"
)

// OutboxEvent is one row for the transactional outbox. The outbox relay
// (internal/outbox) wraps it in the platform event envelope on publish.
type OutboxEvent struct {
	EventType      string
	EventVersion   string
	SchemaVersion  string
	SourceService  string
	TenantID       *string
	LegalEntityID  *string
	ActorID        string
	CorrelationID  string
	CausationID    string
	IdempotencyKey string

	// JurisdictionContext is the request's jurisdiction, when it named one.
	JurisdictionContext string

	// AggregateID keys the Kafka message, so every event of one policy
	// family lands on one partition in order.
	AggregateID      string
	AggregateVersion int

	Payload []byte
}

// enqueue writes ev inside tx — the same transaction as the state change it
// reports (GCP §2 invariant #10: "outbox publication occurs only after
// authoritative commit").
func (s *PgStore) enqueue(ctx context.Context, tx pgx.Tx, ev OutboxEvent) error {
	if ev.EventVersion == "" {
		ev.EventVersion = "1.0"
	}
	if ev.SchemaVersion == "" {
		ev.SchemaVersion = "1.0"
	}
	if ev.SourceService == "" {
		ev.SourceService = "policy-svc"
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox (
			event_type, event_version, schema_version, source_service,
			tenant_id, legal_entity_id, actor_id, correlation_id, causation_id,
			idempotency_key, aggregate_id, aggregate_version, payload, jurisdiction_context
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);`,
		ev.EventType, ev.EventVersion, ev.SchemaVersion, ev.SourceService,
		ev.TenantID, ev.LegalEntityID, ev.ActorID, ev.CorrelationID, nullableString(ev.CausationID),
		ev.IdempotencyKey, ev.AggregateID, ev.AggregateVersion, ev.Payload, nullableString(ev.JurisdictionContext))
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", ev.EventType, err)
	}
	return nil
}

// OutboxBacklog counts unpublished events and the age of the oldest one.
func (s *PgStore) OutboxBacklog(ctx context.Context) (pending int64, oldestSeconds float64, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(EXTRACT(EPOCH FROM (now() - MIN(created_at))), 0)::float8
		FROM outbox WHERE published_at IS NULL;`).Scan(&pending, &oldestSeconds)
	if err != nil {
		return 0, 0, s.domainError("OutboxBacklog", err)
	}
	return pending, oldestSeconds, nil
}

// EnqueueEvent records an event that accompanies no state change of its own
// (policy.applicability.conflicted), in its own transaction.
func (s *PgStore) EnqueueEvent(ctx context.Context, ev OutboxEvent) error {
	err := s.withRLS(ctx, derefOrEmpty(ev.TenantID), func(tx pgx.Tx) error {
		return s.enqueue(ctx, tx, ev)
	})
	if err != nil {
		return s.domainError("EnqueueEvent", err)
	}
	return nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only maps of plain values reach here; a failure is a programming
		// error, and an empty object is safer than a panic in a request.
		return []byte(`{}`)
	}
	return b
}

func policyEvent(eventType string, p *domain.Policy, meta domain.EventMeta) OutboxEvent {
	return OutboxEvent{
		EventType:           eventType,
		TenantID:            p.TenantID,
		ActorID:             p.CreatedByPrincipalID,
		CorrelationID:       meta.CorrelationID,
		CausationID:         meta.CausationID,
		IdempotencyKey:      meta.IdempotencyKey,
		JurisdictionContext: meta.JurisdictionContext,
		AggregateID:         p.PolicyID,
		AggregateVersion:    1,
		Payload: mustJSON(map[string]any{
			"policy_id":               p.PolicyID,
			"policy_code":             p.PolicyCode,
			"policy_name":             p.PolicyName,
			"policy_type":             p.PolicyType,
			"tenant_id":               p.TenantID,
			"policy_status":           p.PolicyStatus,
			"versioning_mode":         p.VersioningMode,
			"created_by_principal_id": p.CreatedByPrincipalID,
			"created_at":              p.CreatedAt,
		}),
	}
}

// versionEvent builds a lifecycle event for v. The payload always carries the
// version's identity, scope, window, status and digest, so a consumer can
// refresh by exact version and hash (V-001 §20.2).
func versionEvent(eventType string, v *domain.PolicyVersion, actor string, meta domain.EventMeta, extra map[string]any) OutboxEvent {
	payload := map[string]any{
		"policy_version_id":         v.PolicyVersionID,
		"policy_id":                 v.PolicyID,
		"tenant_id":                 v.TenantID,
		"legal_entity_id":           v.LegalEntityID,
		"scope_type":                v.ScopeType,
		"version_number":            v.VersionNumber,
		"version_status":            v.VersionStatus,
		"effective_from":            v.EffectiveFrom,
		"effective_to":              v.EffectiveTo,
		"artifact_digest":           v.ArtifactDigest,
		"row_version":               v.RowVersion,
		"activated_by_principal_id": v.ActivatedByPrincipalID,
		"activated_at":              v.ActivatedAt,
		"approved_by_principal_id":  v.ApprovedByPrincipalID,
		"approval_reference":        v.ApprovalReference,
		"created_by_principal_id":   v.CreatedByPrincipalID,
		"suspended_by_principal_id": v.SuspendedByPrincipalID,
		"actor_id":                  actor,
	}
	for k, val := range extra {
		payload[k] = val
	}
	key := meta.IdempotencyKey
	if key == "" {
		key = v.PolicyVersionID + ":" + strconv.Itoa(v.RowVersion)
	}
	return OutboxEvent{
		EventType:           eventType,
		TenantID:            v.TenantID,
		LegalEntityID:       v.LegalEntityID,
		ActorID:             actor,
		CorrelationID:       meta.CorrelationID,
		CausationID:         meta.CausationID,
		IdempotencyKey:      key,
		JurisdictionContext: meta.JurisdictionContext,
		AggregateID:         v.PolicyID,
		AggregateVersion:    v.RowVersion,
		Payload:             mustJSON(payload),
	}
}
