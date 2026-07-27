package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/performance-review-svc/internal/domain"
	svcmiddleware "zoiko.io/performance-review-svc/internal/middleware"
)

// PgStore implements all data-access operations using pgxpool.
// Every method enforces:
//  1. Postgres Row-Level Security via set_config('app.tenant_id', ...)
//  2. Explicit AND tenant_id = $N filter in every query (RLS alone is insufficient
//     when the pool connects as a superuser — learned from general-ledger-svc CI)
type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withRLS opens a transaction, sets the RLS tenant config, runs fn, then commits.
// Any error causes an automatic rollback via defer.
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

// ── Review Cycle operations ───────────────────────────────────────────────────

func (s *PgStore) CreateCycle(ctx context.Context, c *domain.ReviewCycle) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO review_cycles (
				review_cycle_id, tenant_id, legal_entity_id, cycle_name, cycle_type,
				start_date, end_date, cycle_status, effective_from, effective_to,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		`, c.ReviewCycleID, tenantID, c.LegalEntityID, c.CycleName, c.CycleType,
			c.StartDate, c.EndDate, c.CycleStatus, c.EffectiveFrom, c.EffectiveTo,
			c.CreatedAt, c.UpdatedAt)
		return err
	})
}

func (s *PgStore) GetCycle(ctx context.Context, id string) (*domain.ReviewCycle, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var c domain.ReviewCycle
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT review_cycle_id, tenant_id, legal_entity_id, cycle_name, cycle_type,
			       start_date, end_date, cycle_status, effective_from, effective_to,
			       created_at, updated_at
			FROM review_cycles
			WHERE review_cycle_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&c.ReviewCycleID, &c.TenantID, &c.LegalEntityID, &c.CycleName, &c.CycleType,
			&c.StartDate, &c.EndDate, &c.CycleStatus, &c.EffectiveFrom, &c.EffectiveTo,
			&c.CreatedAt, &c.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCycleNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) ListCycles(ctx context.Context, legalEntityID, status string) ([]domain.ReviewCycle, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ReviewCycle
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT review_cycle_id, tenant_id, legal_entity_id, cycle_name, cycle_type,
			       start_date, end_date, cycle_status, effective_from, effective_to,
			       created_at, updated_at
			FROM review_cycles
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND cycle_status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.ReviewCycle
			if err := rows.Scan(
				&c.ReviewCycleID, &c.TenantID, &c.LegalEntityID, &c.CycleName, &c.CycleType,
				&c.StartDate, &c.EndDate, &c.CycleStatus, &c.EffectiveFrom, &c.EffectiveTo,
				&c.CreatedAt, &c.UpdatedAt,
			); err != nil {
				return err
			}
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) UpdateCycleStatus(ctx context.Context, id, newStatus string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE review_cycles
			SET cycle_status = $1, updated_at = $2
			WHERE review_cycle_id = $3 AND tenant_id = $4
		`, newStatus, time.Now().UTC(), id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrCycleNotFound
		}
		return nil
	})
}

// ── Performance Review operations ─────────────────────────────────────────────

func (s *PgStore) CreateReview(ctx context.Context, r *domain.PerformanceReview) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO performance_reviews (
				performance_review_id, tenant_id, legal_entity_id, review_cycle_id,
				employee_id, reviewer_principal_id, review_status, overall_rating,
				self_assessment_payload, manager_eval_payload, governance_decision_id,
				idempotency_key, completed_at, effective_from, effective_to,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		`, r.PerformanceReviewID, tenantID, r.LegalEntityID, r.ReviewCycleID,
			r.EmployeeID, r.ReviewerPrincipalID, r.ReviewStatus, r.OverallRating,
			nil, nil, r.GovernanceDecisionID,
			r.IdempotencyKey, r.CompletedAt, r.EffectiveFrom, r.EffectiveTo,
			r.CreatedAt, r.UpdatedAt)
		return err
	})

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.ErrDuplicateIdempotencyKey
	}
	return err
}

func (s *PgStore) GetReview(ctx context.Context, id string) (*domain.PerformanceReview, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var r domain.PerformanceReview
	var selfPayloadRaw, managerPayloadRaw []byte

	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT performance_review_id, tenant_id, legal_entity_id, review_cycle_id,
			       employee_id, reviewer_principal_id, review_status, overall_rating,
			       self_assessment_payload, manager_eval_payload, governance_decision_id,
			       idempotency_key, completed_at, effective_from, effective_to,
			       created_at, updated_at
			FROM performance_reviews
			WHERE performance_review_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&r.PerformanceReviewID, &r.TenantID, &r.LegalEntityID, &r.ReviewCycleID,
			&r.EmployeeID, &r.ReviewerPrincipalID, &r.ReviewStatus, &r.OverallRating,
			&selfPayloadRaw, &managerPayloadRaw, &r.GovernanceDecisionID,
			&r.IdempotencyKey, &r.CompletedAt, &r.EffectiveFrom, &r.EffectiveTo,
			&r.CreatedAt, &r.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrReviewNotFound
	}
	if err != nil {
		return nil, err
	}

	if len(selfPayloadRaw) > 0 {
		_ = json.Unmarshal(selfPayloadRaw, &r.SelfAssessmentPayload)
	}
	if len(managerPayloadRaw) > 0 {
		_ = json.Unmarshal(managerPayloadRaw, &r.ManagerEvalPayload)
	}
	return &r, nil
}

func (s *PgStore) ListReviews(ctx context.Context, cycleID, employeeID, status string) ([]domain.PerformanceReview, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.PerformanceReview
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT performance_review_id, tenant_id, legal_entity_id, review_cycle_id,
			       employee_id, reviewer_principal_id, review_status, overall_rating,
			       self_assessment_payload, manager_eval_payload, governance_decision_id,
			       idempotency_key, completed_at, effective_from, effective_to,
			       created_at, updated_at
			FROM performance_reviews
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if cycleID != "" {
			args = append(args, cycleID)
			query += fmt.Sprintf(" AND review_cycle_id = $%d", len(args))
		}
		if employeeID != "" {
			args = append(args, employeeID)
			query += fmt.Sprintf(" AND employee_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND review_status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var r domain.PerformanceReview
			var selfRaw, managerRaw []byte
			if err := rows.Scan(
				&r.PerformanceReviewID, &r.TenantID, &r.LegalEntityID, &r.ReviewCycleID,
				&r.EmployeeID, &r.ReviewerPrincipalID, &r.ReviewStatus, &r.OverallRating,
				&selfRaw, &managerRaw, &r.GovernanceDecisionID,
				&r.IdempotencyKey, &r.CompletedAt, &r.EffectiveFrom, &r.EffectiveTo,
				&r.CreatedAt, &r.UpdatedAt,
			); err != nil {
				return err
			}
			if len(selfRaw) > 0 {
				_ = json.Unmarshal(selfRaw, &r.SelfAssessmentPayload)
			}
			if len(managerRaw) > 0 {
				_ = json.Unmarshal(managerRaw, &r.ManagerEvalPayload)
			}
			out = append(out, r)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TransitionStatus performs an atomic conditional UPDATE.
// If the review is not in fromStatus, RowsAffected() == 0 → ErrInvalidStatusTransition.
// extra is optional additional SET clauses (key, value pairs appended to SET).
func (s *PgStore) TransitionStatus(ctx context.Context, id, fromStatus, toStatus string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE performance_reviews
			SET review_status = $1, updated_at = $2
			WHERE performance_review_id = $3 AND tenant_id = $4 AND review_status = $5
		`, toStatus, time.Now().UTC(), id, tenantID, fromStatus)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidStatusTransition
		}
		return nil
	})
}

// SubmitSelfAssessment atomically sets self_assessment_payload and advances status.
func (s *PgStore) SubmitSelfAssessment(ctx context.Context, id string, payload map[string]interface{}) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal self assessment payload: %w", err)
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE performance_reviews
			SET self_assessment_payload = $1, review_status = 'MANAGER_REVIEW_PENDING', updated_at = $2
			WHERE performance_review_id = $3 AND tenant_id = $4 AND review_status = 'SELF_ASSESSMENT_PENDING'
		`, raw, time.Now().UTC(), id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidStatusTransition
		}
		return nil
	})
}

// SubmitManagerEvaluation atomically sets manager_eval_payload + overall_rating and advances status.
func (s *PgStore) SubmitManagerEvaluation(ctx context.Context, id string, payload map[string]interface{}, rating float64) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal manager eval payload: %w", err)
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE performance_reviews
			SET manager_eval_payload = $1, overall_rating = $2,
			    review_status = 'SUBMITTED', updated_at = $3
			WHERE performance_review_id = $4 AND tenant_id = $5 AND review_status = 'MANAGER_REVIEW_PENDING'
		`, raw, rating, time.Now().UTC(), id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidStatusTransition
		}
		return nil
	})
}

// CompleteReview atomically transitions to COMPLETED, sets completed_at,
// and writes the governance_decision_id. Only allowed from APPROVED state.
func (s *PgStore) CompleteReview(ctx context.Context, id, governanceDecisionID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE performance_reviews
			SET review_status = 'COMPLETED', completed_at = $1,
			    governance_decision_id = $2, updated_at = $1
			WHERE performance_review_id = $3 AND tenant_id = $4 AND review_status = 'APPROVED'
		`, now, governanceDecisionID, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrInvalidStatusTransition
		}
		return nil
	})
}

// CancelReviewsByEmployee cancels all open reviews for an employee (on termination).
func (s *PgStore) CancelReviewsByEmployee(ctx context.Context, employeeID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE performance_reviews
			SET review_status = 'CANCELLED', updated_at = $1
			WHERE employee_id = $2 AND tenant_id = $3
			  AND review_status NOT IN ('COMPLETED', 'CANCELLED')
		`, time.Now().UTC(), employeeID, tenantID)
		return err
	})
}
