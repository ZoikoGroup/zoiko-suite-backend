package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/obligation-tracking-svc/internal/domain"
	"zoiko.io/obligation-tracking-svc/internal/middleware"
)

type Store interface {
	CreateObligation(ctx context.Context, o *domain.Obligation) error
	GetObligation(ctx context.Context, id string) (*domain.Obligation, error)
	ListObligations(ctx context.Context, legalEntityID, status, sourceType string) ([]domain.Obligation, error)
	UpdateObligation(ctx context.Context, o *domain.Obligation) error
	FulfillObligation(ctx context.Context, id string, req *domain.FulfillObligationRequest) (*domain.Obligation, error)
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

func (s *PgStore) CreateObligation(ctx context.Context, o *domain.Obligation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	if o.ObligationID == "" {
		o.ObligationID = "obg-" + uuid.New().String()
	}
	o.TenantID = middleware.GetTenantID(ctx)
	now := time.Now().UTC()
	o.CreatedAt = now
	o.UpdatedAt = now
	if o.Status == "" {
		o.Status = domain.ObligationStatusPending
	}
	if o.RiskLevel == "" {
		o.RiskLevel = domain.RiskLevelMedium
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO obligations
			(obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, description,
			 obligation_type, risk_level, status, due_date, assigned_to, effective_from, effective_to,
			 created_by, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		o.ObligationID, o.TenantID, o.LegalEntityID, o.SourceType, o.SourceID, o.Title, o.Description,
		string(o.ObligationType), string(o.RiskLevel), string(o.Status), o.DueDate, o.AssignedTo,
		o.EffectiveFrom, o.EffectiveTo, o.CreatedBy, o.CreatedAt, o.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert obligation: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) GetObligation(ctx context.Context, id string) (*domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	var o domain.Obligation
	var otype, risk, status string
	err = tx.QueryRow(ctx, `
		SELECT obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, COALESCE(description,''),
		       obligation_type, risk_level, status, due_date, COALESCE(assigned_to,''), fulfilled_at, fulfilled_by,
		       fulfillment_note, effective_from, effective_to, created_by, created_at, updated_at
		FROM obligations WHERE obligation_id = $1`, id,
	).Scan(
		&o.ObligationID, &o.TenantID, &o.LegalEntityID, &o.SourceType, &o.SourceID, &o.Title, &o.Description,
		&otype, &risk, &status, &o.DueDate, &o.AssignedTo, &o.FulfilledAt, &o.FulfilledBy,
		&o.FulfillmentNote, &o.EffectiveFrom, &o.EffectiveTo, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		if errorsIs(err, pgx.ErrNoRows) {
			return nil, domain.ErrObligationNotFound
		}
		return nil, err
	}
	o.ObligationType = domain.ObligationType(otype)
	o.RiskLevel = domain.RiskLevel(risk)
	o.Status = domain.ObligationStatus(status)
	_ = tx.Commit(ctx)
	return &o, nil
}

func (s *PgStore) ListObligations(ctx context.Context, legalEntityID, status, sourceType string) ([]domain.Obligation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return nil, err
	}

	rows, err := tx.Query(ctx, `
		SELECT obligation_id, tenant_id, legal_entity_id, source_type, source_id, title, COALESCE(description,''),
		       obligation_type, risk_level, status, due_date, COALESCE(assigned_to,''), fulfilled_at, fulfilled_by,
		       fulfillment_note, effective_from, effective_to, created_by, created_at, updated_at
		FROM obligations
		WHERE ($1 = '' OR legal_entity_id = $1)
		  AND ($2 = '' OR status = $2)
		  AND ($3 = '' OR source_type = $3)
		ORDER BY due_date ASC, created_at DESC`, legalEntityID, status, sourceType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Obligation
	for rows.Next() {
		var o domain.Obligation
		var otype, risk, stat string
		if err := rows.Scan(
			&o.ObligationID, &o.TenantID, &o.LegalEntityID, &o.SourceType, &o.SourceID, &o.Title, &o.Description,
			&otype, &risk, &stat, &o.DueDate, &o.AssignedTo, &o.FulfilledAt, &o.FulfilledBy,
			&o.FulfillmentNote, &o.EffectiveFrom, &o.EffectiveTo, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
		); err != nil {
			return nil, err
		}
		o.ObligationType = domain.ObligationType(otype)
		o.RiskLevel = domain.RiskLevel(risk)
		o.Status = domain.ObligationStatus(stat)
		out = append(out, o)
	}
	_ = tx.Commit(ctx)
	return out, nil
}

func (s *PgStore) UpdateObligation(ctx context.Context, o *domain.Obligation) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.setRLS(ctx, tx); err != nil {
		return err
	}

	o.UpdatedAt = time.Now().UTC()

	_, err = tx.Exec(ctx, `
		UPDATE obligations
		SET title=$1, description=$2, obligation_type=$3, risk_level=$4, due_date=$5,
		    assigned_to=$6, status=$7, effective_to=$8, updated_at=$9
		WHERE obligation_id=$10`,
		o.Title, o.Description, string(o.ObligationType), string(o.RiskLevel), o.DueDate,
		o.AssignedTo, string(o.Status), o.EffectiveTo, o.UpdatedAt, o.ObligationID,
	)
	if err != nil {
		return fmt.Errorf("update obligation: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *PgStore) FulfillObligation(ctx context.Context, id string, req *domain.FulfillObligationRequest) (*domain.Obligation, error) {
	o, err := s.GetObligation(ctx, id)
	if err != nil {
		return nil, err
	}
	if o.Status == domain.ObligationStatusFulfilled {
		return nil, domain.ErrObligationAlreadyFulfilled
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
	o.Status = domain.ObligationStatusFulfilled
	o.FulfilledBy = &req.FulfilledBy
	o.FulfilledAt = &now
	if req.FulfillmentNote != "" {
		o.FulfillmentNote = &req.FulfillmentNote
	}
	o.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE obligations
		SET status=$1, fulfilled_by=$2, fulfilled_at=$3, fulfillment_note=$4, updated_at=$5
		WHERE obligation_id=$6`,
		string(o.Status), o.FulfilledBy, o.FulfilledAt, o.FulfillmentNote, o.UpdatedAt, id,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return o, nil
}

func errorsIs(err, target error) bool {
	return err == target || (err != nil && err.Error() == target.Error())
}
