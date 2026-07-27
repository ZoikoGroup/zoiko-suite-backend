package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/employment-contracts-svc/internal/domain"
	svcmiddleware "zoiko.io/employment-contracts-svc/internal/middleware"
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

func (s *PgStore) IssueContract(ctx context.Context, c *domain.EmploymentContract) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO employment_contracts (
				contract_id, tenant_id, legal_entity_id, employee_id, contract_number,
				version, contract_type, status, title, base_salary_amount, currency,
				pay_frequency, effective_from, effective_to, document_vault_ref, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		`, c.ContractID, tenantID, c.LegalEntityID, c.EmployeeID, c.ContractNumber,
			c.Version, c.ContractType, c.Status, c.Title, c.BaseSalaryAmount, c.Currency,
			c.PayFrequency, c.EffectiveFrom, c.EffectiveTo, c.DocumentVaultRef, c.CreatedAt, c.UpdatedAt)
		return err
	})
}

func (s *PgStore) GetContract(ctx context.Context, id string) (*domain.EmploymentContract, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var c domain.EmploymentContract
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT contract_id, tenant_id, legal_entity_id, employee_id, contract_number,
			       version, contract_type, status, title, base_salary_amount, currency,
			       pay_frequency, effective_from, effective_to, document_vault_ref, created_at, updated_at
			FROM employment_contracts
			WHERE contract_id = $1 AND tenant_id = $2
		`, id, tenantID).Scan(
			&c.ContractID, &c.TenantID, &c.LegalEntityID, &c.EmployeeID, &c.ContractNumber,
			&c.Version, &c.ContractType, &c.Status, &c.Title, &c.BaseSalaryAmount, &c.Currency,
			&c.PayFrequency, &c.EffectiveFrom, &c.EffectiveTo, &c.DocumentVaultRef, &c.CreatedAt, &c.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrContractNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) GetActiveContractByEmployee(ctx context.Context, employeeID string) (*domain.EmploymentContract, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var c domain.EmploymentContract
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT contract_id, tenant_id, legal_entity_id, employee_id, contract_number,
			       version, contract_type, status, title, base_salary_amount, currency,
			       pay_frequency, effective_from, effective_to, document_vault_ref, created_at, updated_at
			FROM employment_contracts
			WHERE tenant_id = $1 AND employee_id = $2 AND status = 'ACTIVE'
			ORDER BY version DESC LIMIT 1
		`, tenantID, employeeID).Scan(
			&c.ContractID, &c.TenantID, &c.LegalEntityID, &c.EmployeeID, &c.ContractNumber,
			&c.Version, &c.ContractType, &c.Status, &c.Title, &c.BaseSalaryAmount, &c.Currency,
			&c.PayFrequency, &c.EffectiveFrom, &c.EffectiveTo, &c.DocumentVaultRef, &c.CreatedAt, &c.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrContractNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *PgStore) ListContracts(ctx context.Context, legalEntityID, employeeID, status string) ([]domain.EmploymentContract, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.EmploymentContract
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		query := `
			SELECT contract_id, tenant_id, legal_entity_id, employee_id, contract_number,
			       version, contract_type, status, title, base_salary_amount, currency,
			       pay_frequency, effective_from, effective_to, document_vault_ref, created_at, updated_at
			FROM employment_contracts
			WHERE tenant_id = $1
		`
		args := []any{tenantID}

		if legalEntityID != "" {
			args = append(args, legalEntityID)
			query += fmt.Sprintf(" AND legal_entity_id = $%d", len(args))
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
			var c domain.EmploymentContract
			if err := rows.Scan(
				&c.ContractID, &c.TenantID, &c.LegalEntityID, &c.EmployeeID, &c.ContractNumber,
				&c.Version, &c.ContractType, &c.Status, &c.Title, &c.BaseSalaryAmount, &c.Currency,
				&c.PayFrequency, &c.EffectiveFrom, &c.EffectiveTo, &c.DocumentVaultRef, &c.CreatedAt, &c.UpdatedAt,
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

func (s *PgStore) GetContractVersionHistory(ctx context.Context, contractNumber string) ([]domain.EmploymentContract, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return nil, domain.ErrIdentityMissing
	}

	var out []domain.EmploymentContract
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT contract_id, tenant_id, legal_entity_id, employee_id, contract_number,
			       version, contract_type, status, title, base_salary_amount, currency,
			       pay_frequency, effective_from, effective_to, document_vault_ref, created_at, updated_at
			FROM employment_contracts
			WHERE tenant_id = $1 AND contract_number = $2
			ORDER BY version ASC
		`, tenantID, contractNumber)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c domain.EmploymentContract
			if err := rows.Scan(
				&c.ContractID, &c.TenantID, &c.LegalEntityID, &c.EmployeeID, &c.ContractNumber,
				&c.Version, &c.ContractType, &c.Status, &c.Title, &c.BaseSalaryAmount, &c.Currency,
				&c.PayFrequency, &c.EffectiveFrom, &c.EffectiveTo, &c.DocumentVaultRef, &c.CreatedAt, &c.UpdatedAt,
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

func (s *PgStore) AmendContract(ctx context.Context, oldContractID string, newContract *domain.EmploymentContract, amd *domain.ContractAmendment) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		// 1. Mark prior version SUPERSEDED and set effective_to
		res, err := tx.Exec(ctx, `
			UPDATE employment_contracts
			SET status = 'SUPERSEDED', effective_to = $1, updated_at = $2
			WHERE contract_id = $3 AND tenant_id = $4 AND status = 'ACTIVE'
		`, amd.EffectiveFrom, time.Now().UTC(), oldContractID, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrContractAlreadyTerminated
		}

		// 2. Insert new contract version
		_, err = tx.Exec(ctx, `
			INSERT INTO employment_contracts (
				contract_id, tenant_id, legal_entity_id, employee_id, contract_number,
				version, contract_type, status, title, base_salary_amount, currency,
				pay_frequency, effective_from, effective_to, document_vault_ref, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		`, newContract.ContractID, tenantID, newContract.LegalEntityID, newContract.EmployeeID, newContract.ContractNumber,
			newContract.Version, newContract.ContractType, newContract.Status, newContract.Title, newContract.BaseSalaryAmount, newContract.Currency,
			newContract.PayFrequency, newContract.EffectiveFrom, newContract.EffectiveTo, newContract.DocumentVaultRef, newContract.CreatedAt, newContract.UpdatedAt)
		if err != nil {
			return err
		}

		// 3. Insert amendment log record
		_, err = tx.Exec(ctx, `
			INSERT INTO contract_amendments (
				amendment_id, tenant_id, contract_id, from_version, to_version,
				amendment_reason, amended_by, effective_from, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`, amd.AmendmentID, tenantID, newContract.ContractID, amd.FromVersion, amd.ToVersion,
			amd.AmendmentReason, amd.AmendedBy, amd.EffectiveFrom, amd.CreatedAt)
		return err
	})
}

func (s *PgStore) TerminateContract(ctx context.Context, contractID, terminationDate string) error {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" {
		return domain.ErrIdentityMissing
	}

	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx, `
			UPDATE employment_contracts
			SET status = 'TERMINATED', effective_to = $1, updated_at = $2
			WHERE contract_id = $3 AND tenant_id = $4 AND status IN ('ACTIVE', 'DRAFT')
		`, terminationDate, time.Now().UTC(), contractID, tenantID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return domain.ErrContractAlreadyTerminated
		}
		return nil
	})
}