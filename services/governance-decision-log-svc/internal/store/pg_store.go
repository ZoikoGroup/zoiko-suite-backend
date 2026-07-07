// Package store provides the append-only persistence layer for governance
// decisions.
//
// Architectural constraints (doctrine.md, mirrored from
// audit-event-store-svc/internal/store/store.go):
//   - No UPDATE or DELETE on any stored decision — ever.
//   - Idempotency is guaranteed by a single atomic database statement:
//       INSERT INTO governance_decisions … ON CONFLICT (decision_id) DO NOTHING
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
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
)

// ListParams narrows a query across the five required filters
// (03-microservices.md §8.7): actor, entity, action, rule basis, time
// range. All fields are optional and compose with AND semantics.
type ListParams struct {
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

	// FindByID retrieves a single decision by its DecisionID.
	// Returns domain.ErrDecisionNotFound if no row matches.
	FindByID(ctx context.Context, decisionID string) (*domain.GovernanceDecision, error)

	// List returns a paginated slice of decisions matching params. All
	// filter fields are optional and compose with AND semantics.
	List(ctx context.Context, params ListParams) ([]*domain.GovernanceDecision, error)
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
     outcome, rule_basis, evaluation_context, correlation_id, decided_at)
VALUES
    ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (decision_id) DO NOTHING`

	tag, err := s.pool.Exec(ctx, q,
		d.DecisionID,
		d.TenantID,
		d.LegalEntityID,
		d.ActorID,
		d.ActionType,
		d.Outcome,
		d.RuleBasis,
		nullableJSON(d.EvaluationContext),
		d.CorrelationID,
		d.DecidedAt,
	)
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
	outcome, rule_basis, evaluation_context, correlation_id, decided_at`

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
		&d.DecidedAt,
	)
	return &d, err
}

// FindByID retrieves a single decision row.
func (s *PgStore) FindByID(ctx context.Context, decisionID string) (*domain.GovernanceDecision, error) {
	const q = `SELECT ` + decisionColumns + `
FROM governance_decisions
WHERE decision_id = $1`

	d, err := scanDecision(s.pool.QueryRow(ctx, q, decisionID))
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

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		s.log.Error("pg List failed", zap.Error(err))
		return nil, fmt.Errorf("%w: list governance decisions: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()

	var results []*domain.GovernanceDecision
	for rows.Next() {
		d, scanErr := scanDecision(rows)
		if scanErr != nil {
			s.log.Error("pg List scan failed", zap.Error(scanErr))
			return nil, fmt.Errorf("%w: list governance decisions: scan: %v", domain.ErrStoreUnavailable, scanErr)
		}
		results = append(results, d)
	}
	if err := rows.Err(); err != nil {
		s.log.Error("pg List rows error", zap.Error(err))
		return nil, fmt.Errorf("%w: list governance decisions: rows: %v", domain.ErrStoreUnavailable, err)
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

// ─── compile-time interface check ──────────────────────────────────────────

var _ Store = (*PgStore)(nil)
