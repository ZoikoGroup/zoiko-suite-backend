// Package store implements privacy-decision-svc's persistence.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
	"zoiko.io/privacy-decision-svc/internal/middleware"
)

// isInvalidUUID reports whether err is Postgres's own "invalid input
// syntax for type uuid" error (SQLSTATE 22P02).
func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// Store is the interface the handler depends on.
type Store interface {
	RecordDecision(ctx context.Context, tenantID string, d *domain.PrivacyDecision) error
	FindDecision(ctx context.Context, decisionID string) (*domain.PrivacyDecision, error)
	GetIdempotency(ctx context.Context, tenantID, key string) (*domain.IdempotencyRecord, error)
	SaveIdempotency(ctx context.Context, rec domain.IdempotencyRecord) error
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
	consent_receipt_id, legal_hold_id, actor_principal_id, correlation_id, decided_at,
	input_fingerprint, notice_version_id, constraints, subject_context, data_context,
	secondary_purpose_id, recipient_context, transfer_decision_id`

func (s *PgStore) RecordDecision(ctx context.Context, tenantID string, d *domain.PrivacyDecision) error {
	if d.DecisionID == "" {
		d.DecisionID = uuid.New().String()
	}
	reasonRaw := domain.MarshalReasonCodes(d.ReasonCodes)
	constraintsRaw := domain.MarshalConstraints(d.Constraints)
	subjRaw := domain.MarshalJSONB(d.SubjectContext)
	dataRaw := domain.MarshalJSONB(d.DataContext)
	recipientRaw := domain.MarshalJSONB(d.RecipientContext)

	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO privacy_decisions (`+decisionColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW(), $15, $16, $17, $18, $19, $20, $21, $22)
			RETURNING decided_at`,
			d.DecisionID, strPtrOrNil(tenantID), d.SubjectRef, d.ProcessingActivityID, d.ActivityVersionID,
			d.PurposeID, d.PurposeVersionID, d.ProposedOperation, d.Result, reasonRaw,
			d.ConsentReceiptID, d.LegalHoldID, d.ActorPrincipalID, strPtrOrNil(d.CorrelationID),
			d.InputFingerprint, d.NoticeVersionID, constraintsRaw, subjRaw, dataRaw,
			strPtrOrNil(func() string {
				if d.SecondaryPurposeID != nil {
					return *d.SecondaryPurposeID
				}
				return ""
			}()), recipientRaw, d.TransferDecisionID,
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
	var reasonRaw, constraintsRaw, subjRaw, dataRaw, recipientRaw []byte
	var correlationID, secPurposeID *string

	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT `+decisionColumns+`
			FROM privacy_decisions WHERE decision_id = $1`,
			decisionID,
		).Scan(
			&d.DecisionID, &d.TenantID, &d.SubjectRef, &d.ProcessingActivityID, &d.ActivityVersionID,
			&d.PurposeID, &d.PurposeVersionID, &d.ProposedOperation, &d.Result, &reasonRaw,
			&d.ConsentReceiptID, &d.LegalHoldID, &d.ActorPrincipalID, &correlationID, &d.DecidedAt,
			&d.InputFingerprint, &d.NoticeVersionID, &constraintsRaw, &subjRaw, &dataRaw,
			&secPurposeID, &recipientRaw, &d.TransferDecisionID,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrDecisionNotFound
	}
	if err != nil {
		s.log.Error("pg FindDecision failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}

	d.ReasonCodes = domain.UnmarshalReasonCodes(reasonRaw)
	d.Constraints = domain.UnmarshalConstraints(constraintsRaw)
	if correlationID != nil {
		d.CorrelationID = *correlationID
	}
	if secPurposeID != nil {
		d.SecondaryPurposeID = secPurposeID
	}
	if len(subjRaw) > 0 && string(subjRaw) != "{}" {
		var sc domain.SubjectContext
		if err := json.Unmarshal(subjRaw, &sc); err == nil {
			d.SubjectContext = &sc
		}
	}
	if len(dataRaw) > 0 && string(dataRaw) != "{}" {
		var dc domain.DataContext
		if err := json.Unmarshal(dataRaw, &dc); err == nil {
			d.DataContext = &dc
		}
	}
	if len(recipientRaw) > 0 && string(recipientRaw) != "{}" {
		var rc domain.RecipientContext
		if err := json.Unmarshal(recipientRaw, &rc); err == nil {
			d.RecipientContext = &rc
		}
	}

	return &d, nil
}

// ── Idempotency (§18.1) ──────────────────────────────────────────────────────

func (s *PgStore) GetIdempotency(ctx context.Context, tenantID, key string) (*domain.IdempotencyRecord, error) {
	var rec domain.IdempotencyRecord
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT idempotency_key, tenant_id, endpoint, request_hash, response_code, response_body, created_at
			FROM decision_idempotency_keys
			WHERE idempotency_key = $1`,
			key,
		).Scan(&rec.Key, &rec.TenantID, &rec.Endpoint, &rec.RequestHash, &rec.ResponseCode, &rec.ResponseBody, &rec.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg GetIdempotency failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return &rec, nil
}

func (s *PgStore) SaveIdempotency(ctx context.Context, rec domain.IdempotencyRecord) error {
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO decision_idempotency_keys (idempotency_key, tenant_id, endpoint, request_hash, response_code, response_body, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW())
			ON CONFLICT (tenant_id, idempotency_key) DO NOTHING`,
			rec.Key, rec.TenantID, rec.Endpoint, rec.RequestHash, rec.ResponseCode, rec.ResponseBody,
		)
		return err
	})
	if err != nil {
		s.log.Error("pg SaveIdempotency failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}
