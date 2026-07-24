package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/contract-lifecycle-svc/internal/domain"
	"zoiko.io/contract-lifecycle-svc/internal/middleware"
)

// Store defines all persistence operations for the contract lifecycle service.
type Store interface {
	CreateContract(ctx context.Context, c *domain.Contract) error
	GetContract(ctx context.Context, id string) (*domain.Contract, error)
	ListContracts(ctx context.Context, legalEntityID string) ([]domain.Contract, error)
	UpdateContract(ctx context.Context, c *domain.Contract, changeSummary string) error
	UpdateContractStatus(ctx context.Context, id string, status domain.ContractStatus, by string) error
	ActivateContract(ctx context.Context, id string, req *domain.ActivateContractRequest) (*domain.Contract, error)
	TerminateContract(ctx context.Context, id string, req *domain.TerminateContractRequest) (*domain.Contract, error)
	ListContractVersions(ctx context.Context, contractID string) ([]domain.ContractVersion, error)
}

// PgStore implements Store using a PostgreSQL connection pool with RLS.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore creates a new PgStore instance.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// setRLS sets the RLS tenant context variable for the current transaction.
func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL app.tenant_id = '%s'", tenantID))
	return err
}

// snapshotVersion creates an immutable version record for audit lineage.
func (s *PgStore) snapshotVersion(ctx context.Context, tx pgx.Tx, c *domain.Contract, summary string) error {
	v := &domain.ContractVersion{
		VersionID:     "cv-" + uuid.New().String(),
		ContractID:    c.ContractID,
		TenantID:      c.TenantID,
		VersionNumber: c.Version,
		Status:        c.Status,
		Title:         c.Title,
		Description:   c.Description,
		EffectiveFrom: c.EffectiveFrom,
		EffectiveTo:   c.EffectiveTo,
		ChangeSummary: summary,
		CreatedBy:     c.CreatedBy,
		CreatedAt:     time.Now().UTC(),
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO contract_versions
			(version_id, contract_id, tenant_id, version_number, status, title, description,
			 effective_from, effective_to, change_summary, created_by, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		v.VersionID, v.ContractID, v.TenantID, v.VersionNumber, string(v.Status), v.Title,
		v.Description, v.EffectiveFrom, v.EffectiveTo, v.ChangeSummary, v.CreatedBy, v.CreatedAt,
	)
	return err
}

// CreateContract inserts a new contract in DRAFT status.
func (s *PgStore) CreateContract(ctx context.Context, c *domain.Contract) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if c.ContractID == "" {
		c.ContractID = "ctr-" + uuid.New().String()
	}
	c.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = domain.ContractStatusDraft
	}
	c.Version = 1

	_, err = tx.Exec(ctx, `
		INSERT INTO contracts
			(contract_id, tenant_id, legal_entity_id, contract_type, title, description,
			 counterparty_id, counterparty_name, status, version, effective_from, effective_to,
			 currency, total_value, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		c.ContractID, c.TenantID, c.LegalEntityID, string(c.ContractType), c.Title, c.Description,
		c.CounterpartyID, c.CounterpartyName, string(c.Status), c.Version, c.EffectiveFrom, c.EffectiveTo,
		c.Currency, c.TotalValue, c.CreatedBy, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert contract: %w", err)
	}

	if err := s.snapshotVersion(ctx, tx, c, "Initial draft"); err != nil {
		return fmt.Errorf("snapshot version: %w", err)
	}

	return tx.Commit(ctx)
}

// GetContract retrieves a single contract by ID.
func (s *PgStore) GetContract(ctx context.Context, id string) (*domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var c domain.Contract
	var ctype, status string
	err = tx.QueryRow(ctx, `
		SELECT contract_id, tenant_id, legal_entity_id, contract_type, title,
		       COALESCE(description,''), counterparty_id, counterparty_name, status, version,
		       effective_from, effective_to, signed_at, signed_by,
		       terminated_at, terminated_by, termination_note,
		       currency, total_value, document_vault_id, created_by, created_at, updated_at
		FROM contracts WHERE contract_id = $1`, id,
	).Scan(
		&c.ContractID, &c.TenantID, &c.LegalEntityID, &ctype, &c.Title,
		&c.Description, &c.CounterpartyID, &c.CounterpartyName, &status, &c.Version,
		&c.EffectiveFrom, &c.EffectiveTo, &c.SignedAt, &c.SignedBy,
		&c.TerminatedAt, &c.TerminatedBy, &c.TerminationNote,
		&c.Currency, &c.TotalValue, &c.DocumentVaultID, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrContractNotFound
		}
		return nil, err
	}
	c.ContractType = domain.ContractType(ctype)
	c.Status = domain.ContractStatus(status)
	_ = tx.Commit(ctx)
	return &c, nil
}

// ListContracts returns all contracts for the tenant, optionally filtered by legal entity.
func (s *PgStore) ListContracts(ctx context.Context, legalEntityID string) ([]domain.Contract, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT contract_id, tenant_id, legal_entity_id, contract_type, title,
		       COALESCE(description,''), counterparty_id, counterparty_name, status, version,
		       effective_from, effective_to, signed_at, signed_by,
		       terminated_at, terminated_by, termination_note,
		       currency, total_value, document_vault_id, created_by, created_at, updated_at
		FROM contracts
		WHERE ($1 = '' OR legal_entity_id = $1)
		ORDER BY created_at DESC`, legalEntityID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Contract
	for rows.Next() {
		var c domain.Contract
		var ctype, status string
		if err := rows.Scan(
			&c.ContractID, &c.TenantID, &c.LegalEntityID, &ctype, &c.Title,
			&c.Description, &c.CounterpartyID, &c.CounterpartyName, &status, &c.Version,
			&c.EffectiveFrom, &c.EffectiveTo, &c.SignedAt, &c.SignedBy,
			&c.TerminatedAt, &c.TerminatedBy, &c.TerminationNote,
			&c.Currency, &c.TotalValue, &c.DocumentVaultID, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		c.ContractType = domain.ContractType(ctype)
		c.Status = domain.ContractStatus(status)
		out = append(out, c)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

// UpdateContract increments the version, persists changes, and snapshots the version.
func (s *PgStore) UpdateContract(ctx context.Context, c *domain.Contract, changeSummary string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	c.Version++
	c.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE contracts
		SET title=$1, description=$2, counterparty_name=$3, effective_to=$4,
		    currency=$5, total_value=$6, version=$7, updated_at=$8
		WHERE contract_id=$9`,
		c.Title, c.Description, c.CounterpartyName, c.EffectiveTo,
		c.Currency, c.TotalValue, c.Version, c.UpdatedAt, c.ContractID,
	)
	if err != nil {
		return fmt.Errorf("update contract: %w", err)
	}

	if err := s.snapshotVersion(ctx, tx, c, changeSummary); err != nil {
		return fmt.Errorf("snapshot version: %w", err)
	}

	return tx.Commit(ctx)
}

// UpdateContractStatus transitions the contract status.
func (s *PgStore) UpdateContractStatus(ctx context.Context, id string, status domain.ContractStatus, by string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	now := time.Now().UTC()
	_, err = tx.Exec(ctx, `
		UPDATE contracts SET status=$1, updated_at=$2 WHERE contract_id=$3`,
		string(status), now, id,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ActivateContract marks a contract ACTIVE and records signing metadata.
func (s *PgStore) ActivateContract(ctx context.Context, id string, req *domain.ActivateContractRequest) (*domain.Contract, error) {
	c, err := s.GetContract(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Status == domain.ContractStatusActive {
		return nil, domain.ErrContractAlreadyActive
	}
	if c.Status == domain.ContractStatusTerminated {
		return nil, domain.ErrContractTerminated
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	c.Status = domain.ContractStatusActive
	c.SignedBy = &req.SignedBy
	c.SignedAt = &req.SignedAt
	c.DocumentVaultID = req.DocumentVaultID
	c.UpdatedAt = now
	c.Version++

	_, err = tx.Exec(ctx, `
		UPDATE contracts
		SET status=$1, signed_by=$2, signed_at=$3, document_vault_id=$4, version=$5, updated_at=$6
		WHERE contract_id=$7`,
		string(c.Status), c.SignedBy, c.SignedAt, c.DocumentVaultID, c.Version, c.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := s.snapshotVersion(ctx, tx, c, "Contract activated and signed"); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// TerminateContract marks a contract TERMINATED and records termination metadata.
func (s *PgStore) TerminateContract(ctx context.Context, id string, req *domain.TerminateContractRequest) (*domain.Contract, error) {
	c, err := s.GetContract(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Status == domain.ContractStatusTerminated {
		return nil, domain.ErrContractTerminated
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	todayStr := now.Format("2006-01-02")
	c.Status = domain.ContractStatusTerminated
	c.TerminatedBy = &req.TerminatedBy
	c.TerminatedAt = &now
	c.TerminationNote = &req.TerminationNote
	c.EffectiveTo = &todayStr
	c.UpdatedAt = now
	c.Version++

	_, err = tx.Exec(ctx, `
		UPDATE contracts
		SET status=$1, terminated_by=$2, terminated_at=$3, termination_note=$4,
		    effective_to=$5, version=$6, updated_at=$7
		WHERE contract_id=$8`,
		string(c.Status), c.TerminatedBy, c.TerminatedAt, c.TerminationNote,
		c.EffectiveTo, c.Version, c.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := s.snapshotVersion(ctx, tx, c, "Contract terminated: "+req.TerminationNote); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ListContractVersions returns the immutable version history of a contract.
func (s *PgStore) ListContractVersions(ctx context.Context, contractID string) ([]domain.ContractVersion, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT version_id, contract_id, tenant_id, version_number, status, title,
		       COALESCE(description,''), effective_from, effective_to, change_summary, created_by, created_at
		FROM contract_versions WHERE contract_id=$1 ORDER BY version_number ASC`, contractID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ContractVersion
	for rows.Next() {
		var v domain.ContractVersion
		var status string
		if err := rows.Scan(
			&v.VersionID, &v.ContractID, &v.TenantID, &v.VersionNumber, &status, &v.Title,
			&v.Description, &v.EffectiveFrom, &v.EffectiveTo, &v.ChangeSummary, &v.CreatedBy, &v.CreatedAt,
		); err != nil {
			return nil, err
		}
		v.Status = domain.ContractStatus(status)
		out = append(out, v)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
