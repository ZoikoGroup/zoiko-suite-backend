package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// TOTP second-factor rows (migration 000012).
//
// A factor is a principal_credentials row with credential_type 'TOTP'. Every
// method runs inside withRLS, like the password methods beside it: an unscoped
// read of this table returns nothing rather than every tenant's factors.
//
// The secret is only ever read here in its encrypted form; decryption happens
// in the MFA service, which holds the key. Nothing in this file can produce a
// working code.

// errTOTPNotAccepted rolls back AcceptTOTPCode's transaction when its
// compare-and-set matched no row.
var errTOTPNotAccepted = errors.New("totp code not accepted")

const totpColumns = `
	credential_id, principal_id, tenant_id, status, secret_ciphertext, secret_key_id,
	totp_last_used_step, failed_attempt_count, locked_until`

func scanTOTP(row pgx.Row) (*domain.TOTPFactorSecret, error) {
	var f domain.TOTPFactorSecret
	var ct, kid *string
	if err := row.Scan(&f.FactorID, &f.PrincipalID, &f.TenantID, &f.Status, &ct, &kid,
		&f.LastUsedStep, &f.FailedAttempts, &f.LockedUntil); err != nil {
		return nil, err
	}
	f.Ciphertext, f.KeyID = deref(ct), deref(kid)
	return &f, nil
}

// HasActiveTOTP reports whether the principal holds a proven TOTP factor.
func (s *PgStore) HasActiveTOTP(ctx context.Context, principalID, tenantID string) (bool, error) {
	if tenantID == "" {
		return false, errors.New("HasActiveTOTP: tenant_id is required")
	}
	var n int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM principal_credentials
			WHERE principal_id = $1 AND tenant_id = $2
			  AND credential_type = 'TOTP' AND status = 'ACTIVE'`,
			principalID, tenantID).Scan(&n)
	})
	return n > 0, err
}

// CreatePendingTOTP stores a new, unproven factor. An earlier enrolment still
// in flight is retired in the same transaction, so a principal has at most one
// PENDING factor (idx_principal_credentials_one_pending) and an abandoned QR
// code can never be activated later.
func (s *PgStore) CreatePendingTOTP(ctx context.Context, f domain.TOTPFactorSecret) error {
	if f.TenantID == "" || f.PrincipalID == "" || f.FactorID == "" {
		return errors.New("CreatePendingTOTP: tenant, principal and factor ids are required")
	}
	if f.Ciphertext == "" || f.KeyID == "" {
		// The one catastrophic caller error: storing the secret unencrypted.
		// There is no field for that, but an empty ciphertext would mean the
		// caller lost the secret entirely.
		return errors.New("CreatePendingTOTP: an encrypted secret and its key id are required")
	}
	now := time.Now().UTC()
	return s.withRLS(ctx, f.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE principal_credentials
			SET status = 'RETIRED', retired_at = $1, retired_reason = 'superseded_enrolment',
			    retired_by_principal_id = $2, updated_at = $1
			WHERE principal_id = $2 AND tenant_id = $3
			  AND credential_type = 'TOTP' AND status = 'PENDING'`,
			now, f.PrincipalID, f.TenantID); err != nil {
			return fmt.Errorf("retire pending totp: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO principal_credentials
				(credential_id, principal_id, tenant_id, credential_type, secret_hash,
				 secret_ciphertext, secret_key_id, algorithm, status,
				 failed_attempt_count, secret_updated_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'TOTP', NULL, $4, $5, 'totp-sha1-6-30', 'PENDING', 0, $6, $6, $6)`,
			f.FactorID, f.PrincipalID, f.TenantID, f.Ciphertext, f.KeyID, now); err != nil {
			return fmt.Errorf("insert pending totp: %w", err)
		}
		return nil
	})
}

// FindTOTPFactor reads one of the principal's factors by id, in any state.
// Returns domain.ErrCredentialNotFound when the principal holds no such factor,
// which includes a factor id belonging to someone else.
func (s *PgStore) FindTOTPFactor(ctx context.Context, factorID, principalID, tenantID string) (*domain.TOTPFactorSecret, error) {
	if tenantID == "" {
		return nil, errors.New("FindTOTPFactor: tenant_id is required")
	}
	var f *domain.TOTPFactorSecret
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		f, err = scanTOTP(tx.QueryRow(ctx, `SELECT `+totpColumns+`
			FROM principal_credentials
			WHERE credential_id = $1 AND principal_id = $2 AND tenant_id = $3
			  AND credential_type = 'TOTP'`, factorID, principalID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCredentialNotFound
	}
	return f, err
}

// FindActiveTOTP reads the principal's proven factor. Returns
// domain.ErrCredentialNotFound when there is none.
func (s *PgStore) FindActiveTOTP(ctx context.Context, principalID, tenantID string) (*domain.TOTPFactorSecret, error) {
	if tenantID == "" {
		return nil, errors.New("FindActiveTOTP: tenant_id is required")
	}
	var f *domain.TOTPFactorSecret
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		f, err = scanTOTP(tx.QueryRow(ctx, `SELECT `+totpColumns+`
			FROM principal_credentials
			WHERE principal_id = $1 AND tenant_id = $2
			  AND credential_type = 'TOTP' AND status = 'ACTIVE'`, principalID, tenantID))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrCredentialNotFound
	}
	return f, err
}

// AcceptTOTPCode records a verified code, once.
//
// The UPDATE is a compare-and-set on three things at once: the factor is in
// the expected state (PENDING to activate, ACTIVE to authenticate), it is not
// locked, and the accepted step is strictly later than the last one. Two
// concurrent requests carrying the same code therefore cannot both succeed —
// the second matches no row — and an observed code cannot be replayed within
// its window. A miss returns domain.ErrTOTPReplayed and writes nothing.
//
// Activation retires the principal's previous ACTIVE factor first (the unique
// index allows one), so enrolling a new device replaces the old one in the
// same transaction rather than leaving two live factors.
//
// The evidence row and the event (rec, when non-empty) commit with the change.
func (s *PgStore) AcceptTOTPCode(ctx context.Context, a domain.TOTPAcceptance, rec outbox.Record) error {
	if a.TenantID == "" {
		return errors.New("AcceptTOTPCode: tenant_id is required")
	}
	now := time.Now().UTC()
	fromStatus := domain.CredentialStatusActive
	if a.Activate {
		fromStatus = domain.CredentialStatusPending
	}
	err := s.withRLS(ctx, a.TenantID, func(tx pgx.Tx) error {
		if a.Activate {
			if _, err := tx.Exec(ctx, `
				UPDATE principal_credentials
				SET status = 'RETIRED', retired_at = $1, retired_reason = 'replaced',
				    retired_by_principal_id = $2, updated_at = $1
				WHERE principal_id = $2 AND tenant_id = $3
				  AND credential_type = 'TOTP' AND status = 'ACTIVE'`,
				now, a.PrincipalID, a.TenantID); err != nil {
				return fmt.Errorf("retire previous totp: %w", err)
			}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE principal_credentials
			SET totp_last_used_step = $1, failed_attempt_count = 0, locked_until = NULL,
			    last_authenticated_at = $2, updated_at = $2,
			    status = 'ACTIVE',
			    activated_at = COALESCE(activated_at, $2)
			WHERE credential_id = $3 AND principal_id = $4 AND tenant_id = $5
			  AND credential_type = 'TOTP' AND status = $6
			  AND (locked_until IS NULL OR locked_until <= $2)
			  AND (totp_last_used_step IS NULL OR totp_last_used_step < $1)`,
			a.Step, now, a.FactorID, a.PrincipalID, a.TenantID, fromStatus)
		if err != nil {
			return fmt.Errorf("accept totp code: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errTOTPNotAccepted
		}
		if err := s.appendDecision(ctx, tx, a.PrincipalID, a.TenantID, a.Action, "ALLOWED", "totp_verified", a.CorrelationID, now); err != nil {
			return err
		}
		if rec.EventID == "" {
			return nil
		}
		return s.outboxEnqueueTx(ctx, tx, rec)
	})
	if errors.Is(err, errTOTPNotAccepted) {
		return domain.ErrTOTPReplayed
	}
	return err
}

// RecordTOTPFailure counts a wrong code against the factor and locks it at
// maxAttempts, in one statement (the same race-free shape as
// RecordAuthFailure), and appends the evidence row.
func (s *PgStore) RecordTOTPFailure(
	ctx context.Context,
	factorID, principalID, tenantID, action, correlationID string,
	maxAttempts int,
	lockDuration time.Duration,
) (locked bool, attempts int, err error) {
	now := time.Now().UTC()
	err = s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			WITH computed AS (
			    SELECT credential_id,
			           CASE
			               WHEN locked_until IS NOT NULL AND locked_until <= $1 THEN 1
			               ELSE failed_attempt_count + 1
			           END AS next_count
			    FROM principal_credentials
			    WHERE credential_id = $4 AND principal_id = $5 AND tenant_id = $6
			      AND credential_type = 'TOTP'
			)
			UPDATE principal_credentials c
			SET failed_attempt_count = computed.next_count,
			    locked_until = CASE WHEN computed.next_count >= $2 THEN $3::timestamptz ELSE NULL END,
			    updated_at = $1
			FROM computed
			WHERE c.credential_id = computed.credential_id AND c.tenant_id = $6
			RETURNING c.failed_attempt_count, (c.locked_until IS NOT NULL)`,
			now, maxAttempts, now.Add(lockDuration), factorID, principalID, tenantID)
		if scanErr := row.Scan(&attempts, &locked); scanErr != nil {
			return fmt.Errorf("record totp failure: %w", scanErr)
		}
		basis := "totp_mismatch"
		if locked {
			basis = "totp_mismatch_factor_locked"
		}
		return s.appendDecision(ctx, tx, principalID, tenantID, action, "DENIED", basis, correlationID, now)
	})
	return locked, attempts, err
}

// RecordMFADecision appends an evidence row for an MFA command that was decided
// without a code comparison (a locked factor, a replay, no factor enrolled).
func (s *PgStore) RecordMFADecision(ctx context.Context, principalID, tenantID, action, outcome, basis, correlationID string) error {
	now := time.Now().UTC()
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return s.appendDecision(ctx, tx, principalID, tenantID, action, outcome, basis, correlationID, now)
	})
}

// RetireTOTPFactors retires the principal's live (ACTIVE or PENDING) factors —
// one by id, or all of them when factorID is empty — and records who did it and
// why. The evidence row and the event (rec, when non-empty and something was
// retired) commit with it. Returns how many factors were retired.
func (s *PgStore) RetireTOTPFactors(
	ctx context.Context,
	principalID, tenantID, factorID, reason, actorPrincipalID, action, correlationID string,
	rec outbox.Record,
) (int, error) {
	if tenantID == "" {
		return 0, errors.New("RetireTOTPFactors: tenant_id is required")
	}
	now := time.Now().UTC()
	var n int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE principal_credentials
			SET status = 'RETIRED', retired_at = $1, retired_reason = $2,
			    retired_by_principal_id = $3, updated_at = $1
			WHERE principal_id = $4 AND tenant_id = $5
			  AND credential_type = 'TOTP' AND status IN ('ACTIVE', 'PENDING')
			  AND ($6 = '' OR credential_id = $6)`,
			now, reason, actorPrincipalID, principalID, tenantID, factorID)
		if err != nil {
			return fmt.Errorf("retire totp: %w", err)
		}
		n = int(tag.RowsAffected())
		if n == 0 {
			return nil
		}
		if err := s.appendDecision(ctx, tx, principalID, tenantID, action, "ALLOWED", "totp_retired:"+reason, correlationID, now); err != nil {
			return err
		}
		if rec.EventID == "" {
			return nil
		}
		return s.outboxEnqueueTx(ctx, tx, rec)
	})
	return n, err
}

// ListMFAFactors lists the principal's TOTP factors, newest first. No secret
// material is selected.
func (s *PgStore) ListMFAFactors(ctx context.Context, principalID, tenantID string) ([]domain.MFAFactor, error) {
	if tenantID == "" {
		return nil, errors.New("ListMFAFactors: tenant_id is required")
	}
	out := []domain.MFAFactor{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT credential_id, principal_id, credential_type, status, created_at,
			       activated_at, last_authenticated_at, retired_reason,
			       failed_attempt_count, locked_until
			FROM principal_credentials
			WHERE principal_id = $1 AND tenant_id = $2 AND credential_type = 'TOTP'
			ORDER BY created_at DESC
			LIMIT 50`, principalID, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f domain.MFAFactor
			if err := rows.Scan(&f.FactorID, &f.PrincipalID, &f.Type, &f.Status, &f.CreatedAt,
				&f.ActivatedAt, &f.LastUsedAt, &f.RetiredReason, &f.FailedAttempts, &f.LockedUntil); err != nil {
				return err
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	return out, err
}
