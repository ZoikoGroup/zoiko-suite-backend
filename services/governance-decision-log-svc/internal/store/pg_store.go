// Package store provides the append-only persistence layer for governance
// decisions.
//
// Architectural constraints (doctrine.md, mirrored from
// audit-event-store-svc/internal/store/store.go):
//   - No UPDATE or DELETE on any stored decision — ever.
//   - Idempotency is guaranteed by a single atomic database statement:
//     INSERT INTO governance_decisions … ON CONFLICT (decision_id) DO NOTHING
//     A prior SELECT-EXISTS check is explicitly prohibited: two concurrent
//     callers can both pass a SELECT EXISTS check before either inserts,
//     producing a duplicate row. The ON CONFLICT clause makes the entire
//     upsert atomic at the database level.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// Store is the persistence interface for governance decisions.
type Store interface {
	// Insert persists d atomically. If a row with the same DecisionID
	// already exists the call is a no-op and returns (false, nil).
	// Returns (true, nil) if this call performed the insert.
	Insert(ctx context.Context, d domain.GovernanceDecision) (created bool, err error)

	// FindByID retrieves a single decision by its DecisionID, scoped to
	// tenantID. A decision belonging to a different tenant is indistinguishable
	// from a nonexistent one — returns domain.ErrDecisionNotFound in both cases.
	FindByID(ctx context.Context, tenantID, decisionID string) (*domain.GovernanceDecision, error)

	// List returns a paginated slice of decisions matching params, always
	// scoped to params.TenantID. All other filter fields are optional and
	// compose with AND semantics.
	List(ctx context.Context, params ListParams) ([]*domain.GovernanceDecision, error)

	// ── replay manifests (backlog item 34) ──────────────────────────────────
	CreateReplayManifest(ctx context.Context, m *domain.ReplayManifest) error
	ListReplayManifestsByDecision(ctx context.Context, decisionID string) ([]*domain.ReplayManifest, error)
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

// Insert writes d into governance_decisions.
//
// The critical dedup guarantee is expressed in a single SQL statement:
//
//	INSERT INTO governance_decisions … ON CONFLICT (decision_id) DO NOTHING
//
// This is the ONLY safe pattern — see package doc comment. RowsAffected
// tells us whether this call performed the insert (1) or lost the race to
// an existing row (0), without a separate SELECT.
func (s *PgStore) Insert(ctx context.Context, d domain.GovernanceDecision) (bool, error) {
	const q = `
INSERT INTO governance_decisions
    (decision_id, tenant_id, legal_entity_id, actor_id, action_type,
     outcome, rule_basis, evaluation_context, correlation_id,
     workflow_instance_id, causation_id, decided_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (decision_id) DO NOTHING`

	var tag pgconn.CommandTag
	err := s.withRLS(ctx, d.TenantID, func(tx pgx.Tx) error {
		var execErr error
		tag, execErr = tx.Exec(ctx, q,
			d.DecisionID,
			d.TenantID,
			d.LegalEntityID,
			d.ActorID,
			d.ActionType,
			d.Outcome,
			d.RuleBasis,
			nullableJSON(d.EvaluationContext),
			d.CorrelationID,
			d.WorkflowInstanceID,
			d.CausationID,
			d.DecidedAt,
		)
		return execErr
	})
	if err != nil {
		s.log.Error("pg Insert failed", zap.String("decision_id", d.DecisionID), zap.Error(err))
		return false, fmt.Errorf("%w: insert governance decision %q: %v", domain.ErrStoreUnavailable, d.DecisionID, err)
	}

	created := tag.RowsAffected() == 1
	s.log.Debug("governance decision insert",
		zap.String("decision_id", d.DecisionID),
		zap.String("tenant_id", d.TenantID),
		zap.Bool("created", created),
	)
	return created, nil
}

// decisionColumns is the standard SELECT column list shared by all queries.
// Order must match scanDecision exactly.
const decisionColumns = `
	decision_id, tenant_id, legal_entity_id, actor_id, action_type,
	outcome, rule_basis, evaluation_context, correlation_id,
	workflow_instance_id, causation_id, decided_at`

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
		&d.EvaluationContext,
		&d.CorrelationID,
		&d.WorkflowInstanceID,
		&d.CausationID,
		&d.DecidedAt,
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

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := fmt.Sprintf(`
SELECT %s
FROM   governance_decisions
%s
ORDER BY decided_at DESC
LIMIT  $%d OFFSET $%d`,
		decisionColumns, where, argIdx, argIdx+1,
	)
	args = append(args, limit, params.Offset)

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

// ── replay manifests (backlog item 34) ────────────────────────────────────────

const replayManifestColumns = `
	replay_manifest_id, decision_id, policy_version_id,
	replayed_outcome, original_outcome, outcomes_match, replay_notes,
	replayed_at, replayed_by_principal_id`

func scanReplayManifest(row pgx.Row) (*domain.ReplayManifest, error) {
	m := &domain.ReplayManifest{}
	err := row.Scan(
		&m.ReplayManifestID, &m.DecisionID, &m.PolicyVersionID,
		&m.ReplayedOutcome, &m.OriginalOutcome, &m.OutcomesMatch, &m.ReplayNotes,
		&m.ReplayedAt, &m.ReplayedByPrincipalID,
	)
	return m, err
}

// CreateReplayManifest inserts a new manifest. No RLS/tenant scoping —
// replay_manifests has no tenant_id of its own; access is via its parent
// decision_id, which IS tenant-scoped.
func (s *PgStore) CreateReplayManifest(ctx context.Context, m *domain.ReplayManifest) error {
	const q = `
		INSERT INTO replay_manifests (
			replay_manifest_id, decision_id, policy_version_id,
			replayed_outcome, original_outcome, outcomes_match, replay_notes,
			replayed_by_principal_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	_, err := s.pool.Exec(ctx, q,
		m.ReplayManifestID, m.DecisionID, m.PolicyVersionID,
		m.ReplayedOutcome, m.OriginalOutcome, m.OutcomesMatch, m.ReplayNotes,
		m.ReplayedByPrincipalID,
	)
	if err != nil {
		s.log.Error("pg CreateReplayManifest failed", zap.String("decision_id", m.DecisionID), zap.Error(err))
		return fmt.Errorf("%w: insert replay manifest: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) ListReplayManifestsByDecision(ctx context.Context, decisionID string) ([]*domain.ReplayManifest, error) {
	const q = `SELECT ` + replayManifestColumns + `
FROM replay_manifests
WHERE decision_id = $1
ORDER BY replayed_at DESC`

	rows, err := s.pool.Query(ctx, q, decisionID)
	if err != nil {
		s.log.Error("pg ListReplayManifestsByDecision failed", zap.String("decision_id", decisionID), zap.Error(err))
		return nil, fmt.Errorf("%w: list replay manifests: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var out []*domain.ReplayManifest
	for rows.Next() {
		m, scanErr := scanReplayManifest(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: scan replay manifest: %v", domain.ErrStoreUnavailable, scanErr)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ─── compile-time interface check ──────────────────────────────────────────

var _ Store = (*PgStore)(nil)
