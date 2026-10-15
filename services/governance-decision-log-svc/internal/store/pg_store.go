// Package store provides the append-only persistence layer for governance
// decisions.
//
// Architectural constraints (doctrine.md, mirrored from
// audit-event-store-svc/internal/store/store.go):
//   - No UPDATE or DELETE on any stored decision — ever.
//   - Idempotency is guaranteed by a single atomic database statement:
//     INSERT INTO governance_decisions … ON CONFLICT (tenant_id, decision_id) DO NOTHING
//     A prior SELECT-EXISTS check is explicitly prohibited: two concurrent
//     callers can both pass a SELECT EXISTS check before either inserts,
//     producing a duplicate row. The ON CONFLICT clause makes the entire
//     upsert atomic at the database level.
//   - A decision, its Idempotency-Key and its outbox event commit together or
//     not at all (RecordDecision). Writing them in three transactions is what
//     let an event, or a key, be lost after the decision committed.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
)

// ListParams narrows a query across the five required filters
// (03-microservices.md §8.7): actor, entity, action, rule basis, time
// range. All fields are optional and compose with AND semantics.
type ListParams struct {
	TenantID      string
	ActorID       string
	LegalEntityID string
	ActionType    string
	RuleBasis     string
	From          time.Time
	To            time.Time
	Limit         int
	Offset        int
}

// DecisionRecord is everything one accepted POST /v1/decisions writes.
type DecisionRecord struct {
	Decision domain.GovernanceDecision
	// IdempotencyKey and BodyHash are recorded only when the decision is
	// newly inserted. Empty IdempotencyKey records no key.
	IdempotencyKey string
	BodyHash       []byte
	// Event is the complete event envelope (events.DecisionRecorded), queued
	// in the outbox only when the decision is newly inserted. Nil queues none.
	Event []byte
}

// ReplayEvent is the outbox row queued with a newly recorded replay manifest.
type ReplayEvent struct {
	// Envelope is the complete event (events.DecisionReplayed).
	Envelope      []byte
	LegalEntityID string
	CorrelationID string
}

// Store is the persistence interface for governance decisions.
type Store interface {
	// RecordDecision writes rec in one transaction. When the tenant already
	// holds rec.Decision.DecisionID it writes nothing and returns the STORED
	// row with created=false, so the caller can compare it with what it was
	// sent instead of echoing an unstored body. ErrIdempotencyKeyInUse means a
	// concurrent request committed the same key first; nothing was written.
	RecordDecision(ctx context.Context, rec DecisionRecord) (stored *domain.GovernanceDecision, created bool, err error)

	// FindByID retrieves a single decision by its DecisionID, scoped to
	// tenantID. A decision belonging to a different tenant is indistinguishable
	// from a nonexistent one — returns domain.ErrDecisionNotFound in both cases.
	FindByID(ctx context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error)

	// List returns a paginated slice of decisions matching params, always
	// scoped to params.TenantID. All other filter fields are optional and
	// compose with AND semantics.
	List(ctx context.Context, params ListParams) ([]*domain.GovernanceDecision, error)

	// CreateReplayManifest is idempotent on (tenant, decision, m.IdempotencyKey):
	// when a manifest already exists for that key it writes nothing and returns
	// the stored manifest with created=false. A non-nil event is queued in the
	// outbox in the same transaction, only when the manifest is new.
	CreateReplayManifest(ctx context.Context, m *domain.ReplayManifest, event *ReplayEvent) (stored *domain.ReplayManifest, created bool, err error)
	ListReplayManifestsByDecision(ctx context.Context, tenantID, decisionID string) ([]*domain.ReplayManifest, error)

	// CheckIdempotencyKey returns the decision_id and body hash recorded for
	// a key, or ("", nil, nil) when the key is unused in this tenant.
	CheckIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (string, []byte, error)
}

// PgStore implements Store against PostgreSQL via pgxpool.
type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

// New constructs a PgStore.
func New(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

// withRLS runs fn inside a transaction with app.tenant_id set via
// set_config, so the tenant_isolation_policy RLS policy on
// governance_decisions (migration 000002) actually scopes every statement
// fn issues. Mirrors the withRLS pattern used throughout the platform (e.g.
// employee-master-svc).
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// RecordDecision — see Store.
//
// The insert is ON CONFLICT (tenant_id, decision_id) DO NOTHING, the key since
// 000013. RETURNING yields no row when the conflict fired, and the stored row
// is then read back inside the same transaction.
func (s *PgStore) RecordDecision(ctx context.Context, rec DecisionRecord) (*domain.GovernanceDecision, bool, error) {
	d := rec.Decision
	const insertQ = `
INSERT INTO governance_decisions
    (decision_id, tenant_id, legal_entity_id, actor_id, action_type,
     outcome, rule_basis, policy_version_id, evaluation_context, correlation_id,
     workflow_instance_id, causation_id, action_subject_type, action_subject_id, decided_at,
     jurisdiction_context)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (tenant_id, decision_id) DO NOTHING
RETURNING ` + decisionColumns

	var stored *domain.GovernanceDecision
	created := false
	err := s.withRLS(ctx, d.TenantID, func(tx pgx.Tx) error {
		row, scanErr := scanDecision(tx.QueryRow(ctx, insertQ,
			d.DecisionID,
			d.TenantID,
			d.LegalEntityID,
			d.ActorID,
			d.ActionType,
			d.Outcome,
			d.RuleBasis,
			nullableString(d.PolicyVersionID),
			nullableJSON(d.EvaluationContext),
			d.CorrelationID,
			d.WorkflowInstanceID,
			d.CausationID,
			nullableString(d.ActionSubjectType),
			nullableString(d.ActionSubjectID),
			d.DecidedAt,
			nullableString(d.JurisdictionContext),
		))
		if errors.Is(scanErr, pgx.ErrNoRows) {
			// The tenant already holds this decision_id. Write nothing more:
			// whether this is a true replay is the caller's call to make
			// against the stored row.
			existing, findErr := scanDecision(tx.QueryRow(ctx,
				`SELECT `+decisionColumns+` FROM governance_decisions WHERE tenant_id = $1 AND decision_id = $2`,
				d.TenantID, d.DecisionID))
			stored = existing
			return findErr
		}
		if scanErr != nil {
			return scanErr
		}
		stored, created = row, true

		if rec.IdempotencyKey != "" {
			tag, keyErr := tx.Exec(ctx,
				`INSERT INTO idempotency_keys (idempotency_key, tenant_id, body_hash, decision_id)
				 VALUES ($1, $2, $3, $4)
				 ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
				rec.IdempotencyKey, d.TenantID, rec.BodyHash, d.DecisionID)
			if keyErr != nil {
				return keyErr
			}
			if tag.RowsAffected() == 0 {
				return domain.ErrIdempotencyKeyInUse
			}
		}

		if rec.Event != nil {
			if _, evErr := tx.Exec(ctx,
				`INSERT INTO outbox (event_type, payload, tenant_id, legal_entity_id, actor_id, correlation_id, idempotency_key, aggregate_id)
				 VALUES ('governance.decision.recorded', $1, $2, $3, $4, $5, $6, $7)`,
				rec.Event, d.TenantID, d.LegalEntityID, d.ActorID, d.CorrelationID, rec.IdempotencyKey, d.DecisionID); evErr != nil {
				return evErr
			}
		}
		return nil
	})
	if errors.Is(err, domain.ErrIdempotencyKeyInUse) {
		return nil, false, err
	}
	if err != nil {
		s.log.Error("pg RecordDecision failed", zap.String("decision_id", d.DecisionID), zap.Error(err))
		return nil, false, fmt.Errorf("%w: record governance decision %q: %v", domain.ErrStoreUnavailable, d.DecisionID, err)
	}
	s.log.Debug("governance decision insert",
		zap.String("decision_id", d.DecisionID),
		zap.String("tenant_id", d.TenantID),
		zap.Bool("created", created),
	)
	return stored, created, nil
}

// decisionColumns is the standard SELECT column list shared by all queries.
// Order must match scanDecision exactly.
const decisionColumns = `
	decision_id, tenant_id, legal_entity_id, actor_id, action_type,
	outcome, rule_basis, policy_version_id, evaluation_context, correlation_id,
	workflow_instance_id, causation_id, action_subject_type, action_subject_id, decided_at,
	jurisdiction_context,
	stored_at`

// scanDecision scans one row produced by a decisionColumns SELECT.
func scanDecision(row pgx.Row) (*domain.GovernanceDecision, error) {
	var d domain.GovernanceDecision
	err := row.Scan(
		&d.DecisionID,
		&d.TenantID,
		&d.LegalEntityID,
		&d.ActorID,
		&d.ActionType,
		&d.Outcome,
		&d.RuleBasis,
		&d.PolicyVersionID,
		&d.EvaluationContext,
		&d.CorrelationID,
		&d.WorkflowInstanceID,
		&d.CausationID,
		&d.ActionSubjectType,
		&d.ActionSubjectID,
		&d.DecidedAt,
		&d.JurisdictionContext,
		&d.RecordedAt,
	)
	return &d, err
}

// FindByID retrieves a single decision row, scoped to tenantID. The explicit
// tenant_id = $2 filter is deliberate defense-in-depth alongside the RLS
// policy — see withRLS — rather than relying on RLS alone.
func (s *PgStore) FindByID(ctx context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error) {
	const q = `SELECT ` + decisionColumns + `
FROM governance_decisions
WHERE decision_id = $1 AND tenant_id = $2`

	var d *domain.GovernanceDecision
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var scanErr error
		d, scanErr = scanDecision(tx.QueryRow(ctx, q, decisionID, tenantID))
		return scanErr
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDecisionNotFound
	}
	if err != nil {
		s.log.Error("pg FindByID failed", zap.String("decision_id", decisionID), zap.Error(err))
		return nil, fmt.Errorf("%w: find governance decision %q: %v", domain.ErrStoreUnavailable, decisionID, err)
	}
	return d, nil
}

// List returns a paginated, optionally-filtered slice of decisions, newest
// first. All five filters (actor, entity, action, rule basis, time range)
// are optional and compose with AND semantics.
func (s *PgStore) List(ctx context.Context, params ListParams) ([]*domain.GovernanceDecision, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := params.Offset
	if offset < 0 {
		offset = 0
	}

	args := []any{}
	conditions := []string{}
	argIdx := 1

	addCond := func(cond string, val any) {
		conditions = append(conditions, fmt.Sprintf(cond, argIdx))
		args = append(args, val)
		argIdx++
	}

	// tenant_id is always the first condition — every List call is
	// tenant-scoped, backstopped by the RLS policy set in withRLS below.
	addCond("tenant_id = $%d", params.TenantID)

	if params.ActorID != "" {
		addCond("actor_id = $%d", params.ActorID)
	}
	if params.LegalEntityID != "" {
		addCond("legal_entity_id = $%d", params.LegalEntityID)
	}
	if params.ActionType != "" {
		addCond("action_type = $%d", params.ActionType)
	}
	if params.RuleBasis != "" {
		addCond("rule_basis = $%d", params.RuleBasis)
	}
	if !params.From.IsZero() {
		addCond("decided_at >= $%d", params.From)
	}
	if !params.To.IsZero() {
		addCond("decided_at <= $%d", params.To)
	}

	where := "WHERE " + strings.Join(conditions, " AND ")

	query := fmt.Sprintf(`
SELECT %s
FROM   governance_decisions
%s
ORDER BY decided_at DESC
LIMIT  $%d OFFSET $%d`,
		decisionColumns, where, argIdx, argIdx+1,
	)
	args = append(args, limit, offset)

	var results []*domain.GovernanceDecision
	err := s.withRLS(ctx, params.TenantID, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, query, args...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()

		for rows.Next() {
			d, scanErr := scanDecision(rows)
			if scanErr != nil {
				return scanErr
			}
			results = append(results, d)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg List failed", zap.Error(err))
		return nil, fmt.Errorf("%w: list governance decisions: %v", domain.ErrStoreUnavailable, err)
	}
	return results, nil
}

// nullableJSON converts an empty/nil RawMessage to nil so Postgres stores
// SQL NULL instead of an empty JSONB value.
func nullableJSON(raw []byte) interface{} {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

// nullableString converts a nil or empty string pointer to nil so Postgres stores
// SQL NULL instead of an empty string.
func nullableString(s *string) interface{} {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// ── replay manifests (backlog item 34) ────────────────────────────────────────

const replayManifestColumns = `
	replay_manifest_id, decision_id, policy_version_id,
	replayed_outcome, original_outcome, outcomes_match, replay_notes,
	replayed_at, replayed_by_principal_id, tenant_id,
	COALESCE(idempotency_key, ''), request_hash`

func scanReplayManifest(row pgx.Row) (*domain.ReplayManifest, error) {
	m := &domain.ReplayManifest{}
	err := row.Scan(
		&m.ReplayManifestID, &m.DecisionID, &m.PolicyVersionID,
		&m.ReplayedOutcome, &m.OriginalOutcome, &m.OutcomesMatch, &m.ReplayNotes,
		&m.ReplayedAt, &m.ReplayedByPrincipalID, &m.TenantID,
		&m.IdempotencyKey, &m.RequestHash,
	)
	return m, err
}

// CreateReplayManifest — see Store. The manifest inherits the tenant_id from
// the parent decision, so the insert runs under that tenant's RLS scope.
//
// The unique index from 000014 is the arbiter: of two concurrent replays with
// one key, exactly one inserts, and the other reads back the winner's manifest.
func (s *PgStore) CreateReplayManifest(ctx context.Context, m *domain.ReplayManifest, event *ReplayEvent) (*domain.ReplayManifest, bool, error) {
	const insertQ = `
		INSERT INTO replay_manifests (
			replay_manifest_id, decision_id, policy_version_id,
			replayed_outcome, original_outcome, outcomes_match, replay_notes,
			replayed_at, replayed_by_principal_id, tenant_id,
			idempotency_key, request_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), $12)
		ON CONFLICT (tenant_id, decision_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING ` + replayManifestColumns

	var stored *domain.ReplayManifest
	created := false
	err := s.withRLS(ctx, m.TenantID, func(tx pgx.Tx) error {
		row, scanErr := scanReplayManifest(tx.QueryRow(ctx, insertQ,
			m.ReplayManifestID, m.DecisionID, m.PolicyVersionID,
			m.ReplayedOutcome, m.OriginalOutcome, m.OutcomesMatch, m.ReplayNotes,
			m.ReplayedAt, m.ReplayedByPrincipalID, m.TenantID,
			m.IdempotencyKey, m.RequestHash,
		))
		if errors.Is(scanErr, pgx.ErrNoRows) {
			existing, findErr := scanReplayManifest(tx.QueryRow(ctx,
				`SELECT `+replayManifestColumns+` FROM replay_manifests
				 WHERE tenant_id = $1 AND decision_id = $2 AND idempotency_key = $3`,
				m.TenantID, m.DecisionID, m.IdempotencyKey))
			stored = existing
			return findErr
		}
		if scanErr != nil {
			return scanErr
		}
		stored, created = row, true

		if event != nil {
			if _, evErr := tx.Exec(ctx,
				`INSERT INTO outbox (event_type, payload, tenant_id, legal_entity_id, actor_id, correlation_id, idempotency_key, aggregate_id)
				 VALUES ('governance.decision.replayed', $1, $2, $3, $4, $5, $6, $7)`,
				event.Envelope, m.TenantID, event.LegalEntityID, m.ReplayedByPrincipalID,
				event.CorrelationID, m.IdempotencyKey, m.DecisionID); evErr != nil {
				return evErr
			}
		}
		return nil
	})
	if err != nil {
		s.log.Error("pg CreateReplayManifest failed", zap.String("decision_id", m.DecisionID), zap.Error(err))
		return nil, false, fmt.Errorf("%w: insert replay manifest: %v", domain.ErrStoreUnavailable, err)
	}
	return stored, created, nil
}

func (s *PgStore) ListReplayManifestsByDecision(ctx context.Context, tenantID, decisionID string) ([]*domain.ReplayManifest, error) {
	// Explicit tenant_id filter for defense-in-depth alongside RLS
	// (mirrors FindByID pattern for governance_decisions)
	const q = `SELECT ` + replayManifestColumns + `
FROM replay_manifests
WHERE decision_id = $1 AND tenant_id = $2
ORDER BY replayed_at DESC`

	var out []*domain.ReplayManifest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, q, decisionID, tenantID)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()

		for rows.Next() {
			m, scanErr := scanReplayManifest(rows)
			if scanErr != nil {
				return fmt.Errorf("%w: scan replay manifest: %v", domain.ErrStoreUnavailable, scanErr)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		s.log.Error("pg ListReplayManifestsByDecision failed", zap.String("decision_id", decisionID), zap.Error(err))
		return nil, fmt.Errorf("%w: list replay manifests: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// CheckIdempotencyKey retrieves the stored decision_id and body_hash for a
// given idempotency key. Returns ("", nil, nil) if the key doesn't exist.
func (s *PgStore) CheckIdempotencyKey(ctx context.Context, tenantID, idempotencyKey string) (string, []byte, error) {
	var decisionID string
	var bodyHash []byte
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT decision_id, body_hash FROM idempotency_keys WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenantID, idempotencyKey,
		).Scan(&decisionID, &bodyHash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		s.log.Error("pg CheckIdempotencyKey failed", zap.String("idempotency_key", idempotencyKey), zap.Error(err))
		return "", nil, fmt.Errorf("%w: check idempotency key: %v", domain.ErrStoreUnavailable, err)
	}
	return decisionID, bodyHash, nil
}

// ─── compile-time interface check ──────────────────────────────────────────

var _ Store = (*PgStore)(nil)
