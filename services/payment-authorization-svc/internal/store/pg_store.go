// Package store implements payment-authorization-svc's persistence.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/middleware"
	"zoiko.io/payment-authorization-svc/internal/outbox"
)

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type nullString struct{ dest *string }

func (n *nullString) Scan(src interface{}) error {
	if src == nil {
		*n.dest = ""
		return nil
	}
	s, _ := src.(string)
	*n.dest = s
	return nil
}

// Store is the interface the handler depends on.
type Store interface {
	RequestAuthorization(ctx context.Context, tenantID string, auth domain.PaymentAuthorization, snapshots []domain.PayeeSnapshot) (*domain.PaymentAuthorization, error)
	FindAuthorization(ctx context.Context, authorizationID string) (*domain.PaymentAuthorization, error)
	ListPayeeSnapshots(ctx context.Context, authorizationID string) ([]domain.PayeeSnapshot, error)

	// ApproveAuthorization records principalID's signature. The authorization
	// becomes APPROVED only when the number of distinct signatures reaches
	// the required count (the larger of what it already required and
	// requiredSignatures); until then it stays PENDING with the signature
	// recorded. Errors: ErrInvalidTransition (not PENDING), ErrStaleVersion,
	// ErrAuthorizationExpired, ErrAlreadySigned.
	ApproveAuthorization(ctx context.Context, authorizationID, policyResult, policyVersionID, principalID string, requiredSignatures int, expectedVersion *int) (*domain.PaymentAuthorization, error)
	ListSignatures(ctx context.Context, authorizationID string) ([]domain.Signature, error)
	// ListExpiredCandidates returns authorizations past expires_at across all
	// tenants, for the expiry sweeper. It relies on the same RLS-exempt
	// runtime role as the outbox relay.
	ListExpiredCandidates(ctx context.Context, limit int) ([]domain.ExpiredRef, error)

	// Idempotency (spec §16 idempotency_key). BeginIdempotent claims the key
	// (created=true) or returns what is already stored for it.
	BeginIdempotent(ctx context.Context, scope, key, requestHash string) (rec *domain.IdempotencyRecord, created bool, err error)
	CompleteIdempotent(ctx context.Context, scope, key string, statusCode int, body []byte) error
	ReleaseIdempotent(ctx context.Context, scope, key string) error
	PurgeIdempotency(ctx context.Context, olderThan time.Duration) (int64, error)
	RejectAuthorization(ctx context.Context, authorizationID string, req domain.RejectPaymentRequest, principalID string) (*domain.PaymentAuthorization, error)
	InvalidateAuthorization(ctx context.Context, authorizationID, reason string) (*domain.PaymentAuthorization, error)
	ConsumeAuthorization(ctx context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error)
	RevokeAuthorization(ctx context.Context, authorizationID string, req domain.RevokeAuthorizationRequest, principalID string) (*domain.PaymentAuthorization, error)
	ExpireAuthorization(ctx context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error)

	ListEvents(ctx context.Context, authorizationID string) ([]domain.AuthorizationEvent, error)
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

func (s *PgStore) recordEvent(ctx context.Context, tx pgx.Tx, tenantID *string, authorizationID, eventType, detail, actorPrincipalID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO authorization_events (event_id, tenant_id, authorization_id, event_type, detail, actor_principal_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), tenantID, authorizationID, eventType, detail, actorPrincipalID,
	)
	return err
}

// ── authorizations ───────────────────────────────────────────────────────────

const authColumns = `
	authorization_id, tenant_id, legal_entity_id, proposal_id, proposal_fingerprint, net_amount, currency,
	status, policy_assessment_result, policy_version_id, requested_by_principal_id,
	approved_by_principal_id, approved_at, rejected_reason,
	revoked_by_principal_id, revoked_reason, revoked_at, expired_at,
	consumed_by_principal_id, consumed_at, invalidated_reason, created_at, updated_at,
	version, required_signatures, signature_count, expires_at`

func scanAuth(row pgx.Row) (*domain.PaymentAuthorization, error) {
	a := &domain.PaymentAuthorization{}
	err := row.Scan(&a.AuthorizationID, &a.TenantID, &a.LegalEntityID, &a.ProposalID, &a.ProposalFingerprint,
		&a.NetAmount, &a.Currency, &a.Status, &nullString{&a.PolicyAssessmentResult}, &nullString{&a.PolicyVersionID},
		&a.RequestedByPrincipalID, &a.ApprovedByPrincipalID, &a.ApprovedAt, &nullString{&a.RejectedReason},
		&a.RevokedByPrincipalID, &nullString{&a.RevokedReason}, &a.RevokedAt, &a.ExpiredAt,
		&a.ConsumedByPrincipalID, &a.ConsumedAt, &nullString{&a.InvalidatedReason}, &a.CreatedAt, &a.UpdatedAt,
		&a.Version, &a.RequiredSignatures, &a.SignatureCount, &a.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// RequestAuthorization relies on the migration's partial unique index to
// reject a second active (PENDING/APPROVED) authorization against the same
// proposal — a real database invariant beyond AP-10's own four named
// negative-path scenarios.
func (s *PgStore) RequestAuthorization(ctx context.Context, tenantID string, auth domain.PaymentAuthorization, snapshots []domain.PayeeSnapshot) (*domain.PaymentAuthorization, error) {
	id := uuid.New().String()
	var out *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanAuth(tx.QueryRow(ctx, `
			INSERT INTO payment_authorizations (`+authColumns+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', '', '', $8, NULL, NULL, '', NULL, '', NULL, NULL, NULL, NULL, '', NOW(), NOW(),
				1, 1, 0, $9)
			RETURNING `+authColumns,
			id, strPtrOrNil(tenantID), auth.LegalEntityID, auth.ProposalID, auth.ProposalFingerprint,
			auth.NetAmount, auth.Currency, auth.RequestedByPrincipalID, auth.ExpiresAt,
		))
		if err != nil {
			return err
		}
		for _, snap := range snapshots {
			if _, err := tx.Exec(ctx, `
				INSERT INTO authorization_payee_snapshots (snapshot_id, tenant_id, authorization_id, payee_ref, payee_snapshot_at, destination_id)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				uuid.New().String(), out.TenantID, id, snap.PayeeRef, snap.PayeeSnapshotAt, snap.DestinationID,
			); err != nil {
				return err
			}
		}
		if err := s.recordEvent(ctx, tx, out.TenantID, id, domain.EventAuthorizationRequested, auth.ProposalID, auth.RequestedByPrincipalID); err != nil {
			return err
		}
		corr := middleware.CorrelationIDFromContext(ctx)
		var corrPtr *string
		if corr != "" {
			corrPtr = &corr
		}
		outboxEventID := uuid.NewString()
		env := outbox.NewVariantBEnvelope(
			outboxEventID,
			domain.EventAuthorizationRequested,
			id,
			out.TenantID,
			&auth.RequestedByPrincipalID,
			corrPtr,
			out,
		)
		return outbox.Insert(ctx, tx, outbox.Event{
			OutboxEventID: outboxEventID,
			AggregateType: "payment_authorization",
			AggregateID:   id,
			EventType:     domain.EventAuthorizationRequested,
			TenantID:      out.TenantID,
			LegalEntityID: auth.LegalEntityID,
			ActorID:       &auth.RequestedByPrincipalID,
			CorrelationID: corrPtr,
			Payload:       env,
		})
	})
	if isUniqueViolation(err) {
		return nil, domain.ErrProposalAlreadyRequested
	}
	if err != nil {
		s.log.Error("pg RequestAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

func (s *PgStore) FindAuthorization(ctx context.Context, authorizationID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `SELECT `+authColumns+` FROM payment_authorizations WHERE authorization_id = $1`, authorizationID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrAuthorizationNotFound
	}
	if err != nil {
		s.log.Error("pg FindAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) ListPayeeSnapshots(ctx context.Context, authorizationID string) ([]domain.PayeeSnapshot, error) {
	var out []domain.PayeeSnapshot
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT snapshot_id, tenant_id, authorization_id, payee_ref, payee_snapshot_at, destination_id, created_at
			FROM authorization_payee_snapshots WHERE authorization_id = $1 ORDER BY created_at ASC`, authorizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var snap domain.PayeeSnapshot
			if err := rows.Scan(&snap.SnapshotID, &snap.TenantID, &snap.AuthorizationID, &snap.PayeeRef, &snap.PayeeSnapshotAt, &snap.DestinationID, &snap.CreatedAt); err != nil {
				return err
			}
			out = append(out, snap)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListPayeeSnapshots failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// ApproveAuthorization records one signer's signature under a row lock and
// moves the authorization to APPROVED only when enough distinct signers have
// signed. The unique (authorization_id, signer) index makes a second
// signature by the same principal impossible; the required count never goes
// down once raised.
func (s *PgStore) ApproveAuthorization(ctx context.Context, authorizationID, policyResult, policyVersionID, principalID string, requiredSignatures int, expectedVersion *int) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		current, err := scanAuth(tx.QueryRow(ctx, `SELECT `+authColumns+` FROM payment_authorizations WHERE authorization_id = $1 FOR UPDATE`, authorizationID))
		if err != nil {
			return err
		}
		if current.Status != domain.StatusPending {
			return domain.ErrInvalidTransition
		}
		if expectedVersion != nil && current.Version != *expectedVersion {
			return domain.ErrStaleVersion
		}
		if current.ExpiresAt != nil && !current.ExpiresAt.After(time.Now()) {
			return domain.ErrAuthorizationExpired
		}

		// ON CONFLICT DO NOTHING (not a caught unique violation): a failed
		// INSERT would abort this transaction.
		tag, err := tx.Exec(ctx, `
			INSERT INTO authorization_signatures (signature_id, tenant_id, authorization_id, signer_principal_id, policy_result, policy_version_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (authorization_id, signer_principal_id) DO NOTHING`,
			uuid.New().String(), current.TenantID, authorizationID, principalID, policyResult, policyVersionID,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrAlreadySigned
		}

		required := current.RequiredSignatures
		if requiredSignatures > required {
			required = requiredSignatures
		}
		count := current.SignatureCount + 1

		if count < required {
			a, err = scanAuth(tx.QueryRow(ctx, `
				UPDATE payment_authorizations SET signature_count = $2, required_signatures = $3,
					policy_assessment_result = $4, policy_version_id = $5, updated_at = NOW()
				WHERE authorization_id = $1 AND status = 'PENDING'
				RETURNING `+authColumns,
				authorizationID, count, required, policyResult, policyVersionID,
			))
			if err != nil {
				return err
			}
			return s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationSigned,
				fmt.Sprintf("signature %d of %d", count, required), principalID)
		}

		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'APPROVED', signature_count = $2, required_signatures = $3,
				policy_assessment_result = $4, policy_version_id = $5,
				approved_by_principal_id = $6, approved_at = NOW(), updated_at = NOW()
			WHERE authorization_id = $1 AND status = 'PENDING'
			RETURNING `+authColumns,
			authorizationID, count, required, policyResult, policyVersionID, principalID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventPaymentAuthorized, fmt.Sprintf("%d of %d signatures", count, required), principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, domain.EventPaymentAuthorized, a, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	for _, known := range []error{domain.ErrInvalidTransition, domain.ErrStaleVersion, domain.ErrAuthorizationExpired, domain.ErrAlreadySigned} {
		if errors.Is(err, known) {
			return nil, err
		}
	}
	if err != nil {
		s.log.Error("pg ApproveAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) ListSignatures(ctx context.Context, authorizationID string) ([]domain.Signature, error) {
	var out []domain.Signature
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT signature_id, tenant_id, authorization_id, signer_principal_id, policy_result, policy_version_id, signed_at
			FROM authorization_signatures WHERE authorization_id = $1 ORDER BY signed_at ASC`, authorizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sg domain.Signature
			if err := rows.Scan(&sg.SignatureID, &sg.TenantID, &sg.AuthorizationID, &sg.SignerPrincipalID, &sg.PolicyResult, &sg.PolicyVersionID, &sg.SignedAt); err != nil {
				return err
			}
			out = append(out, sg)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListSignatures failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}

// ListExpiredCandidates reads across tenants (no app.tenant_id), exactly as
// the outbox relay does; it depends on the runtime role bypassing RLS.
func (s *PgStore) ListExpiredCandidates(ctx context.Context, limit int) ([]domain.ExpiredRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT authorization_id::text, COALESCE(tenant_id::text, '')
		FROM payment_authorizations
		WHERE status IN ('PENDING', 'APPROVED') AND expires_at IS NOT NULL AND expires_at < NOW()
		ORDER BY expires_at ASC LIMIT $1`, limit)
	if err != nil {
		s.log.Error("pg ListExpiredCandidates failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	defer rows.Close()
	var out []domain.ExpiredRef
	for rows.Next() {
		var r domain.ExpiredRef
		if err := rows.Scan(&r.AuthorizationID, &r.TenantID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── idempotency ──────────────────────────────────────────────────────────────

// BeginIdempotent claims (tenant, scope, key). An earlier claim that never
// completed (a crash mid-command) is taken over after a minute so a retry is
// not locked out forever; a completed one is returned for replay.
func (s *PgStore) BeginIdempotent(ctx context.Context, scope, key, requestHash string) (*domain.IdempotencyRecord, bool, error) {
	tenantKey := middleware.TenantFromContext(ctx)
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (tenant_key, scope, idem_key, request_hash)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_key, scope, idem_key) DO UPDATE
			SET request_hash = EXCLUDED.request_hash, created_at = NOW()
			WHERE idempotency_keys.status_code IS NULL AND idempotency_keys.created_at < NOW() - INTERVAL '60 seconds'`,
		tenantKey, scope, key, requestHash)
	if err != nil {
		s.log.Error("pg BeginIdempotent failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if tag.RowsAffected() == 1 {
		return nil, true, nil
	}
	var rec domain.IdempotencyRecord
	var status *int
	if err := s.pool.QueryRow(ctx, `
		SELECT request_hash, status_code, response_body FROM idempotency_keys
		WHERE tenant_key = $1 AND scope = $2 AND idem_key = $3`, tenantKey, scope, key,
	).Scan(&rec.RequestHash, &status, &rec.Body); err != nil {
		s.log.Error("pg BeginIdempotent read failed", zap.Error(err))
		return nil, false, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	if status != nil {
		rec.Completed, rec.StatusCode = true, *status
	}
	return &rec, false, nil
}

func (s *PgStore) CompleteIdempotent(ctx context.Context, scope, key string, statusCode int, body []byte) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE idempotency_keys SET status_code = $4, response_body = $5, completed_at = NOW()
		WHERE tenant_key = $1 AND scope = $2 AND idem_key = $3 AND status_code IS NULL`,
		middleware.TenantFromContext(ctx), scope, key, statusCode, body)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) ReleaseIdempotent(ctx context.Context, scope, key string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM idempotency_keys WHERE tenant_key = $1 AND scope = $2 AND idem_key = $3 AND status_code IS NULL`,
		middleware.TenantFromContext(ctx), scope, key)
	if err != nil {
		return fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return nil
}

func (s *PgStore) PurgeIdempotency(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < NOW() - make_interval(secs => $1)`, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return tag.RowsAffected(), nil
}

// enqueueEvent writes the authorization's state change to the transactional
// outbox, in the same transaction as the change itself, so a published event
// can neither be lost nor describe a change that rolled back. Used for the
// terminal outcomes that carry no further state: rejected, invalidated,
// revoked and expired.
func (s *PgStore) enqueueEvent(ctx context.Context, tx pgx.Tx, eventType string, a *domain.PaymentAuthorization, actor string) error {
	corr := middleware.CorrelationIDFromContext(ctx)
	var corrPtr *string
	if corr != "" {
		corrPtr = &corr
	}
	outboxEventID := uuid.NewString()
	env := outbox.NewVariantBEnvelope(outboxEventID, eventType, a.AuthorizationID, a.TenantID, &actor, corrPtr, a)
	return outbox.Insert(ctx, tx, outbox.Event{
		OutboxEventID: outboxEventID,
		AggregateType: "payment_authorization",
		AggregateID:   a.AuthorizationID,
		EventType:     eventType,
		TenantID:      a.TenantID,
		LegalEntityID: a.LegalEntityID,
		ActorID:       &actor,
		CorrelationID: corrPtr,
		Payload:       env,
	})
}

func (s *PgStore) RejectAuthorization(ctx context.Context, authorizationID string, req domain.RejectPaymentRequest, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'REJECTED', rejected_reason = $2, updated_at = NOW()
			WHERE authorization_id = $1 AND status = 'PENDING'
			RETURNING `+authColumns,
			authorizationID, req.Reason,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationRejected, req.Reason, principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, domain.EventAuthorizationRejected, a, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RejectAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// InvalidateAuthorization moves a PENDING or APPROVED authorization to
// INVALIDATED — the literal enforcement of negative-path scenario #1's
// outcome ("any protected-field mismatch invalidates"), reachable from
// either the ApprovePayment or ConsumePaymentAuthorization checkpoint.
func (s *PgStore) InvalidateAuthorization(ctx context.Context, authorizationID, reason string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'INVALIDATED', invalidated_reason = $2, updated_at = NOW()
			WHERE authorization_id = $1 AND status IN ('PENDING', 'APPROVED')
			RETURNING `+authColumns,
			authorizationID, reason,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationInvalidated, reason, "system"); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, domain.EventAuthorizationInvalidated, a, "system")
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg InvalidateAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// ConsumeAuthorization's WHERE status = 'APPROVED' guard is the first layer
// of negative-path scenario #4's replay protection — the migration's
// terminal-status trigger is the second, permanent layer.
func (s *PgStore) ConsumeAuthorization(ctx context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'CONSUMED', consumed_by_principal_id = $2, consumed_at = NOW(), updated_at = NOW()
			WHERE authorization_id = $1 AND status = 'APPROVED'
			RETURNING `+authColumns,
			authorizationID, principalID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationConsumed, "", principalID); err != nil {
			return err
		}
		corr := middleware.CorrelationIDFromContext(ctx)
		var corrPtr *string
		if corr != "" {
			corrPtr = &corr
		}
		outboxEventID := uuid.NewString()
		env := outbox.NewVariantBEnvelope(
			outboxEventID,
			domain.EventAuthorizationConsumed,
			a.AuthorizationID,
			a.TenantID,
			&principalID,
			corrPtr,
			a,
		)
		return outbox.Insert(ctx, tx, outbox.Event{
			OutboxEventID: outboxEventID,
			AggregateType: "payment_authorization",
			AggregateID:   a.AuthorizationID,
			EventType:     domain.EventAuthorizationConsumed,
			TenantID:      a.TenantID,
			LegalEntityID: a.LegalEntityID,
			ActorID:       &principalID,
			CorrelationID: corrPtr,
			Payload:       env,
		})
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ConsumeAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) RevokeAuthorization(ctx context.Context, authorizationID string, req domain.RevokeAuthorizationRequest, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'REVOKED', revoked_by_principal_id = $2, revoked_reason = $3, revoked_at = NOW(), updated_at = NOW()
			WHERE authorization_id = $1 AND status IN ('PENDING', 'APPROVED')
			RETURNING `+authColumns,
			authorizationID, principalID, req.Reason,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationRevoked, req.Reason, principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, domain.EventAuthorizationRevoked, a, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg RevokeAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

func (s *PgStore) ExpireAuthorization(ctx context.Context, authorizationID, principalID string) (*domain.PaymentAuthorization, error) {
	var a *domain.PaymentAuthorization
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = scanAuth(tx.QueryRow(ctx, `
			UPDATE payment_authorizations SET status = 'EXPIRED', expired_at = NOW(), updated_at = NOW()
			WHERE authorization_id = $1 AND status IN ('PENDING', 'APPROVED')
			RETURNING `+authColumns,
			authorizationID,
		))
		if err != nil {
			return err
		}
		if err := s.recordEvent(ctx, tx, a.TenantID, authorizationID, domain.EventAuthorizationExpired, "", principalID); err != nil {
			return err
		}
		return s.enqueueEvent(ctx, tx, domain.EventAuthorizationExpired, a, principalID)
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return nil, domain.ErrInvalidTransition
	}
	if err != nil {
		s.log.Error("pg ExpireAuthorization failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return a, nil
}

// ── events ───────────────────────────────────────────────────────────────────

func (s *PgStore) ListEvents(ctx context.Context, authorizationID string) ([]domain.AuthorizationEvent, error) {
	var out []domain.AuthorizationEvent
	err := s.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id, tenant_id, authorization_id, event_type, detail, actor_principal_id, created_at
			FROM authorization_events WHERE authorization_id = $1 ORDER BY created_at ASC`, authorizationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.AuthorizationEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.AuthorizationID, &e.EventType, &nullString{&e.Detail}, &e.ActorPrincipalID, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if isInvalidUUID(err) {
		return nil, nil
	}
	if err != nil {
		s.log.Error("pg ListEvents failed", zap.Error(err))
		return nil, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return out, nil
}
