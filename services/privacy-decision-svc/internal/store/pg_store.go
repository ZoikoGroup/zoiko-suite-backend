// Package store implements privacy-decision-svc's persistence.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/middleware"
)

// Store is the interface the handler depends on.
type Store interface {
	RecordDecision(ctx context.Context, tenantID string, d *domain.PrivacyDecision) error
	FindDecision(ctx context.Context, decisionID string) (*domain.PrivacyDecision, error)
}

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", middleware.TenantFromContext(ctx)); err != nil {
		return fmt.Errorf("set_config app.tenant_id: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const decisionColumns = `
	decision_id, tenant_id, subject_ref, processing_activity_id, activity_version_id,
	purpose_id, purpose_version_id, proposed_operation, result, reason_codes,
	consent_receipt_id, legal_hold_id, actor_principal_id, correlation_id, decided_at`

func (s *PgStore) RecordDecision(ctx context.Context, tenantID string, d *domain.PrivacyDecision) error {
	if d.DecisionID == "" {
		d.DecisionID = uuid.New().String()
	}
	reasonRaw := domain.MarshalReasonCodes(d.ReasonCodes)

	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO privacy_decisions (`+decisionColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW())
			RETURNING decided_at`,
			d.DecisionID, strPtrOrNil(tenantID), d.SubjectRef, d.ProcessingActivityID, d.ActivityVersionID,
			d.PurposeID, d.PurposeVersionID, d.ProposedOperation, d.Result, reasonRaw,
			d.ConsentReceiptID, d.LegalHoldID, d.ActorPrincipalID, strPtrOrNil(d.CorrelationID),
		).Scan(&d.DecidedAt)
	})
	if err != nil {
		s.log.Error("pg RecordDecision failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	d.TenantID = strPtrOrNil(tenantID)
	return nil
}

func (s *PgStore) FindDecision(ctx context.Context, decisionID string) (*domain.PrivacyDecision, error) {
	var d domain.PrivacyDecision
	var reasonRaw []byte
	var correlationID *string
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT `+decisionColumns+`
			FROM privacy_decisions WHERE decision_id = $1`,
			decisionID,
		).Scan(&d.DecisionID, &d.TenantID, &d.SubjectRef, &d.ProcessingActivityID, &d.ActivityVersionID,
			&d.PurposeID, &d.PurposeVersionID, &d.ProposedOperation, &d.Result, &reasonRaw,
			&d.ConsentReceiptID, &d.LegalHoldID, &d.ActorPrincipalID, &correlationID, &d.DecidedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrDecisionNotFound
	}
	if err != nil {
		s.log.Error("pg FindDecision failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	d.ReasonCodes = domain.UnmarshalReasonCodes(reasonRaw)
	if correlationID != nil {
		d.CorrelationID = *correlationID
	}
	return &d, nil
}
