// Package store implements privacy-decision-svc's persistence.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/privacy-decision-svc/internal/domain"
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
	FindDecision(ctx context.Context, tenantID, decisionID string) (*domain.PrivacyDecision, error)
	// GetIdempotency looks up a stored response by (tenant, principal, key).
	GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error)
	// CommitDecision persists the decision AND its idempotency record in one
	// transaction (§13.2/§18.1): either both exist or neither does. If the key
	// is already claimed it persists nothing and returns the existing record.
	CommitDecision(ctx context.Context, claim domain.IdempotencyRecord, d *domain.PrivacyDecision, render RenderFunc) (existing *domain.IdempotencyRecord, err error)
}

// RenderFunc builds the HTTP status and body to store for replay, from the
// decision as persisted (DecidedAt/EvidenceRef/ContextHash are set by then).
type RenderFunc func(d *domain.PrivacyDecision) (code int, body []byte, err error)

type PgStore struct {
	pool *pgxpool.Pool
	log  *zap.Logger
}

func NewPgStore(pool *pgxpool.Pool, log *zap.Logger) *PgStore {
	return &PgStore{pool: pool, log: log}
}

func (s *PgStore) withTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return errors.New("tenant_id is required for store operations")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
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
	secondary_purpose_id, recipient_context, transfer_decision_id, policy_refs, evidence_ref, expires_at`

func (s *PgStore) RecordDecision(ctx context.Context, tenantID string, d *domain.PrivacyDecision) error {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return errors.New("tenantID is required")
	}
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return insertDecision(ctx, tx, tenantID, d)
	})
	if err != nil {
		s.log.Error("pg RecordDecision failed", zap.Error(err))
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

// insertDecision inserts the append-only decision row inside tx.
func insertDecision(ctx context.Context, tx pgx.Tx, tenantID string, d *domain.PrivacyDecision) error {
	if d.DecisionID == "" {
		d.DecisionID = uuid.New().String()
	}
	d.TenantID = &tenantID
	d.ContextHash = d.InputFingerprint
	if d.EvidenceRef == "" {
		d.EvidenceRef = "prv-dec:" + d.DecisionID
	}
	reasonRaw := domain.MarshalReasonCodes(d.ReasonCodes)
	constraintsRaw := domain.MarshalConstraints(d.Constraints)
	policyRaw := domain.MarshalPolicyRefs(d.PolicyRefs)
	subjRaw := domain.MarshalJSONB(d.SubjectContext)
	dataRaw := domain.MarshalJSONB(d.DataContext)
	recipientRaw := domain.MarshalJSONB(d.RecipientContext)
	var secondary string
	if d.SecondaryPurposeID != nil {
		secondary = *d.SecondaryPurposeID
	}
	return tx.QueryRow(ctx, `
		INSERT INTO privacy_decisions (`+decisionColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW(), $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
		RETURNING decided_at`,
		d.DecisionID, tenantID, d.SubjectRef, d.ProcessingActivityID, d.ActivityVersionID,
		d.PurposeID, d.PurposeVersionID, d.ProposedOperation, d.Result, reasonRaw,
		d.ConsentReceiptID, d.LegalHoldID, d.ActorPrincipalID, strPtrOrNil(d.CorrelationID),
		d.InputFingerprint, d.NoticeVersionID, constraintsRaw, subjRaw, dataRaw,
		strPtrOrNil(secondary), recipientRaw, d.TransferDecisionID, policyRaw, d.EvidenceRef, d.ExpiresAt,
	).Scan(&d.DecidedAt)
}

// CommitDecision claims the idempotency key, inserts the decision and stores
// the replayable response in ONE transaction. A failure at any step rolls the
// whole transaction back, so a retry with the same key re-evaluates and can
// never leave behind a decision without its idempotency record (or the
// reverse), nor a duplicate decision.
func (s *PgStore) CommitDecision(ctx context.Context, claim domain.IdempotencyRecord, d *domain.PrivacyDecision, render RenderFunc) (*domain.IdempotencyRecord, error) {
	tenantID := strings.TrimSpace(claim.TenantID)
	if tenantID == "" {
		return nil, errors.New("tenantID is required")
	}
	var existing *domain.IdempotencyRecord
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO decision_idempotency_keys
				(idempotency_key, tenant_id, principal_id, endpoint, request_hash, response_code, response_body, created_at)
			VALUES ($1, $2, $3, $4, $5, 0, ''::bytea, NOW())
			ON CONFLICT (tenant_id, principal_id, idempotency_key) DO NOTHING`,
			claim.Key, tenantID, claim.PrincipalID, claim.Endpoint, claim.RequestHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			rec, err := getIdempotencyTx(ctx, tx, tenantID, claim.PrincipalID, claim.Key)
			if err != nil {
				return err
			}
			existing = rec
			return errSkipCommit
		}
		if err := insertDecision(ctx, tx, tenantID, d); err != nil {
			return err
		}
		code, body, err := render(d)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE decision_idempotency_keys SET response_code = $1, response_body = $2
			WHERE tenant_id = $3 AND principal_id = $4 AND idempotency_key = $5`,
			code, body, tenantID, claim.PrincipalID, claim.Key)
		return err
	})
	if errors.Is(err, errSkipCommit) {
		return existing, nil
	}
	if err != nil {
		s.log.Error("pg CommitDecision failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil, nil
}

// errSkipCommit aborts (rolls back) a transaction that has nothing to write.
var errSkipCommit = errors.New("skip commit")

func (s *PgStore) FindDecision(ctx context.Context, tenantID, decisionID string) (*domain.PrivacyDecision, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, errors.New("tenantID is required")
	}
	var d domain.PrivacyDecision
	var reasonRaw, constraintsRaw, subjRaw, dataRaw, recipientRaw, policyRaw []byte
	var correlationID, secPurposeID, evidenceRef *string
	var expiresAt *time.Time

	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT `+decisionColumns+`
			FROM privacy_decisions WHERE decision_id = $1 AND tenant_id = $2`,
			decisionID, tenantID,
		).Scan(
			&d.DecisionID, &d.TenantID, &d.SubjectRef, &d.ProcessingActivityID, &d.ActivityVersionID,
			&d.PurposeID, &d.PurposeVersionID, &d.ProposedOperation, &d.Result, &reasonRaw,
			&d.ConsentReceiptID, &d.LegalHoldID, &d.ActorPrincipalID, &correlationID, &d.DecidedAt,
			&d.InputFingerprint, &d.NoticeVersionID, &constraintsRaw, &subjRaw, &dataRaw,
			&secPurposeID, &recipientRaw, &d.TransferDecisionID, &policyRaw, &evidenceRef, &expiresAt,
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
	d.PolicyRefs = domain.UnmarshalPolicyRefs(policyRaw)
	d.ContextHash = d.InputFingerprint
	if evidenceRef != nil && *evidenceRef != "" {
		d.EvidenceRef = *evidenceRef
	} else {
		d.EvidenceRef = "prv-dec:" + d.DecisionID
	}
	d.ExpiresAt = expiresAt
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

func getIdempotencyTx(ctx context.Context, tx pgx.Tx, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	var rec domain.IdempotencyRecord
	err := tx.QueryRow(ctx, `
		SELECT idempotency_key, tenant_id, principal_id, endpoint, request_hash, response_code, response_body, created_at
		FROM decision_idempotency_keys
		WHERE idempotency_key = $1 AND tenant_id = $2 AND principal_id = $3`,
		key, tenantID, principalID,
	).Scan(&rec.Key, &rec.TenantID, &rec.PrincipalID, &rec.Endpoint, &rec.RequestHash, &rec.ResponseCode, &rec.ResponseBody, &rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *PgStore) GetIdempotency(ctx context.Context, tenantID, principalID, key string) (*domain.IdempotencyRecord, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, errors.New("tenantID is required")
	}
	var rec *domain.IdempotencyRecord
	err := s.withTenant(ctx, tenantID, func(tx pgx.Tx) error {
		r, err := getIdempotencyTx(ctx, tx, tenantID, principalID, key)
		rec = r
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg GetIdempotency failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return rec, nil
}
