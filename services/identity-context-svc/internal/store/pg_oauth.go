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

// withTokenLookup runs fn with the app.token_lookup capability: SELECT across
// tenants on the tables migration 000014 opens to it, for rows found by an
// unguessable secret (a code, a refresh token, a link) before their tenant is
// known. It grants no write; a caller that then writes switches to the
// found row's tenant with setTenant inside the same transaction.
func (s *PgStore) withTokenLookup(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.token_lookup', 'true', true)"); err != nil {
		return fmt.Errorf("set_config app.token_lookup: %w", err)
	}
	if err := fn(tx); err != nil {
		var keep errCommitThen
		if errors.As(err, &keep) {
			if cerr := tx.Commit(ctx); cerr != nil {
				return cerr
			}
		}
		return err
	}
	return tx.Commit(ctx)
}

// lockByTenant reads a row's tenant through the lookup hatch and switches the
// transaction to it, so the SELECT … FOR UPDATE that follows passes the
// tenant policy (FOR UPDATE also needs UPDATE visibility, which the hatch
// does not grant).
func lockByTenant(ctx context.Context, tx pgx.Tx, query string, arg any) error {
	var tenant string
	if err := tx.QueryRow(ctx, query, arg).Scan(&tenant); err != nil {
		return err
	}
	return setTenant(ctx, tx, tenant)
}

func setTenant(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if tenantID == "" {
		return errors.New("tenant_id is required")
	}
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}

func (s *PgStore) enqueueAll(ctx context.Context, tx pgx.Tx, recs []outbox.Record) error {
	for _, rec := range recs {
		if rec.EventID == "" {
			continue
		}
		if err := s.outboxEnqueueTx(ctx, tx, rec); err != nil {
			return err
		}
	}
	return nil
}

// ── Login sessions ──────────────────────────────────────────────────────────

// CreateAuthSession records a completed login.
func (s *PgStore) CreateAuthSession(ctx context.Context, a domain.AuthSession, recs ...outbox.Record) error {
	return s.withRLS(ctx, a.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO auth_sessions (sid, tenant_id, principal_id, client_id, auth_time, amr,
			    assurance_level, idp_id, created_at, expires_at, client_ip_hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			a.SID, a.TenantID, a.PrincipalID, a.ClientID, a.AuthTime, a.AMR, a.AssuranceLevel,
			nullIfEmpty(a.IdPID), a.CreatedAt, a.ExpiresAt, nullIfEmpty(a.ClientIPHash))
		if err != nil {
			return fmt.Errorf("insert auth_session: %w", err)
		}
		return s.enqueueAll(ctx, tx, recs)
	})
}

const authSessionCols = `sid, tenant_id, principal_id, client_id, auth_time, amr, assurance_level,
	COALESCE(idp_id,''), created_at, expires_at, ended_at, COALESCE(end_reason,''), COALESCE(client_ip_hash,'')`

func scanAuthSession(row pgx.Row) (*domain.AuthSession, error) {
	var a domain.AuthSession
	err := row.Scan(&a.SID, &a.TenantID, &a.PrincipalID, &a.ClientID, &a.AuthTime, &a.AMR, &a.AssuranceLevel,
		&a.IdPID, &a.CreatedAt, &a.ExpiresAt, &a.EndedAt, &a.EndReason, &a.ClientIPHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAuthSession reads a login session in tenantID; nil when absent.
func (s *PgStore) GetAuthSession(ctx context.Context, tenantID, sid string) (*domain.AuthSession, error) {
	var out *domain.AuthSession
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanAuthSession(tx.QueryRow(ctx, `SELECT `+authSessionCols+` FROM auth_sessions
			WHERE sid = $1 AND tenant_id = $2`, sid, tenantID))
		return err
	})
	return out, err
}

// EndAuthSession ends a login session and revokes its refresh tokens, and
// returns the live session contexts resolved under it (for the caller to
// invalidate). Idempotent: an ended session returns no ids.
func (s *PgStore) EndAuthSession(ctx context.Context, tenantID, sid, reason string, at time.Time, recs ...outbox.Record) ([]string, error) {
	var ids []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE auth_sessions SET ended_at = $3, end_reason = $4
			WHERE sid = $1 AND tenant_id = $2 AND ended_at IS NULL`, sid, tenantID, at, reason)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE oauth_refresh_tokens SET revoked_at = $3, revoke_reason = $4
			WHERE sid = $1 AND tenant_id = $2 AND revoked_at IS NULL`, sid, tenantID, at, reason); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT session_context_id FROM session_contexts
			WHERE auth_session_id = $1 AND tenant_id = $2 AND invalidated_at IS NULL AND expires_at > $3`, sid, tenantID, at)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	return ids, err
}

// LiveAuthSessionIDs lists a principal's live login sessions.
func (s *PgStore) LiveAuthSessionIDs(ctx context.Context, tenantID, principalID string, now time.Time) ([]string, error) {
	var out []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT sid FROM auth_sessions
			WHERE tenant_id = $1 AND principal_id = $2 AND ended_at IS NULL AND expires_at > $3`, tenantID, principalID, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sid string
			if err := rows.Scan(&sid); err != nil {
				return err
			}
			out = append(out, sid)
		}
		return rows.Err()
	})
	return out, err
}

// ── Authorization codes ─────────────────────────────────────────────────────

// InsertAuthCode stores a (hashed) authorization code.
func (s *PgStore) InsertAuthCode(ctx context.Context, c domain.AuthorizationCode) error {
	return s.withRLS(ctx, c.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO oauth_authorization_codes (code_hash, tenant_id, client_id, redirect_uri, sid,
			    principal_id, scope, nonce, code_challenge, code_challenge_method, dpop_jkt, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			c.CodeHash, c.TenantID, c.ClientID, c.RedirectURI, c.SID, c.PrincipalID, c.Scope,
			nullIfEmpty(c.Nonce), c.CodeChallenge, c.CodeChallengeMethod, nullIfEmpty(c.DPoPJKT), c.ExpiresAt)
		return err
	})
}

// ConsumeAuthCode marks a code used and returns it. A code presented twice
// returns ErrCodeReplayed with the code (so the caller can end its session,
// RFC 6749 §4.1.2); an unknown or expired one ErrCodeInvalid.
func (s *PgStore) ConsumeAuthCode(ctx context.Context, codeHash string, now time.Time) (*domain.AuthorizationCode, error) {
	var c domain.AuthorizationCode
	var nonce, jkt *string
	err := s.withTokenLookup(ctx, func(tx pgx.Tx) error {
		if err := lockByTenant(ctx, tx, `SELECT tenant_id FROM oauth_authorization_codes WHERE code_hash = $1`, codeHash); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCodeInvalid
			}
			return err
		}
		err := tx.QueryRow(ctx, `SELECT code_hash, tenant_id, client_id, redirect_uri, sid, principal_id, scope,
			    nonce, code_challenge, code_challenge_method, dpop_jkt, expires_at, consumed_at
			FROM oauth_authorization_codes WHERE code_hash = $1 FOR UPDATE`, codeHash).Scan(
			&c.CodeHash, &c.TenantID, &c.ClientID, &c.RedirectURI, &c.SID, &c.PrincipalID, &c.Scope,
			&nonce, &c.CodeChallenge, &c.CodeChallengeMethod, &jkt, &c.ExpiresAt, &c.ConsumedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrCodeInvalid
		}
		if err != nil {
			return err
		}
		if c.ConsumedAt != nil {
			return domain.ErrCodeReplayed
		}
		if !now.Before(c.ExpiresAt) {
			return domain.ErrCodeInvalid
		}
		_, err = tx.Exec(ctx, `UPDATE oauth_authorization_codes SET consumed_at = $2 WHERE code_hash = $1`, codeHash, now)
		return err
	})
	if nonce != nil {
		c.Nonce = *nonce
	}
	if jkt != nil {
		c.DPoPJKT = *jkt
	}
	if errors.Is(err, domain.ErrCodeReplayed) {
		return &c, err
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ── Refresh tokens ──────────────────────────────────────────────────────────

// InsertRefreshToken stores a (hashed) refresh token.
func (s *PgStore) InsertRefreshToken(ctx context.Context, t domain.RefreshToken) error {
	return s.withRLS(ctx, t.TenantID, func(tx pgx.Tx) error {
		return insertRefresh(ctx, tx, t)
	})
}

func insertRefresh(ctx context.Context, tx pgx.Tx, t domain.RefreshToken) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO oauth_refresh_tokens (token_hash, family_id, tenant_id, principal_id, client_id, sid, scope,
		    dpop_jkt, issued_at, expires_at, family_expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		t.TokenHash, t.FamilyID, t.TenantID, t.PrincipalID, t.ClientID, t.SID, t.Scope,
		nullIfEmpty(t.DPoPJKT), t.IssuedAt, t.ExpiresAt, t.FamilyExpiresAt)
	return err
}

// RotateRefreshToken redeems presented and stores next in its family, in one
// transaction. A token already used or revoked is REUSE (RFC 9700 §4.14.2):
// the whole family and its login session are revoked, and ErrRefreshReused
// is returned with the token so the caller can end the session's envelopes.
// check runs on the found token before rotation (client/DPoP binding); an
// error from it aborts without consuming the token.
func (s *PgStore) RotateRefreshToken(ctx context.Context, presentedHash string, now time.Time,
	check func(*domain.RefreshToken) error, next func(*domain.RefreshToken) domain.RefreshToken) (*domain.RefreshToken, error) {
	var t domain.RefreshToken
	var jkt, reason *string
	err := s.withTokenLookup(ctx, func(tx pgx.Tx) error {
		if err := lockByTenant(ctx, tx, `SELECT tenant_id FROM oauth_refresh_tokens WHERE token_hash = $1`, presentedHash); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRefreshInvalid
			}
			return err
		}
		err := tx.QueryRow(ctx, `SELECT token_hash, family_id, tenant_id, principal_id, client_id, sid, scope, dpop_jkt,
			    issued_at, expires_at, family_expires_at, used_at, revoked_at, revoke_reason
			FROM oauth_refresh_tokens WHERE token_hash = $1 FOR UPDATE`, presentedHash).Scan(
			&t.TokenHash, &t.FamilyID, &t.TenantID, &t.PrincipalID, &t.ClientID, &t.SID, &t.Scope, &jkt,
			&t.IssuedAt, &t.ExpiresAt, &t.FamilyExpiresAt, &t.UsedAt, &t.RevokedAt, &reason)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrRefreshInvalid
		}
		if err != nil {
			return err
		}
		if jkt != nil {
			t.DPoPJKT = *jkt
		}
		if t.UsedAt != nil || (t.RevokedAt != nil && reason != nil && *reason == "ROTATED") {
			if _, err := tx.Exec(ctx, `UPDATE oauth_refresh_tokens SET revoked_at = COALESCE(revoked_at, $2),
				revoke_reason = COALESCE(revoke_reason, $3) WHERE family_id = $1`, t.FamilyID, now, domain.SessionEndRefreshReuse); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE auth_sessions SET ended_at = $2, end_reason = $3
				WHERE sid = $1 AND ended_at IS NULL`, t.SID, now, domain.SessionEndRefreshReuse); err != nil {
				return err
			}
			return errCommitThen{domain.ErrRefreshReused}
		}
		if t.RevokedAt != nil || !now.Before(t.ExpiresAt) || !now.Before(t.FamilyExpiresAt) {
			return domain.ErrRefreshInvalid
		}
		if check != nil {
			if err := check(&t); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE oauth_refresh_tokens SET used_at = $2 WHERE token_hash = $1`, presentedHash, now); err != nil {
			return err
		}
		return insertRefresh(ctx, tx, next(&t))
	})
	var commitThen errCommitThen
	if errors.As(err, &commitThen) {
		return &t, commitThen.err
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// errCommitThen makes withTokenLookup's fn commit its writes and still
// return an error to the caller.
type errCommitThen struct{ err error }

func (e errCommitThen) Error() string { return e.err.Error() }

// RevokeRefreshToken revokes the family of a presented refresh token
// (RFC 7009). Unknown tokens are not an error (RFC 7009 §2.2).
func (s *PgStore) RevokeRefreshToken(ctx context.Context, tokenHash, clientID string, now time.Time) error {
	return s.withTokenLookup(ctx, func(tx pgx.Tx) error {
		var family, tenant, owner string
		err := tx.QueryRow(ctx, `SELECT family_id, tenant_id, client_id FROM oauth_refresh_tokens WHERE token_hash = $1`,
			tokenHash).Scan(&family, &tenant, &owner)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != clientID) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := setTenant(ctx, tx, tenant); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE oauth_refresh_tokens SET revoked_at = COALESCE(revoked_at, $2),
			revoke_reason = COALESCE(revoke_reason, 'CLIENT_REVOKED') WHERE family_id = $1`, family, now)
		return err
	})
}

// RevokeAccessToken denies an access token's jti until it expires.
func (s *PgStore) RevokeAccessToken(ctx context.Context, tenantID, jti string, expiresAt time.Time) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO oauth_revoked_tokens (jti, tenant_id, expires_at) VALUES ($1,$2,$3)
			ON CONFLICT (jti) DO NOTHING`, jti, tenantID, expiresAt)
		return err
	})
}

// AccessTokenRevoked reports whether jti was revoked.
func (s *PgStore) AccessTokenRevoked(ctx context.Context, tenantID, jti string) (bool, error) {
	var n int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM oauth_revoked_tokens WHERE jti = $1 AND tenant_id = $2`, jti, tenantID).Scan(&n)
	})
	return n > 0, err
}

// PurgeExpiredOAuth deletes expired codes, revoked-jti rows and refresh
// tokens past their family lifetime (retention worker). Uses the
// retention-sweep capability like the other cross-tenant purges.
func (s *PgStore) PurgeExpiredOAuth(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, "SELECT set_config('app.retention_sweep', 'true', true)"); err != nil {
		return 0, err
	}
	for _, q := range []string{
		`DELETE FROM oauth_authorization_codes WHERE expires_at < $1`,
		`DELETE FROM oauth_revoked_tokens WHERE expires_at < $1`,
		`DELETE FROM oauth_refresh_tokens WHERE family_expires_at < $1`,
		`DELETE FROM credential_tokens WHERE expires_at < $1`,
	} {
		tag, err := tx.Exec(ctx, q, before)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, tx.Commit(ctx)
}
