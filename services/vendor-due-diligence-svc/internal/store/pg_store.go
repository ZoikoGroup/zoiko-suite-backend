package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"zoiko.io/vendor-due-diligence-svc/internal/domain"
	svcmiddleware "zoiko.io/vendor-due-diligence-svc/internal/middleware"
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

// CreateCheck inserts a new check in STARTED status, idempotent on
// (tenant_id, correlation_id): a retry finds the existing row instead of
// starting a second check for the same request.
func (s *PgStore) CreateCheck(ctx context.Context, c *domain.VendorDDCheck) (created bool, err error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return false, domain.ErrIdentityMissing
	}

	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO vendor_dd_checks (
				check_id, tenant_id, legal_entity_id, counterparty_id, vendor_name,
				status, correlation_id, initiated_by_principal_id, started_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT (tenant_id, correlation_id) DO NOTHING
		`, c.CheckID, tenantID, c.LegalEntityID, c.CounterpartyID, c.VendorName,
			c.Status, c.CorrelationID, c.InitiatedByPrincipalID, c.StartedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			created = true
			return nil
		}

		// Conflict: a check for this (tenant_id, correlation_id) already
		// exists — fetch its current state so the caller returns the
		// existing check rather than a second, divergent one.
		row := tx.QueryRow(ctx, `
			SELECT check_id, legal_entity_id, counterparty_id, vendor_name, status,
			       COALESCE(risk_outcome, ''), COALESCE(screening_basis, ''),
			       initiated_by_principal_id, started_at, completed_at
			FROM vendor_dd_checks WHERE tenant_id = $1 AND correlation_id = $2
		`, tenantID, c.CorrelationID)
		return row.Scan(
			&c.CheckID, &c.LegalEntityID, &c.CounterpartyID, &c.VendorName, &c.Status,
			&c.RiskOutcome, &c.ScreeningBasis,
			&c.InitiatedByPrincipalID, &c.StartedAt, &c.CompletedAt,
		)
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *PgStore) GetCheck(ctx context.Context, id string) (*domain.VendorDDCheck, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var c domain.VendorDDCheck
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT check_id, tenant_id, legal_entity_id, counterparty_id, vendor_name, status,
			       COALESCE(risk_outcome, ''), COALESCE(screening_basis, ''), correlation_id,
			       initiated_by_principal_id, started_at, completed_at
			FROM vendor_dd_checks
			WHERE check_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&c.CheckID, &c.TenantID, &c.LegalEntityID, &c.CounterpartyID, &c.VendorName, &c.Status,
			&c.RiskOutcome, &c.ScreeningBasis, &c.CorrelationID,
			&c.InitiatedByPrincipalID, &c.StartedAt, &c.CompletedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCheckNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) ListChecks(ctx context.Context, legalEntityID, counterpartyID string) ([]domain.VendorDDCheck, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.VendorDDCheck
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT check_id, tenant_id, legal_entity_id, counterparty_id, vendor_name, status,
			       COALESCE(risk_outcome, ''), COALESCE(screening_basis, ''), correlation_id,
			       initiated_by_principal_id, started_at, completed_at
			FROM vendor_dd_checks
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
		}
		if counterpartyID != "" {
			args = append(args, counterpartyID)
			query += fmt.Sprintf(" AND counterparty_id = $%d", len(args))
		}
		query += " ORDER BY started_at DESC"

		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.VendorDDCheck
			if err := rows.Scan(
				&c.CheckID, &c.TenantID, &c.LegalEntityID, &c.CounterpartyID, &c.VendorName, &c.Status,
				&c.RiskOutcome, &c.ScreeningBasis, &c.CorrelationID,
				&c.InitiatedByPrincipalID, &c.StartedAt, &c.CompletedAt,
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

// CompleteCheck records the final status, risk outcome, and basis, setting
// completed_at. newStatus is COMPLETED or FAILED per the caller's decision.
func (s *PgStore) CompleteCheck(ctx context.Context, id, newStatus, riskOutcome, screeningBasis string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		res, err := tx.Exec(ctx, `
			UPDATE vendor_dd_checks
			SET status = $1, risk_outcome = $2, screening_basis = $3, completed_at = $4
			WHERE check_id = $5 AND tenant_id = $6
		`, newStatus, riskOutcome, screeningBasis, now, id, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrCheckNotFound
		}
		return nil
	})
}

func (s *PgStore) AddEvidence(ctx context.Context, e *domain.VendorDDEvidence) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO vendor_dd_evidence (
				evidence_id, check_id, tenant_id, evidence_type, description,
				document_reference, recorded_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, e.EvidenceID, e.CheckID, tenantID, e.EvidenceType, e.Description,
			e.DocumentReference, e.RecordedAt)
		return err
	})
}

func (s *PgStore) ListEvidence(ctx context.Context, checkID string) ([]domain.VendorDDEvidence, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.VendorDDEvidence
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT evidence_id, check_id, tenant_id, evidence_type, description,
			       COALESCE(document_reference, ''), recorded_at
			FROM vendor_dd_evidence
			WHERE check_id = $1 AND tenant_id = $2
			ORDER BY recorded_at ASC
		`, checkID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var e domain.VendorDDEvidence
			if err := rows.Scan(
				&e.EvidenceID, &e.CheckID, &e.TenantID, &e.EvidenceType, &e.Description,
				&e.DocumentReference, &e.RecordedAt,
			); err != nil {
				return err
			}
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
