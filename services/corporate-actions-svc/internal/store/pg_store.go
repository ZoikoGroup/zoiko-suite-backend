package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/corporate-actions-svc/internal/domain"
	"zoiko.io/corporate-actions-svc/internal/middleware"
)

type Store interface {
	CreateAction(ctx context.Context, a *domain.CorporateAction) error
	GetAction(ctx context.Context, id string) (*domain.CorporateAction, error)
	ListActions(ctx context.Context, legalEntityID, actionType, status string) ([]domain.CorporateAction, error)
	UpdateAction(ctx context.Context, a *domain.CorporateAction) error
	ExecuteAction(ctx context.Context, id string, req *domain.ExecuteCorporateActionRequest) (*domain.CorporateAction, error)
}

type PgStore struct {
	pool *pgxpool.Pool
}

func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

func (s *PgStore) setRLS(ctx context.Context, tx pgx.Tx) error {
	tenantID := middleware.GetTenantID(ctx)
	_, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID)
	return err
}

func (s *PgStore) CreateAction(ctx context.Context, a *domain.CorporateAction) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if a.ActionID == "" {
		a.ActionID = "act-" + uuid.New().String()
	}
	a.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now
	if a.Status == "" {
		a.Status = domain.ActionStatusProposed
	}
	if a.Currency == "" {
		a.Currency = "USD"
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO corporate_actions
			(action_id, tenant_id, legal_entity_id, title, action_type, description, resolution_id,
			 effective_date, status, valuation_amount, currency, effective_from, effective_to,
			 created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		a.ActionID, a.TenantID, a.LegalEntityID, a.Title, string(a.ActionType), a.Description, a.ResolutionID,
		a.EffectiveDate, string(a.Status), a.ValuationAmount, a.Currency, a.EffectiveFrom, a.EffectiveTo,
		a.CreatedBy, a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert corporate action: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetAction(ctx context.Context, id string) (*domain.CorporateAction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var a domain.CorporateAction
	var atype, status string
	err = tx.QueryRow(ctx, `
		SELECT action_id, tenant_id, legal_entity_id, title, action_type, COALESCE(description,''), COALESCE(resolution_id,''),
		       effective_date, status, valuation_amount, currency, executed_at, executed_by, document_vault_id,
		       effective_from, effective_to, created_by, created_at, updated_at
		FROM corporate_actions WHERE action_id = $1`, id,
	).Scan(
		&a.ActionID, &a.TenantID, &a.LegalEntityID, &a.Title, &atype, &a.Description, &a.ResolutionID,
		&a.EffectiveDate, &status, &a.ValuationAmount, &a.Currency, &a.ExecutedAt, &a.ExecutedBy, &a.DocumentVaultID,
		&a.EffectiveFrom, &a.EffectiveTo, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrCorporateActionNotFound
		}
		return nil, err
	}
	a.ActionType = domain.ActionType(atype)
	a.Status = domain.ActionStatus(status)
	_ = tx.Commit(ctx)
	return &a, nil
}

func (s *PgStore) ListActions(ctx context.Context, legalEntityID, actionType, status string) ([]domain.CorporateAction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT action_id, tenant_id, legal_entity_id, title, action_type, COALESCE(description,''), COALESCE(resolution_id,''),
		       effective_date, status, valuation_amount, currency, executed_at, executed_by, document_vault_id,
		       effective_from, effective_to, created_by, created_at, updated_at
		FROM corporate_actions
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR action_type = $2)
		  AND ($3 = '' OR status = $3)
		ORDER BY created_at DESC`, legalEntityID, actionType, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.CorporateAction
	for rows.Next() {
		var a domain.CorporateAction
		var atype, stat string
		if err := rows.Scan(
			&a.ActionID, &a.TenantID, &a.LegalEntityID, &a.Title, &atype, &a.Description, &a.ResolutionID,
			&a.EffectiveDate, &stat, &a.ValuationAmount, &a.Currency, &a.ExecutedAt, &a.ExecutedBy, &a.DocumentVaultID,
			&a.EffectiveFrom, &a.EffectiveTo, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, err
		}
		a.ActionType = domain.ActionType(atype)
		a.Status = domain.ActionStatus(stat)
		out = append(out, a)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateAction(ctx context.Context, a *domain.CorporateAction) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	a.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE corporate_actions
		SET title=$1, action_type=$2, description=$3, resolution_id=$4, effective_date=$5,
		    status=$6, valuation_amount=$7, currency=$8, effective_to=$9, updated_at=$10
		WHERE action_id=$11`,
		a.Title, string(a.ActionType), a.Description, a.ResolutionID, a.EffectiveDate,
		string(a.Status), a.ValuationAmount, a.Currency, a.EffectiveTo, a.UpdatedAt, a.ActionID,
	)
	if err != nil {
		return fmt.Errorf("update corporate action: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) ExecuteAction(ctx context.Context, id string, req *domain.ExecuteCorporateActionRequest) (*domain.CorporateAction, error) {
	a, err := s.GetAction(ctx, id)
	if err != nil {
		return nil, err
	}
	if a.Status == domain.ActionStatusExecuted {
		return nil, domain.ErrActionAlreadyExecuted
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
	a.Status = domain.ActionStatusExecuted
	a.ExecutedBy = &req.ExecutedBy
	a.ExecutedAt = &now
	a.DocumentVaultID = req.DocumentVaultID
	a.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE corporate_actions
		SET status=$1, executed_by=$2, executed_at=$3, document_vault_id=$4, updated_at=$5
		WHERE action_id=$6`,
		string(a.Status), a.ExecutedBy, a.ExecutedAt, a.DocumentVaultID, a.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
