package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/decision-support-svc/internal/domain"
	svcmiddleware "zoiko.io/decision-support-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

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

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

const recommendationColumns = `
	recommendation_id, tenant_id, legal_entity_id, subject_type, subject_reference,
	action_type, recommended_action, confidence_score, rationale, prior_decisions_sampled,
	requested_by_principal_id, correlation_id, created_at
`

func scanRecommendation(row pgx.Row, r *domain.Recommendation) error {
	var recommendedAction string
	if err := row.Scan(
		&r.RecommendationID, &r.TenantID, &r.LegalEntityID, &r.SubjectType, &r.SubjectReference,
		&r.ActionType, &recommendedAction, &r.ConfidenceScore, &r.Rationale, &r.PriorDecisionsSampled,
		&r.RequestedByPrincipalID, &r.CorrelationID, &r.CreatedAt,
	); err != nil {
		return err
	}
	r.RecommendedAction = domain.RecommendedAction(recommendedAction)
	return nil
}

// CreateRecommendation inserts a new recommendation record, idempotent on
// (tenant_id, correlation_id) — a retried request replays the original
// recommendation rather than re-querying governance-decision-log-svc and
// potentially computing a slightly different answer on retry.
func (s *PgStore) CreateRecommendation(ctx context.Context, r *domain.Recommendation) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO recommendations (
				recommendation_id, tenant_id, legal_entity_id, subject_type, subject_reference,
				action_type, recommended_action, confidence_score, rationale, prior_decisions_sampled,
				requested_by_principal_id, correlation_id, created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, r.RecommendationID, tenantID, r.LegalEntityID, r.SubjectType, r.SubjectReference,
			r.ActionType, string(r.RecommendedAction), r.ConfidenceScore, r.Rationale, r.PriorDecisionsSampled,
			r.RequestedByPrincipalID, r.CorrelationID, r.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}
		row := tx.QueryRow(ctx, "SELECT "+recommendationColumns+" FROM recommendations WHERE tenant_id = $1 AND correlation_id = $2", tenantID, r.CorrelationID)
		return scanRecommendation(row, r)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) GetRecommendation(ctx context.Context, recommendationID string) (*domain.Recommendation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var r domain.Recommendation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+recommendationColumns+" FROM recommendations WHERE tenant_id = $1 AND recommendation_id = $2", tenantID, recommendationID)
		return scanRecommendation(row, &r)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrRecommendationNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PgStore) ListRecommendations(ctx context.Context, legalEntityID, subjectReference string) ([]domain.Recommendation, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.Recommendation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := "SELECT " + recommendationColumns + " FROM recommendations WHERE tenant_id = $1"
		args := []any{tenantID}
		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if subjectReference != "" {
			args = append(args, subjectReference)
			query += fmt.Sprintf(" AND subject_reference = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.Recommendation
			if err := scanRecommendation(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
