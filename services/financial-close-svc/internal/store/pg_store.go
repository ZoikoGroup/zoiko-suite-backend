package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

type PgStore struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// withRLS runs a query block under the specified tenant RLS context.
func (s *PgStore) withRLS(ctx context.Context, tenantID string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// Inject tenant context into the session configuration
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

func (s *PgStore) CreateFiscalPeriod(ctx context.Context, fp *domain.FiscalPeriod) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO fiscal_periods (
				fiscal_period_id, tenant_id, legal_entity_id, period_name, period_start, period_end, close_status
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, fp.FiscalPeriodID, tenantID, fp.LegalEntityID, fp.PeriodName, fp.PeriodStart, fp.PeriodEnd, fp.CloseStatus)
		return err
	})
}

func (s *PgStore) GetFiscalPeriod(ctx context.Context, id string) (*domain.FiscalPeriod, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var fp domain.FiscalPeriod
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT fiscal_period_id, tenant_id, legal_entity_id, period_name, period_start, period_end, close_status, close_locked_at, evidence_document_id
			FROM fiscal_periods WHERE fiscal_period_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&fp.FiscalPeriodID, &fp.TenantID, &fp.LegalEntityID, &fp.PeriodName, &fp.PeriodStart, &fp.PeriodEnd, &fp.CloseStatus, &fp.CloseLockedAt, &fp.EvidenceDocumentID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFiscalPeriodNotFound
	}
	if err != nil {
		return nil, err
	}
	return &fp, nil
}

func (s *PgStore) GetFiscalPeriodByName(ctx context.Context, legalEntityID, name string) (*domain.FiscalPeriod, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var fp domain.FiscalPeriod
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT fiscal_period_id, tenant_id, legal_entity_id, period_name, period_start, period_end, close_status, close_locked_at, evidence_document_id
			FROM fiscal_periods WHERE legal_entity_id = $1 AND period_name = $2 AND tenant_id = $3
		`, legalEntityID, name, tenantID).Scan(
			&fp.FiscalPeriodID, &fp.TenantID, &fp.LegalEntityID, &fp.PeriodName, &fp.PeriodStart, &fp.PeriodEnd, &fp.CloseStatus, &fp.CloseLockedAt, &fp.EvidenceDocumentID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrFiscalPeriodNotFound
	}
	if err != nil {
		return nil, err
	}
	return &fp, nil
}

func (s *PgStore) ListFiscalPeriods(ctx context.Context, legalEntityID string) ([]domain.FiscalPeriod, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.FiscalPeriod
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT fiscal_period_id, tenant_id, legal_entity_id, period_name, period_start, period_end, close_status, close_locked_at, evidence_document_id
			FROM fiscal_periods WHERE legal_entity_id = $1 AND tenant_id = $2
			ORDER BY period_start DESC
		`, legalEntityID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var fp domain.FiscalPeriod
			if err := rows.Scan(
				&fp.FiscalPeriodID, &fp.TenantID, &fp.LegalEntityID, &fp.PeriodName, &fp.PeriodStart, &fp.PeriodEnd, &fp.CloseStatus, &fp.CloseLockedAt, &fp.EvidenceDocumentID,
			); err != nil {
				return err
			}
			out = append(out, fp)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *PgStore) LockFiscalPeriod(ctx context.Context, id string, lockedAt time.Time, evidenceDocID string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE fiscal_periods
			SET close_status = 'LOCKED', close_locked_at = $1, evidence_document_id = $2
			WHERE fiscal_period_id = $3 AND tenant_id = $4 AND close_status = 'OPEN'
		`, lockedAt, evidenceDocID, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrPeriodAlreadyLocked
		}
		return nil
	})
}

func (s *PgStore) CreateCloseEvidence(ctx context.Context, evidence *domain.CloseEvidence) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO close_evidences (
				evidence_id, tenant_id, fiscal_period_id, trial_balance_hash, signature, generated_at
			) VALUES ($1, $2, $3, $4, $5, $6)
		`, evidence.EvidenceID, tenantID, evidence.FiscalPeriodID, evidence.TrialBalanceHash, evidence.Signature, evidence.GeneratedAt)
		return err
	})
}
