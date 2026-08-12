package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/performance-review-svc/internal/domain"
	svcmiddleware "zoiko.io/performance-review-svc/internal/middleware"
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

// ── review cycles ─────────────────────────────────────────────────────────────

const cycleColumns = `
	cycle_id, tenant_id, legal_entity_id, cycle_name, period_start::text, period_end::text,
	status, created_by_principal_id, correlation_id, created_at, updated_at, closed_at
`

func scanCycle(row pgx.Row, c *domain.ReviewCycle) error {
	var status string
	if err := row.Scan(
		&c.CycleID, &c.TenantID, &c.LegalEntityID, &c.CycleName, &c.PeriodStart, &c.PeriodEnd,
		&status, &c.CreatedByPrincipalID, &c.CorrelationID, &c.CreatedAt, &c.UpdatedAt, &c.ClosedAt,
	); err != nil {
		return err
	}
	c.Status = domain.CycleStatus(status)
	return nil
}

// CreateCycle inserts a new review cycle, idempotent on
// (tenant_id, correlation_id).
func (s *PgStore) CreateCycle(ctx context.Context, c *domain.ReviewCycle) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO review_cycles (
				cycle_id, tenant_id, legal_entity_id, cycle_name, period_start, period_end,
				status, created_by_principal_id, correlation_id, created_at, updated_at, closed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, c.CycleID, tenantID, c.LegalEntityID, c.CycleName, c.PeriodStart, c.PeriodEnd,
			string(c.Status), c.CreatedByPrincipalID, c.CorrelationID, c.CreatedAt, c.UpdatedAt, c.ClosedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}
		row := tx.QueryRow(ctx, "SELECT "+cycleColumns+" FROM review_cycles WHERE tenant_id = $1 AND correlation_id = $2", tenantID, c.CorrelationID)
		return scanCycle(row, c)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) GetCycle(ctx context.Context, cycleID string) (*domain.ReviewCycle, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var c domain.ReviewCycle
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+cycleColumns+" FROM review_cycles WHERE tenant_id = $1 AND cycle_id = $2", tenantID, cycleID)
		return scanCycle(row, &c)
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
		query := "SELECT " + cycleColumns + " FROM review_cycles WHERE tenant_id = $1"
		args := []any{tenantID}
		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.ReviewCycle
			if err := scanCycle(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CloseCycle transitions a cycle from OPEN to CLOSED, re-checking the
// current status inside the same transaction the update runs in.
func (s *PgStore) CloseCycle(ctx context.Context, cycleID string) (*domain.ReviewCycle, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out domain.ReviewCycle
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var current domain.ReviewCycle
		row := tx.QueryRow(ctx, "SELECT "+cycleColumns+" FROM review_cycles WHERE tenant_id = $1 AND cycle_id = $2 FOR UPDATE", tenantID, cycleID)
		if err := scanCycle(row, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCycleNotFound
			}
			return err
		}
		if current.Status != domain.CycleStatusOpen {
			return domain.ErrInvalidTransition
		}

		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
			UPDATE review_cycles SET status = $1, closed_at = $2, updated_at = $2
			WHERE tenant_id = $3 AND cycle_id = $4
		`, string(domain.CycleStatusClosed), now, tenantID, cycleID); err != nil {
			return err
		}

		row = tx.QueryRow(ctx, "SELECT "+cycleColumns+" FROM review_cycles WHERE tenant_id = $1 AND cycle_id = $2", tenantID, cycleID)
		return scanCycle(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── review records ────────────────────────────────────────────────────────────

const reviewColumns = `
	review_id, tenant_id, legal_entity_id, cycle_id, employee_id, reviewer_principal_id,
	rating, COALESCE(comments, ''), status, created_by_principal_id, correlation_id,
	created_at, updated_at, submitted_at, completed_at
`

func scanReview(row pgx.Row, r *domain.ReviewRecord) error {
	var status string
	if err := row.Scan(
		&r.ReviewID, &r.TenantID, &r.LegalEntityID, &r.CycleID, &r.EmployeeID, &r.ReviewerPrincipalID,
		&r.Rating, &r.Comments, &status, &r.CreatedByPrincipalID, &r.CorrelationID,
		&r.CreatedAt, &r.UpdatedAt, &r.SubmittedAt, &r.CompletedAt,
	); err != nil {
		return err
	}
	r.Status = domain.ReviewStatus(status)
	return nil
}

// CreateReview inserts a new review record, idempotent on
// (tenant_id, correlation_id).
func (s *PgStore) CreateReview(ctx context.Context, r *domain.ReviewRecord) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO review_records (
				review_id, tenant_id, legal_entity_id, cycle_id, employee_id, reviewer_principal_id,
				rating, comments, status, created_by_principal_id, correlation_id,
				created_at, updated_at, submitted_at, completed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, r.ReviewID, tenantID, r.LegalEntityID, r.CycleID, r.EmployeeID, r.ReviewerPrincipalID,
			r.Rating, r.Comments, string(r.Status), r.CreatedByPrincipalID, r.CorrelationID,
			r.CreatedAt, r.UpdatedAt, r.SubmittedAt, r.CompletedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}
		row := tx.QueryRow(ctx, "SELECT "+reviewColumns+" FROM review_records WHERE tenant_id = $1 AND correlation_id = $2", tenantID, r.CorrelationID)
		return scanReview(row, r)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) GetReview(ctx context.Context, reviewID string) (*domain.ReviewRecord, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var r domain.ReviewRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, "SELECT "+reviewColumns+" FROM review_records WHERE tenant_id = $1 AND review_id = $2", tenantID, reviewID)
		return scanReview(row, &r)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrReviewNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *PgStore) ListReviews(ctx context.Context, legalEntityID, cycleID, employeeID, status string) ([]domain.ReviewRecord, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.ReviewRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := "SELECT " + reviewColumns + " FROM review_records WHERE tenant_id = $1"
		args := []any{tenantID}
		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if cycleID != "" {
			args = append(args, cycleID)
			query += fmt.Sprintf(" AND cycle_id = $%d", len(args))
		}
		if employeeID != "" {
			args = append(args, employeeID)
			query += fmt.Sprintf(" AND employee_id = $%d", len(args))
		}
		if status != "" {
			args = append(args, status)
			query += fmt.Sprintf(" AND status = $%d", len(args))
		}
		query += " ORDER BY created_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.ReviewRecord
			if err := scanReview(rows, &r); err != nil {
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

// SubmitReview transitions a review from DRAFT to SUBMITTED, recording the
// reviewer's rating and comments.
func (s *PgStore) SubmitReview(ctx context.Context, reviewID string, rating int, comments string) (*domain.ReviewRecord, error) {
	return s.transition(ctx, reviewID, domain.ReviewStatusDraft, func(tx pgx.Tx, tenantID string, now time.Time) error {
		_, err := tx.Exec(ctx, `
			UPDATE review_records
			SET status = $1, rating = $2, comments = $3, submitted_at = $4, updated_at = $4
			WHERE tenant_id = $5 AND review_id = $6
		`, string(domain.ReviewStatusSubmitted), rating, comments, now, tenantID, reviewID)
		return err
	})
}

// CompleteReview transitions a review from SUBMITTED to COMPLETED.
func (s *PgStore) CompleteReview(ctx context.Context, reviewID string) (*domain.ReviewRecord, error) {
	return s.transition(ctx, reviewID, domain.ReviewStatusSubmitted, func(tx pgx.Tx, tenantID string, now time.Time) error {
		_, err := tx.Exec(ctx, `
			UPDATE review_records SET status = $1, completed_at = $2, updated_at = $2
			WHERE tenant_id = $3 AND review_id = $4
		`, string(domain.ReviewStatusCompleted), now, tenantID, reviewID)
		return err
	})
}

// transition fetches the review, verifies it is currently in
// requiredStatus, runs the caller's UPDATE, and re-fetches the final row —
// all inside one RLS-scoped transaction so the check-then-act is atomic.
func (s *PgStore) transition(ctx context.Context, reviewID string, requiredStatus domain.ReviewStatus, update func(tx pgx.Tx, tenantID string, now time.Time) error) (*domain.ReviewRecord, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out domain.ReviewRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var current domain.ReviewRecord
		row := tx.QueryRow(ctx, "SELECT "+reviewColumns+" FROM review_records WHERE tenant_id = $1 AND review_id = $2 FOR UPDATE", tenantID, reviewID)
		if err := scanReview(row, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrReviewNotFound
			}
			return err
		}
		if current.Status != requiredStatus {
			return domain.ErrInvalidTransition
		}

		if err := update(tx, tenantID, time.Now().UTC()); err != nil {
			return err
		}

		row = tx.QueryRow(ctx, "SELECT "+reviewColumns+" FROM review_records WHERE tenant_id = $1 AND review_id = $2", tenantID, reviewID)
		return scanReview(row, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
