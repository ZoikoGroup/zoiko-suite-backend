package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/counterparty-management-svc/internal/domain"
	"zoiko.io/counterparty-management-svc/internal/middleware"
)

type Store interface {
	CreateCounterparty(ctx context.Context, c *domain.Counterparty) error
	GetCounterparty(ctx context.Context, id string) (*domain.Counterparty, error)
	ListCounterparties(ctx context.Context, legalEntityID, counterpartyType, status string) ([]domain.Counterparty, error)
	UpdateCounterparty(ctx context.Context, c *domain.Counterparty) error
	UpdateComplianceStatus(ctx context.Context, id, complianceStatus string) (*domain.Counterparty, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID))
	return err
}

func (s *PgStore) CreateCounterparty(ctx context.Context, c *domain.Counterparty) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if c.CounterpartyID == "" {
		c.CounterpartyID = "cpty-" + uuid.New().String()
	}
	c.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = domain.StatusOnboarding
	}
	if c.RiskCategory == "" {
		c.RiskCategory = domain.RiskMedium
	}
	if c.ComplianceStatus == "" {
		c.ComplianceStatus = "PENDING"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO counterparties
			(counterparty_id, tenant_id, legal_entity_id, name, counterparty_type, registration_number,
			 tax_id, jurisdiction_id, risk_category, status, contact_email, phone, address, compliance_status,
			 effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		c.CounterpartyID, c.TenantID, c.LegalEntityID, c.Name, string(c.CounterpartyType), c.RegistrationNumber,
		c.TaxID, c.JurisdictionID, string(c.RiskCategory), string(c.Status), c.ContactEmail, c.Phone, c.Address,
		c.ComplianceStatus, c.EffectiveFrom, c.EffectiveTo, c.CreatedBy, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert counterparty: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetCounterparty(ctx context.Context, id string) (*domain.Counterparty, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var c domain.Counterparty
	var ctype, risk, status string
	err = tx.QueryRow(ctx, `
		SELECT counterparty_id, tenant_id, legal_entity_id, name, counterparty_type, COALESCE(registration_number,''),
		       COALESCE(tax_id,''), jurisdiction_id, risk_category, status, COALESCE(contact_email,''),
		       COALESCE(phone,''), COALESCE(address,''), compliance_status, effective_from, effective_to,
		       created_by, created_at, updated_at
		FROM counterparties WHERE counterparty_id = $1`, id,
	).Scan(
		&c.CounterpartyID, &c.TenantID, &c.LegalEntityID, &c.Name, &ctype, &c.RegistrationNumber,
		&c.TaxID, &c.JurisdictionID, &risk, &status, &c.ContactEmail,
		&c.Phone, &c.Address, &c.ComplianceStatus, &c.EffectiveFrom, &c.EffectiveTo,
		&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrCounterpartyNotFound
		}
		return nil, err
	}
	c.CounterpartyType = domain.CounterpartyType(ctype)
	c.RiskCategory = domain.RiskCategory(risk)
	c.Status = domain.CounterpartyStatus(status)
	_ = tx.Commit(ctx)
	return &c, nil
}

func (s *PgStore) ListCounterparties(ctx context.Context, legalEntityID, counterpartyType, status string) ([]domain.Counterparty, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT counterparty_id, tenant_id, legal_entity_id, name, counterparty_type, COALESCE(registration_number,''),
		       COALESCE(tax_id,''), jurisdiction_id, risk_category, status, COALESCE(contact_email,''),
		       COALESCE(phone,''), COALESCE(address,''), compliance_status, effective_from, effective_to,
		       created_by, created_at, updated_at
		FROM counterparties
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR counterparty_type = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC`, legalEntityID, counterpartyType, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Counterparty
	for rows.Next() {
		var c domain.Counterparty
		var ctype, risk, stat string
		if err := rows.Scan(
			&c.CounterpartyID, &c.TenantID, &c.LegalEntityID, &c.Name, &ctype, &c.RegistrationNumber,
			&c.TaxID, &c.JurisdictionID, &risk, &stat, &c.ContactEmail,
			&c.Phone, &c.Address, &c.ComplianceStatus, &c.EffectiveFrom, &c.EffectiveTo,
			&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		c.CounterpartyType = domain.CounterpartyType(ctype)
		c.RiskCategory = domain.RiskCategory(risk)
		c.Status = domain.CounterpartyStatus(stat)
		out = append(out, c)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateCounterparty(ctx context.Context, c *domain.Counterparty) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	c.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE counterparties
		SET name=$1, counterparty_type=$2, registration_number=$3, tax_id=$4, jurisdiction_id=$5,
		    risk_category=$6, status=$7, contact_email=$8, phone=$9, address=$10, effective_to=$11, updated_at=$12
		WHERE counterparty_id=$13`,
		c.Name, string(c.CounterpartyType), c.RegistrationNumber, c.TaxID, c.JurisdictionID,
		string(c.RiskCategory), string(c.Status), c.ContactEmail, c.Phone, c.Address, c.EffectiveTo, c.UpdatedAt, c.CounterpartyID,
	)
	if err != nil {
		return fmt.Errorf("update counterparty: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) UpdateComplianceStatus(ctx context.Context, id, complianceStatus string) (*domain.Counterparty, error) {
	c, err := s.GetCounterparty(ctx, id)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	c.ComplianceStatus = complianceStatus
	c.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE counterparties
		SET compliance_status=$1, updated_at=$2
		WHERE counterparty_id=$3`,
		c.ComplianceStatus, c.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
