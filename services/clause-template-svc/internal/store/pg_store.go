package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/clause-template-svc/internal/domain"
	"zoiko.io/clause-template-svc/internal/middleware"
)

type Store interface {
	CreateClause(ctx context.Context, c *domain.Clause) error
	GetClause(ctx context.Context, id string) (*domain.Clause, error)
	ListClauses(ctx context.Context, legalEntityID, category string) ([]domain.Clause, error)
	UpdateClause(ctx context.Context, c *domain.Clause) error

	CreateTemplate(ctx context.Context, t *domain.ContractTemplate) error
	GetTemplate(ctx context.Context, id string) (*domain.ContractTemplate, error)
	ListTemplates(ctx context.Context, legalEntityID, contractType string) ([]domain.ContractTemplate, error)
	UpdateTemplate(ctx context.Context, t *domain.ContractTemplate) error
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

func (s *PgStore) CreateClause(ctx context.Context, c *domain.Clause) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if c.ClauseID == "" {
		c.ClauseID = "cls-" + uuid.New().String()
	}
	c.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = domain.StatusDraft
	}
	c.Version = 1

	_, err = tx.Exec(ctx, `
		INSERT INTO clauses
			(clause_id, tenant_id, legal_entity_id, title, category, body, status, version,
			 jurisdiction_id, effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		c.ClauseID, c.TenantID, c.LegalEntityID, c.Title, string(c.Category), c.Body,
		string(c.Status), c.Version, c.JurisdictionID, c.EffectiveFrom, c.EffectiveTo,
		c.CreatedBy, c.CreatedAt, c.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert clause: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetClause(ctx context.Context, id string) (*domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var c domain.Clause
	var category, status string
	err = tx.QueryRow(ctx, `
		SELECT clause_id, tenant_id, legal_entity_id, title, category, body, status, version,
		       jurisdiction_id, effective_from, effective_to, created_by, created_at, updated_at
		FROM clauses WHERE clause_id = $1`, id,
	).Scan(
		&c.ClauseID, &c.TenantID, &c.LegalEntityID, &c.Title, &category, &c.Body, &status, &c.Version,
		&c.JurisdictionID, &c.EffectiveFrom, &c.EffectiveTo, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrClauseNotFound
		}
		return nil, err
	}
	c.Category = domain.ClauseCategory(category)
	c.Status = domain.Status(status)
	_ = tx.Commit(ctx)
	return &c, nil
}

func (s *PgStore) ListClauses(ctx context.Context, legalEntityID, category string) ([]domain.Clause, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT clause_id, tenant_id, legal_entity_id, title, category, body, status, version,
		       jurisdiction_id, effective_from, effective_to, created_by, created_at, updated_at
		FROM clauses
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR category = $2)
		ORDER BY created_at DESC`, legalEntityID, category,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Clause
	for rows.Next() {
		var c domain.Clause
		var cat, status string
		if err := rows.Scan(
			&c.ClauseID, &c.TenantID, &c.LegalEntityID, &c.Title, &cat, &c.Body, &status, &c.Version,
			&c.JurisdictionID, &c.EffectiveFrom, &c.EffectiveTo, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		); err != nil {
			return nil, err
		}
		c.Category = domain.ClauseCategory(cat)
		c.Status = domain.Status(status)
		out = append(out, c)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateClause(ctx context.Context, c *domain.Clause) error {
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
		UPDATE clauses
		SET title=$1, category=$2, body=$3, jurisdiction_id=$4, effective_to=$5, version=$6, updated_at=$7
		WHERE clause_id=$8`,
		c.Title, string(c.Category), c.Body, c.JurisdictionID, c.EffectiveTo, c.Version, c.UpdatedAt, c.ClauseID,
	)
	if err != nil {
		return fmt.Errorf("update clause: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) CreateTemplate(ctx context.Context, t *domain.ContractTemplate) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if t.TemplateID == "" {
		t.TemplateID = "tmpl-" + uuid.New().String()
	}
	t.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = domain.StatusDraft
	}
	t.Version = 1

	if t.ClauseIDs == nil {
		t.ClauseIDs = []string{}
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO contract_templates
			(template_id, tenant_id, legal_entity_id, title, contract_type, description, clause_ids,
			 status, version, jurisdiction_id, effective_from, effective_to, created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		t.TemplateID, t.TenantID, t.LegalEntityID, t.Title, t.ContractType, t.Description, t.ClauseIDs,
		string(t.Status), t.Version, t.JurisdictionID, t.EffectiveFrom, t.EffectiveTo,
		t.CreatedBy, t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert template: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetTemplate(ctx context.Context, id string) (*domain.ContractTemplate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var t domain.ContractTemplate
	var status string
	err = tx.QueryRow(ctx, `
		SELECT template_id, tenant_id, legal_entity_id, title, contract_type, COALESCE(description,''), clause_ids,
		       status, version, jurisdiction_id, effective_from, effective_to, created_by, created_at, updated_at
		FROM contract_templates WHERE template_id = $1`, id,
	).Scan(
		&t.TemplateID, &t.TenantID, &t.LegalEntityID, &t.Title, &t.ContractType, &t.Description, &t.ClauseIDs,
		&status, &t.Version, &t.JurisdictionID, &t.EffectiveFrom, &t.EffectiveTo, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrTemplateNotFound
		}
		return nil, err
	}
	t.Status = domain.Status(status)
	_ = tx.Commit(ctx)
	return &t, nil
}

func (s *PgStore) ListTemplates(ctx context.Context, legalEntityID, contractType string) ([]domain.ContractTemplate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT template_id, tenant_id, legal_entity_id, title, contract_type, COALESCE(description,''), clause_ids,
		       status, version, jurisdiction_id, effective_from, effective_to, created_by, created_at, updated_at
		FROM contract_templates
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR contract_type = $2)
		ORDER BY created_at DESC`, legalEntityID, contractType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ContractTemplate
	for rows.Next() {
		var t domain.ContractTemplate
		var status string
		if err := rows.Scan(
			&t.TemplateID, &t.TenantID, &t.LegalEntityID, &t.Title, &t.ContractType, &t.Description, &t.ClauseIDs,
			&status, &t.Version, &t.JurisdictionID, &t.EffectiveFrom, &t.EffectiveTo, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		t.Status = domain.Status(status)
		out = append(out, t)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateTemplate(ctx context.Context, t *domain.ContractTemplate) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	t.Version++
	t.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE contract_templates
		SET title=$1, contract_type=$2, description=$3, clause_ids=$4, jurisdiction_id=$5, effective_to=$6, version=$7, updated_at=$8
		WHERE template_id=$9`,
		t.Title, t.ContractType, t.Description, t.ClauseIDs, t.JurisdictionID, t.EffectiveTo, t.Version, t.UpdatedAt, t.TemplateID,
	)
	if err != nil {
		return fmt.Errorf("update template: %w", err)
	}

	return tx.Commit(ctx)
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
