package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oklog/ulid/v2"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// ErrPrincipalConflict is a subject whose email or external id already
// exists in the tenant.
var ErrPrincipalConflict = errors.New("a principal with this email or external id already exists")

// CreatePrincipal inserts a subject and its creation evidence, with its
// events, in one transaction (AU-09).
func (s *PgStore) CreatePrincipal(ctx context.Context, np domain.NewPrincipal, recs ...outbox.Record) error {
	if np.IdentityProviderSubject == "" {
		np.IdentityProviderSubject = "local:" + np.PrincipalID
	}
	now := time.Now().UTC()
	err := s.withRLS(ctx, np.TenantID, func(tx pgx.Tx) error {
		var invited *time.Time
		if np.Status == domain.PrincipalStatusPendingActivation {
			invited = &now
		}
		var activated *time.Time
		if np.Status == domain.PrincipalStatusActive {
			activated = &now
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO principals (principal_id, tenant_id, principal_type, identity_provider_subject, email,
			    display_name, status, created_at, updated_at, external_id, source, created_by, invited_at, activated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,$10,$11,$12,$13)`,
			np.PrincipalID, np.TenantID, string(np.PrincipalType), np.IdentityProviderSubject,
			strings.ToLower(strings.TrimSpace(np.Email)), np.DisplayName, string(np.Status), now,
			nullIfEmpty(np.ExternalID), np.Source, nullIfEmpty(np.CreatedBy), invited, activated); err != nil {
			return err
		}
		if err := s.appendDecision(ctx, tx, np.PrincipalID, np.TenantID, "SUBJECT_CREATED", "APPLIED",
			np.Source+":"+np.CreatedBy, "", now); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrPrincipalConflict
	}
	return err
}

// PrincipalDetail is a principal with its lifecycle columns.
type PrincipalDetail struct {
	domain.Principal
	ExternalID  string     `json:"external_id,omitempty"`
	Source      string     `json:"source"`
	InvitedAt   *time.Time `json:"invited_at,omitempty"`
	ActivatedAt *time.Time `json:"activated_at,omitempty"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

const principalDetailCols = `principal_id, tenant_id, principal_type, identity_provider_subject, email, display_name,
	status, created_at, data_classification, COALESCE(external_id,''), source, invited_at, activated_at, disabled_at, updated_at`

func scanPrincipalDetail(row pgx.Row) (*PrincipalDetail, error) {
	var p PrincipalDetail
	err := row.Scan(&p.PrincipalID, &p.TenantID, &p.PrincipalType, &p.IdentityProviderSubject, &p.Email,
		&p.DisplayName, &p.Status, &p.CreatedAt, &p.DataClassification, &p.ExternalID, &p.Source,
		&p.InvitedAt, &p.ActivatedAt, &p.DisabledAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// GetPrincipalDetail reads one subject; nil when absent.
func (s *PgStore) GetPrincipalDetail(ctx context.Context, tenantID, principalID string) (*PrincipalDetail, error) {
	var out *PrincipalDetail
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPrincipalDetail(tx.QueryRow(ctx, `SELECT `+principalDetailCols+` FROM principals
			WHERE principal_id = $1 AND tenant_id = $2`, principalID, tenantID))
		return err
	})
	return out, err
}

// FindPrincipalBy finds a subject by "email", "external_id" or
// "identity_provider_subject"; nil when absent.
func (s *PgStore) FindPrincipalBy(ctx context.Context, tenantID, field, value string) (*PrincipalDetail, error) {
	var where string
	switch field {
	case "email":
		where = "LOWER(email) = LOWER($2)"
	case "external_id":
		where = "external_id = $2"
	case "identity_provider_subject":
		where = "identity_provider_subject = $2"
	default:
		return nil, fmt.Errorf("unsupported principal lookup %q", field)
	}
	var out *PrincipalDetail
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPrincipalDetail(tx.QueryRow(ctx, `SELECT `+principalDetailCols+` FROM principals
			WHERE tenant_id = $1 AND `+where+` ORDER BY created_at LIMIT 1`, tenantID, value))
		return err
	})
	return out, err
}

// ListPrincipals pages through a tenant's subjects (SCIM list, admin list).
// filterField is "", "email" or "external_id".
func (s *PgStore) ListPrincipals(ctx context.Context, tenantID, filterField, filterValue string, offset, limit int) ([]PrincipalDetail, int, error) {
	where := "tenant_id = $1"
	args := []any{tenantID}
	switch filterField {
	case "email":
		where += " AND LOWER(email) = LOWER($2)"
		args = append(args, filterValue)
	case "external_id":
		where += " AND external_id = $2"
		args = append(args, filterValue)
	}
	var out []PrincipalDetail
	var total int
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM principals WHERE `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT %s FROM principals WHERE %s ORDER BY created_at, principal_id
			OFFSET %d LIMIT %d`, principalDetailCols, where, offset, limit), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPrincipalDetail(rows)
			if err != nil {
				return err
			}
			out = append(out, *p)
		}
		return rows.Err()
	})
	return out, total, err
}

// UpdatePrincipalProfile changes display name, email and external id.
func (s *PgStore) UpdatePrincipalProfile(ctx context.Context, tenantID, principalID, email, displayName, externalID string) error {
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE principals SET
			    email = COALESCE(NULLIF($3,''), email),
			    display_name = COALESCE(NULLIF($4,''), display_name),
			    external_id = COALESCE(NULLIF($5,''), external_id),
			    updated_at = NOW()
			WHERE principal_id = $1 AND tenant_id = $2`, principalID, tenantID,
			strings.ToLower(strings.TrimSpace(email)), displayName, externalID)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrPrincipalConflict
	}
	return err
}

// SetPrincipalLifecycle moves a subject to status with its evidence and
// events in one transaction; reason is recorded on a disable. Returns false
// when the subject was already in that status.
func (s *PgStore) SetPrincipalLifecycle(ctx context.Context, tenantID, principalID string, status domain.PrincipalStatus,
	actor, reason, correlationID string, recs ...outbox.Record) (bool, error) {
	changed := false
	now := time.Now().UTC()
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var disabledAt *time.Time
		var disabledReason *string
		var activatedAt *time.Time
		switch status {
		case domain.PrincipalStatusDisabled, domain.PrincipalStatusSuspended:
			disabledAt, disabledReason = &now, &reason
		case domain.PrincipalStatusActive:
			activatedAt = &now
		}
		tag, err := tx.Exec(ctx, `UPDATE principals SET status = $3, updated_at = $4,
			    disabled_at = CASE WHEN $5::timestamptz IS NOT NULL THEN $5 WHEN $3 = 'ACTIVE' THEN NULL ELSE disabled_at END,
			    disabled_reason = CASE WHEN $5::timestamptz IS NOT NULL THEN $6 WHEN $3 = 'ACTIVE' THEN NULL ELSE disabled_reason END,
			    activated_at = COALESCE(activated_at, $7)
			WHERE principal_id = $1 AND tenant_id = $2 AND status <> $3`,
			principalID, tenantID, string(status), now, disabledAt, disabledReason, activatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		changed = true
		if err := s.appendDecision(ctx, tx, principalID, tenantID, "STATUS_TRANSITION", "APPLIED",
			string(status)+":"+actor, correlationID, now); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	return changed, err
}

// ── One-time links ──────────────────────────────────────────────────────────

// IssueCredentialToken stores a link, revoking any earlier unconsumed link of
// the same purpose for the principal (only the newest works).
func (s *PgStore) IssueCredentialToken(ctx context.Context, t domain.CredentialToken) error {
	return s.withRLS(ctx, t.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE credential_tokens SET revoked_at = NOW()
			WHERE tenant_id = $1 AND principal_id = $2 AND purpose = $3 AND consumed_at IS NULL AND revoked_at IS NULL`,
			t.TenantID, t.PrincipalID, t.Purpose); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO credential_tokens (token_hash, tenant_id, principal_id, purpose, request_id, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6)`, t.TokenHash, t.TenantID, t.PrincipalID, t.Purpose, nullIfEmpty(t.RequestID), t.ExpiresAt)
		return err
	})
}

// ConsumeCredentialToken redeems a link of purpose and, in the same
// transaction, runs apply (set the password, activate) and enqueues recs.
// A used, revoked, expired or unknown link is ErrCredentialTokenUsed.
func (s *PgStore) ConsumeCredentialToken(ctx context.Context, tokenHash, purpose string, now time.Time,
	apply func(ctx context.Context, tx pgx.Tx, t domain.CredentialToken) ([]outbox.Record, error)) (*domain.CredentialToken, error) {
	var t domain.CredentialToken
	err := s.withTokenLookup(ctx, func(tx pgx.Tx) error {
		if err := lockByTenant(ctx, tx, `SELECT tenant_id FROM credential_tokens WHERE token_hash = $1`, tokenHash); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrCredentialTokenUsed
			}
			return err
		}
		var consumed, revoked *time.Time
		var req *string
		err := tx.QueryRow(ctx, `SELECT tenant_id, principal_id, purpose, request_id, expires_at, consumed_at, revoked_at
			FROM credential_tokens WHERE token_hash = $1 FOR UPDATE`, tokenHash).Scan(
			&t.TenantID, &t.PrincipalID, &t.Purpose, &req, &t.ExpiresAt, &consumed, &revoked)
		if err != nil {
			return err
		}
		if req != nil {
			t.RequestID = *req
		}
		t.TokenHash = tokenHash
		if t.Purpose != purpose || consumed != nil || revoked != nil || !now.Before(t.ExpiresAt) {
			return domain.ErrCredentialTokenUsed
		}
		if _, err := tx.Exec(ctx, `UPDATE credential_tokens SET consumed_at = $2 WHERE token_hash = $1`, tokenHash, now); err != nil {
			return err
		}
		recs, err := apply(ctx, tx, t)
		if err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SetPasswordTx retires the active password, stores hash and records it in
// the history, inside tx (activation, reset, change).
func (s *PgStore) SetPasswordTx(ctx context.Context, tx pgx.Tx, tenantID, principalID, hash string) error {
	now := time.Now().UTC()
	if !strings.HasPrefix(hash, "$argon2id$") {
		return errors.New("secret hash is not a PHC-encoded argon2id digest")
	}
	if _, err := tx.Exec(ctx, `UPDATE principal_credentials SET status = 'RETIRED', updated_at = $1
		WHERE principal_id = $2 AND tenant_id = $3 AND credential_type = 'PASSWORD' AND status = 'ACTIVE'`,
		now, principalID, tenantID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO principal_credentials (credential_id, principal_id, tenant_id, credential_type,
		    secret_hash, algorithm, status, failed_attempt_count, secret_updated_at, created_at, updated_at)
		VALUES ($1,$2,$3,'PASSWORD',$4,'argon2id','ACTIVE',0,$5,$5,$5)`,
		ulid.Make().String(), principalID, tenantID, hash, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO password_history (principal_id, tenant_id, secret_hash, created_at) VALUES ($1,$2,$3,$4)`,
		principalID, tenantID, hash, now)
	return err
}

// ActivatePrincipalTx moves a PENDING_ACTIVATION subject to ACTIVE inside tx.
func (s *PgStore) ActivatePrincipalTx(ctx context.Context, tx pgx.Tx, tenantID, principalID string) error {
	_, err := tx.Exec(ctx, `UPDATE principals SET status = 'ACTIVE', activated_at = NOW(), updated_at = NOW()
		WHERE principal_id = $1 AND tenant_id = $2 AND status = 'PENDING_ACTIVATION'`, principalID, tenantID)
	return err
}

// ChangePassword stores a new password (self-service change) with history
// and events.
func (s *PgStore) ChangePassword(ctx context.Context, tenantID, principalID, hash string, recs ...outbox.Record) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := s.SetPasswordTx(ctx, tx, tenantID, principalID, hash); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
}

// RecentPasswordHashes returns the last n password digests (reuse check).
func (s *PgStore) RecentPasswordHashes(ctx context.Context, tenantID, principalID string, n int) ([]string, error) {
	var out []string
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT secret_hash FROM password_history WHERE tenant_id = $1 AND principal_id = $2
			ORDER BY created_at DESC LIMIT $3`, tenantID, principalID, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// ── Governed password reset requests ────────────────────────────────────────

// CreateResetRequest stores an administrator-initiated reset request.
func (s *PgStore) CreateResetRequest(ctx context.Context, r domain.PasswordResetRequest, recs ...outbox.Record) error {
	return s.withRLS(ctx, r.TenantID, func(tx pgx.Tx) error {
		approvals, _ := json.Marshal(r.Approvals)
		if _, err := tx.Exec(ctx, `INSERT INTO password_reset_requests (request_id, tenant_id, principal_id, requested_by,
			    reason, approvals_required, approvals, status, created_at, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, r.RequestID, r.TenantID, r.PrincipalID, r.RequestedBy, r.Reason,
			r.ApprovalsRequired, approvals, r.Status, r.CreatedAt, r.ExpiresAt); err != nil {
			return err
		}
		return s.enqueueAll(ctx, tx, recs)
	})
}

// GetResetRequest reads a reset request; nil when absent.
func (s *PgStore) GetResetRequest(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error) {
	var r domain.PasswordResetRequest
	var approvals []byte
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT request_id, tenant_id, principal_id, requested_by, reason, approvals_required,
			    approvals, status, created_at, decided_at, expires_at
			FROM password_reset_requests WHERE request_id = $1 AND tenant_id = $2`, requestID, tenantID).Scan(
			&r.RequestID, &r.TenantID, &r.PrincipalID, &r.RequestedBy, &r.Reason, &r.ApprovalsRequired,
			&approvals, &r.Status, &r.CreatedAt, &r.DecidedAt, &r.ExpiresAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(approvals, &r.Approvals)
	return &r, nil
}

// DecideResetRequest records one approver's decision under a row lock and
// returns the updated request. issue runs (in the same transaction) when the
// request reaches its approval count, to store the reset link.
func (s *PgStore) DecideResetRequest(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
	issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error) {
	var r domain.PasswordResetRequest
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var approvals []byte
		err := tx.QueryRow(ctx, `SELECT request_id, tenant_id, principal_id, requested_by, reason, approvals_required,
			    approvals, status, created_at, decided_at, expires_at
			FROM password_reset_requests WHERE request_id = $1 AND tenant_id = $2 FOR UPDATE`, requestID, tenantID).Scan(
			&r.RequestID, &r.TenantID, &r.PrincipalID, &r.RequestedBy, &r.Reason, &r.ApprovalsRequired,
			&approvals, &r.Status, &r.CreatedAt, &r.DecidedAt, &r.ExpiresAt)
		if err != nil {
			return err
		}
		_ = json.Unmarshal(approvals, &r.Approvals)
		if r.Status != "PENDING_APPROVAL" {
			return fmt.Errorf("reset request is %s", r.Status)
		}
		if !time.Now().Before(r.ExpiresAt) {
			_, _ = tx.Exec(ctx, `UPDATE password_reset_requests SET status = 'EXPIRED', decided_at = NOW() WHERE request_id = $1`, requestID)
			return errors.New("reset request expired")
		}
		for _, prior := range r.Approvals {
			if prior.ApproverID == a.ApproverID {
				return errors.New("this approver has already decided")
			}
		}
		r.Approvals = append(r.Approvals, a)
		now := time.Now().UTC()
		if a.Decision == "REJECT" {
			r.Status, r.DecidedAt = "REJECTED", &now
		} else {
			approved := 0
			for _, x := range r.Approvals {
				if x.Decision == "APPROVE" {
					approved++
				}
			}
			if approved >= r.ApprovalsRequired {
				r.Status, r.DecidedAt = "ISSUED", &now
				if err := issue(ctx, tx, &r); err != nil {
					return err
				}
			}
		}
		raw, _ := json.Marshal(r.Approvals)
		if _, err := tx.Exec(ctx, `UPDATE password_reset_requests SET approvals = $2, status = $3, decided_at = $4
			WHERE request_id = $1`, requestID, raw, r.Status, r.DecidedAt); err != nil {
			return err
		}
		if r.Status == "PENDING_APPROVAL" {
			return nil
		}
		return s.enqueueAll(ctx, tx, recs)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// CompleteResetRequestTx marks the request COMPLETED when its link is used.
func (s *PgStore) CompleteResetRequestTx(ctx context.Context, tx pgx.Tx, requestID string) error {
	if requestID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE password_reset_requests SET status = 'COMPLETED' WHERE request_id = $1 AND status = 'ISSUED'`, requestID)
	return err
}

// InsertCredentialTokenTx stores a link inside an open transaction.
func (s *PgStore) InsertCredentialTokenTx(ctx context.Context, tx pgx.Tx, t domain.CredentialToken) error {
	if _, err := tx.Exec(ctx, `UPDATE credential_tokens SET revoked_at = NOW()
		WHERE tenant_id = $1 AND principal_id = $2 AND purpose = $3 AND consumed_at IS NULL AND revoked_at IS NULL`,
		t.TenantID, t.PrincipalID, t.Purpose); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO credential_tokens (token_hash, tenant_id, principal_id, purpose, request_id, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, t.TokenHash, t.TenantID, t.PrincipalID, t.Purpose, nullIfEmpty(t.RequestID), t.ExpiresAt)
	return err
}
