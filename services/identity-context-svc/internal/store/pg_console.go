package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"zoiko.io/identity-context-svc/internal/domain"
)

// Console read models (IC-2). Every list here pages by offset inside a
// cursor the handler signs and binds to tenant, route and filter
// (identity-envelope/apicontract), so a page can only be continued by the
// tenant and query that started it. The store fetches limit rows; callers ask
// for one more than they show to learn whether a next page exists.

// PrincipalListFilter narrows GET /v1/principals.
type PrincipalListFilter struct {
	Status        string
	PrincipalType string
	// Query matches principal id, email or display name, case-insensitively.
	Query string
}

// ListPrincipalsPage returns one page of a tenant's principals, newest first.
func (s *PgStore) ListPrincipalsPage(ctx context.Context, tenantID string, f PrincipalListFilter, offset, limit int) ([]PrincipalDetail, error) {
	where := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if f.Status != "" {
		args = append(args, f.Status)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.PrincipalType != "" {
		args = append(args, f.PrincipalType)
		where = append(where, fmt.Sprintf("principal_type = $%d", len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+escapeLike(q)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf("(principal_id ILIKE $%d OR email ILIKE $%d OR display_name ILIKE $%d)", n, n, n))
	}
	args = append(args, offset, limit)
	query := `SELECT ` + principalDetailCols + `, record_version FROM principals WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY created_at DESC, principal_id DESC OFFSET $%d LIMIT $%d`, len(args)-1, len(args))

	out := []PrincipalDetail{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p PrincipalDetail
			if err := rows.Scan(&p.PrincipalID, &p.TenantID, &p.PrincipalType, &p.IdentityProviderSubject, &p.Email,
				&p.DisplayName, &p.Status, &p.CreatedAt, &p.DataClassification, &p.ExternalID, &p.Source,
				&p.InvitedAt, &p.ActivatedAt, &p.DisabledAt, &p.UpdatedAt, &p.RecordVersion); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// Session states derived for the console. A session is ACTIVE until it is
// invalidated or reaches expires_at.
const (
	SessionStateActive      = "ACTIVE"
	SessionStateExpired     = "EXPIRED"
	SessionStateInvalidated = "INVALIDATED"
)

// SessionListFilter narrows GET /v1/context/sessions.
type SessionListFilter struct {
	PrincipalID string
	State       string
}

// SessionSummary is one row of the Sessions register: enough to find and end
// a session, never the envelope itself.
type SessionSummary struct {
	SessionContextID   string     `json:"session_context_id"`
	PrincipalID        string     `json:"principal_id"`
	DisplayName        string     `json:"display_name"`
	PrincipalType      string     `json:"principal_type"`
	LegalEntityID      string     `json:"legal_entity_id,omitempty"`
	TrustPosture       string     `json:"trust_posture"`
	MFAVerified        bool       `json:"mfa_verified"`
	AdaptiveRiskScore  int        `json:"adaptive_risk_score"`
	SourceChannel      string     `json:"source_channel,omitempty"`
	SupportContextID   string     `json:"support_context_id,omitempty"`
	IssuedAt           time.Time  `json:"issued_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	InvalidatedAt      *time.Time `json:"invalidated_at,omitempty"`
	InvalidationReason string     `json:"invalidation_reason,omitempty"`
	State              string     `json:"state"`
}

// ListSessionsPage returns one page of a tenant's session contexts, newest
// first, with the holder's display name.
func (s *PgStore) ListSessionsPage(ctx context.Context, tenantID string, f SessionListFilter, offset, limit int) ([]SessionSummary, error) {
	where := []string{"sc.tenant_id = $1"}
	args := []any{tenantID}
	if f.PrincipalID != "" {
		args = append(args, f.PrincipalID)
		where = append(where, fmt.Sprintf("sc.principal_id = $%d", len(args)))
	}
	switch f.State {
	case "":
	case SessionStateActive:
		where = append(where, "sc.invalidated_at IS NULL AND sc.expires_at > NOW()")
	case SessionStateExpired:
		where = append(where, "sc.invalidated_at IS NULL AND sc.expires_at <= NOW()")
	case SessionStateInvalidated:
		where = append(where, "sc.invalidated_at IS NOT NULL")
	default:
		return nil, fmt.Errorf("%w: session state %q", ErrInvalidFilter, f.State)
	}
	args = append(args, offset, limit)
	query := `
		SELECT sc.session_context_id, sc.principal_id, COALESCE(p.display_name, ''), COALESCE(p.principal_type, ''),
		       COALESCE(sc.legal_entity_id, ''), sc.trust_posture, sc.mfa_verified, sc.adaptive_risk_score,
		       COALESCE(sc.source_channel, ''), COALESCE(sc.support_context_id, ''),
		       sc.issued_at, sc.expires_at, sc.invalidated_at, COALESCE(sc.invalidation_reason, ''),
		       CASE WHEN sc.invalidated_at IS NOT NULL THEN 'INVALIDATED'
		            WHEN sc.expires_at <= NOW() THEN 'EXPIRED' ELSE 'ACTIVE' END
		  FROM session_contexts sc
		  LEFT JOIN principals p ON p.principal_id = sc.principal_id AND p.tenant_id = sc.tenant_id
		 WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY sc.issued_at DESC, sc.session_context_id DESC OFFSET $%d LIMIT $%d`, len(args)-1, len(args))

	out := []SessionSummary{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r SessionSummary
			if err := rows.Scan(&r.SessionContextID, &r.PrincipalID, &r.DisplayName, &r.PrincipalType,
				&r.LegalEntityID, &r.TrustPosture, &r.MFAVerified, &r.AdaptiveRiskScore,
				&r.SourceChannel, &r.SupportContextID,
				&r.IssuedAt, &r.ExpiresAt, &r.InvalidatedAt, &r.InvalidationReason, &r.State); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// Support context states derived for the console.
const (
	SupportStatePending = "PENDING_APPROVAL"
	SupportStateActive  = "ACTIVE"
	SupportStateRevoked = "REVOKED"
	SupportStateExpired = "EXPIRED"
)

// ListSupportContextsPage returns one page of a tenant's support access
// requests and grants, newest first. state is "" or one of SupportState*.
func (s *PgStore) ListSupportContextsPage(ctx context.Context, tenantID, state string, offset, limit int) ([]domain.SupportContext, error) {
	cond := ""
	switch state {
	case "":
	case SupportStatePending:
		cond = " AND approval_status = 'PENDING_APPROVAL' AND revoked_at IS NULL"
	case SupportStateActive:
		cond = " AND approval_status = 'APPROVED' AND revoked_at IS NULL AND expires_at > NOW()"
	case SupportStateRevoked:
		cond = " AND revoked_at IS NOT NULL"
	case SupportStateExpired:
		cond = " AND revoked_at IS NULL AND approval_status = 'APPROVED' AND expires_at <= NOW()"
	default:
		return nil, fmt.Errorf("%w: support state %q", ErrInvalidFilter, state)
	}
	out := []domain.SupportContext{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, supportContextColumns+`
			 WHERE tenant_id = $1`+cond+`
			 ORDER BY granted_at DESC, support_context_id DESC OFFSET $2 LIMIT $3`,
			tenantID, offset, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sc domain.SupportContext
			if err := scanSupportContext(rows, &sc); err != nil {
				return err
			}
			out = append(out, sc)
		}
		return rows.Err()
	})
	return out, err
}

// ResetListFilter narrows GET /v1/password-resets.
type ResetListFilter struct {
	Status      string
	PrincipalID string
}

// resetStatusExpr is a reset request's status as the console shows it: a
// request still awaiting approval, or issued and unused, past its expiry is
// EXPIRED, whether or not anything has touched the row since.
const resetStatusExpr = `CASE WHEN status IN ('PENDING_APPROVAL', 'ISSUED') AND expires_at <= NOW() THEN 'EXPIRED' ELSE status END`

// ListPasswordResetsPage returns one page of a tenant's governed password
// resets, newest first: the Password resets register.
func (s *PgStore) ListPasswordResetsPage(ctx context.Context, tenantID string, f ResetListFilter, offset, limit int) ([]domain.PasswordResetRequest, error) {
	where := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if f.Status != "" {
		switch f.Status {
		case "PENDING_APPROVAL", "ISSUED", "REJECTED", "COMPLETED", "EXPIRED":
		default:
			return nil, fmt.Errorf("%w: reset status %q", ErrInvalidFilter, f.Status)
		}
		args = append(args, f.Status)
		where = append(where, fmt.Sprintf("(%s) = $%d", resetStatusExpr, len(args)))
	}
	if f.PrincipalID != "" {
		args = append(args, f.PrincipalID)
		where = append(where, fmt.Sprintf("principal_id = $%d", len(args)))
	}
	args = append(args, offset, limit)
	query := `SELECT request_id, tenant_id, principal_id, requested_by, reason, approvals_required,
		       approvals, ` + resetStatusExpr + `, created_at, decided_at, expires_at
		  FROM password_reset_requests WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY created_at DESC, request_id DESC OFFSET $%d LIMIT $%d`, len(args)-1, len(args))

	out := []domain.PasswordResetRequest{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r domain.PasswordResetRequest
			var approvals []byte
			if err := rows.Scan(&r.RequestID, &r.TenantID, &r.PrincipalID, &r.RequestedBy, &r.Reason, &r.ApprovalsRequired,
				&approvals, &r.Status, &r.CreatedAt, &r.DecidedAt, &r.ExpiresAt); err != nil {
				return err
			}
			if err := json.Unmarshal(approvals, &r.Approvals); err != nil {
				return fmt.Errorf("reset %s approvals: %w", r.RequestID, err)
			}
			if r.Approvals == nil {
				r.Approvals = []domain.ResetApproval{}
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// AuthEventFilter narrows GET /v1/authentication-events.
type AuthEventFilter struct {
	PrincipalID string
	Outcome     string
	Since       *time.Time
}

// ListAuthenticationEventsPage returns one page of a tenant's authentication
// events, newest first (ORG-1 ListAuthenticationEvents).
func (s *PgStore) ListAuthenticationEventsPage(ctx context.Context, tenantID string, f AuthEventFilter, offset, limit int) ([]domain.AuthenticationEvent, error) {
	where := []string{"tenant_id = $1"}
	args := []any{tenantID}
	if f.PrincipalID != "" {
		args = append(args, f.PrincipalID)
		where = append(where, fmt.Sprintf("principal_id = $%d", len(args)))
	}
	if f.Outcome != "" {
		args = append(args, f.Outcome)
		where = append(where, fmt.Sprintf("outcome = $%d", len(args)))
	}
	if f.Since != nil {
		args = append(args, *f.Since)
		where = append(where, fmt.Sprintf("occurred_at >= $%d", len(args)))
	}
	args = append(args, offset, limit)
	query := `SELECT event_id, tenant_id, principal_id, event_type, outcome, COALESCE(assurance_level, ''),
		       COALESCE(source_ip, ''), COALESCE(user_agent, ''), occurred_at, COALESCE(correlation_id, ''), details
		  FROM authentication_events WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY occurred_at DESC, event_id DESC OFFSET $%d LIMIT $%d`, len(args)-1, len(args))

	out := []domain.AuthenticationEvent{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.AuthenticationEvent
			if err := rows.Scan(&e.EventID, &e.TenantID, &e.PrincipalID, &e.EventType, &e.Outcome,
				&e.AssuranceLevel, &e.SourceIP, &e.UserAgent, &e.OccurredAt, &e.CorrelationID, &e.Details); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// AssuranceChange is one row of a principal's assurance history.
type AssuranceChange struct {
	HistoryID      string    `json:"history_id"`
	PrincipalID    string    `json:"principal_id"`
	AssuranceLevel string    `json:"assurance_level"`
	ChangedAt      time.Time `json:"changed_at"`
	ChangedBy      string    `json:"changed_by,omitempty"`
	Reason         string    `json:"reason,omitempty"`
}

// ListAssuranceHistoryPage returns one page of a principal's assurance
// changes, newest first (ORG-1 GetAssuranceHistory).
func (s *PgStore) ListAssuranceHistoryPage(ctx context.Context, tenantID, principalID string, offset, limit int) ([]AssuranceChange, error) {
	out := []AssuranceChange{}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT history_id, principal_id, assurance_level, changed_at, COALESCE(changed_by, ''), COALESCE(reason, '')
			  FROM principal_assurance_history
			 WHERE tenant_id = $1 AND principal_id = $2
			 ORDER BY changed_at DESC, history_id DESC OFFSET $3 LIMIT $4`,
			tenantID, principalID, offset, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a AssuranceChange
			if err := rows.Scan(&a.HistoryID, &a.PrincipalID, &a.AssuranceLevel, &a.ChangedAt, &a.ChangedBy, &a.Reason); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// RoleMFACoverage is how many current holders of a role have a second factor.
type RoleMFACoverage struct {
	RoleID  string `json:"role_id"`
	Holders int    `json:"holders"`
	WithMFA int    `json:"with_mfa"`
}

// IdentitySummary is GET /v1/identity/summary: the console's KPI and panel
// counts, all read in one transaction so they describe the same instant.
type IdentitySummary struct {
	GeneratedAt             time.Time         `json:"generated_at"`
	PrincipalsByStatus      map[string]int    `json:"principals_by_status"`
	ActivePrincipalsByType  map[string]int    `json:"active_principals_by_type"`
	ActiveSessions          int               `json:"active_sessions"`
	ActiveSessionsMFA       int               `json:"active_sessions_mfa_verified"`
	ActiveHumans            int               `json:"active_humans"`
	ActiveHumansWithMFA     int               `json:"active_humans_with_mfa"`
	AdminsWithoutMFA        int               `json:"admins_without_mfa"`
	SupportPendingApproval  int               `json:"support_pending_approval"`
	SupportActive           int               `json:"support_active"`
	PasswordResetsPending   int               `json:"password_resets_pending"`
	FailedSignIns24h        int               `json:"failed_sign_ins_24h"`
	SuccessfulSignIns24h    int               `json:"successful_sign_ins_24h"`
	FactorHolders           map[string]int    `json:"factor_holders"`
	Pipeline24h             map[string]int    `json:"pipeline_24h"`
	MFAByRole               []RoleMFACoverage `json:"mfa_by_role"`
	MergedIdentities        int               `json:"merged_identities"`
	SessionInvalidations24h map[string]int    `json:"session_invalidations_24h"`
}

// hasSecondFactor is a SQL predicate on alias p: an active TOTP factor or an
// active passkey.
const hasSecondFactor = `(EXISTS (SELECT 1 FROM principal_credentials c
	    WHERE c.principal_id = p.principal_id AND c.tenant_id = p.tenant_id
	      AND c.credential_type = 'TOTP' AND c.status = 'ACTIVE')
	 OR EXISTS (SELECT 1 FROM webauthn_credentials w
	    WHERE w.principal_id = p.principal_id AND w.tenant_id = p.tenant_id AND w.status = 'ACTIVE'))`

// assignmentNotSuspended is notSuspended on alias a: a suspended assignment
// stays in the projection (000016) but holds nothing until it is resumed.
const assignmentNotSuspended = `
			     AND NOT (a.suspended_at IS NOT NULL AND (a.resumed_at IS NULL OR a.suspended_at > a.resumed_at))`

// GetIdentitySummary counts what the console's KPI cards and panels show.
func (s *PgStore) GetIdentitySummary(ctx context.Context, tenantID string) (*IdentitySummary, error) {
	out := &IdentitySummary{
		GeneratedAt:             time.Now().UTC(),
		PrincipalsByStatus:      map[string]int{},
		ActivePrincipalsByType:  map[string]int{},
		FactorHolders:           map[string]int{},
		Pipeline24h:             map[string]int{},
		MFAByRole:               []RoleMFACoverage{},
		SessionInvalidations24h: map[string]int{},
	}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		groups := []struct {
			sql string
			dst map[string]int
		}{
			{`SELECT status, COUNT(*) FROM principals WHERE tenant_id = $1 GROUP BY status`, out.PrincipalsByStatus},
			{`SELECT principal_type, COUNT(*) FROM principals WHERE tenant_id = $1 AND status = 'ACTIVE' GROUP BY principal_type`, out.ActivePrincipalsByType},
			{`SELECT invalidation_reason, COUNT(*) FROM session_contexts
			   WHERE tenant_id = $1 AND invalidated_at > NOW() - INTERVAL '24 hours' GROUP BY invalidation_reason`, out.SessionInvalidations24h},
		}
		for _, g := range groups {
			if err := scanCounts(ctx, tx, g.sql, tenantID, g.dst); err != nil {
				return err
			}
		}

		counts := []struct {
			sql string
			dst *int
		}{
			{`SELECT COUNT(*) FROM session_contexts WHERE tenant_id = $1 AND invalidated_at IS NULL AND expires_at > NOW()`, &out.ActiveSessions},
			{`SELECT COUNT(*) FROM session_contexts WHERE tenant_id = $1 AND invalidated_at IS NULL AND expires_at > NOW() AND mfa_verified`, &out.ActiveSessionsMFA},
			{`SELECT COUNT(*) FROM principals p WHERE p.tenant_id = $1 AND p.status = 'ACTIVE' AND p.principal_type = 'HUMAN'`, &out.ActiveHumans},
			{`SELECT COUNT(*) FROM principals p WHERE p.tenant_id = $1 AND p.status = 'ACTIVE' AND p.principal_type = 'HUMAN' AND ` + hasSecondFactor, &out.ActiveHumansWithMFA},
			{`SELECT COUNT(DISTINCT p.principal_id) FROM principals p
			    JOIN principal_role_assignments a ON a.principal_id = p.principal_id AND a.tenant_id = p.tenant_id
			   WHERE p.tenant_id = $1 AND p.status = 'ACTIVE' AND p.principal_type = 'HUMAN'
			     AND a.effective_from <= NOW() AND a.effective_to > NOW()` + assignmentNotSuspended + `
			     AND a.role_id ILIKE '%ADMIN%' AND NOT ` + hasSecondFactor, &out.AdminsWithoutMFA},
			{`SELECT COUNT(*) FROM support_contexts WHERE tenant_id = $1 AND approval_status = 'PENDING_APPROVAL' AND revoked_at IS NULL`, &out.SupportPendingApproval},
			{`SELECT COUNT(*) FROM support_contexts WHERE tenant_id = $1 AND approval_status = 'APPROVED' AND revoked_at IS NULL AND expires_at > NOW()`, &out.SupportActive},
			{`SELECT COUNT(*) FROM password_reset_requests WHERE tenant_id = $1 AND status = 'PENDING_APPROVAL' AND expires_at > NOW()`, &out.PasswordResetsPending},
			{`SELECT COUNT(*) FROM authentication_events WHERE tenant_id = $1 AND outcome = 'FAILURE' AND occurred_at > NOW() - INTERVAL '24 hours'`, &out.FailedSignIns24h},
			{`SELECT COUNT(*) FROM authentication_events WHERE tenant_id = $1 AND event_type = 'LOGIN' AND outcome = 'SUCCESS' AND occurred_at > NOW() - INTERVAL '24 hours'`, &out.SuccessfulSignIns24h},
			// Both merge routes: /v1/subjects (identity_merges) and /v1/identities (principal_merges).
			{`SELECT (SELECT COUNT(*) FROM identity_merges WHERE tenant_id = $1)
			       + (SELECT COUNT(*) FROM principal_merges WHERE tenant_id = $1)`, &out.MergedIdentities},
		}
		for _, c := range counts {
			if err := tx.QueryRow(ctx, c.sql, tenantID).Scan(c.dst); err != nil {
				return fmt.Errorf("identity summary: %w", err)
			}
		}

		// Holders of each kind of credential among active principals. Not
		// exclusive: one person may hold a password, a TOTP factor and a passkey.
		var password, totp, passkey, federated int
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT COUNT(DISTINCT c.principal_id) FROM principal_credentials c JOIN principals p
			     ON p.principal_id = c.principal_id AND p.tenant_id = c.tenant_id
			   WHERE c.tenant_id = $1 AND p.status = 'ACTIVE' AND c.credential_type = 'PASSWORD' AND c.status = 'ACTIVE'),
			  (SELECT COUNT(DISTINCT c.principal_id) FROM principal_credentials c JOIN principals p
			     ON p.principal_id = c.principal_id AND p.tenant_id = c.tenant_id
			   WHERE c.tenant_id = $1 AND p.status = 'ACTIVE' AND c.credential_type = 'TOTP' AND c.status = 'ACTIVE'),
			  (SELECT COUNT(DISTINCT w.principal_id) FROM webauthn_credentials w JOIN principals p
			     ON p.principal_id = w.principal_id AND p.tenant_id = w.tenant_id
			   WHERE w.tenant_id = $1 AND p.status = 'ACTIVE' AND w.status = 'ACTIVE'),
			  (SELECT COUNT(DISTINCT x.principal_id) FROM (
			       SELECT principal_id, tenant_id FROM federation_links WHERE tenant_id = $1 AND status = 'ACTIVE'
			       UNION SELECT principal_id, tenant_id FROM federated_identities WHERE tenant_id = $1) x
			     JOIN principals p ON p.principal_id = x.principal_id AND p.tenant_id = x.tenant_id
			   WHERE p.status = 'ACTIVE')`, tenantID).Scan(&password, &totp, &passkey, &federated); err != nil {
			return fmt.Errorf("identity summary factors: %w", err)
		}
		out.FactorHolders["PASSWORD"], out.FactorHolders["TOTP"] = password, totp
		out.FactorHolders["PASSKEY"], out.FactorHolders["FEDERATED"] = passkey, federated

		var signedIn, resolved, steppedUp, ended int
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT COUNT(*) FROM auth_sessions WHERE tenant_id = $1 AND created_at > NOW() - INTERVAL '24 hours'),
			  (SELECT COUNT(*) FROM session_contexts WHERE tenant_id = $1 AND issued_at > NOW() - INTERVAL '24 hours'),
			  (SELECT COUNT(*) FROM authentication_events WHERE tenant_id = $1 AND event_type = 'STEP_UP'
			      AND outcome = 'SUCCESS' AND occurred_at > NOW() - INTERVAL '24 hours'),
			  (SELECT COUNT(*) FROM session_contexts WHERE tenant_id = $1 AND invalidated_at > NOW() - INTERVAL '24 hours')`,
			tenantID).Scan(&signedIn, &resolved, &steppedUp, &ended); err != nil {
			return fmt.Errorf("identity summary pipeline: %w", err)
		}
		out.Pipeline24h["SIGNED_IN"], out.Pipeline24h["CONTEXT_RESOLVED"] = signedIn, resolved
		out.Pipeline24h["STEPPED_UP"], out.Pipeline24h["ENDED"] = steppedUp, ended

		rows, err := tx.Query(ctx, `
			SELECT a.role_id, COUNT(DISTINCT p.principal_id),
			       COUNT(DISTINCT p.principal_id) FILTER (WHERE `+hasSecondFactor+`)
			  FROM principal_role_assignments a
			  JOIN principals p ON p.principal_id = a.principal_id AND p.tenant_id = a.tenant_id
			 WHERE a.tenant_id = $1 AND p.status = 'ACTIVE' AND p.principal_type = 'HUMAN'
			   AND a.effective_from <= NOW() AND a.effective_to > NOW()`+assignmentNotSuspended+`
			 GROUP BY a.role_id ORDER BY 2 DESC, 1 LIMIT 8`, tenantID)
		if err != nil {
			return fmt.Errorf("identity summary roles: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rc RoleMFACoverage
			if err := rows.Scan(&rc.RoleID, &rc.Holders, &rc.WithMFA); err != nil {
				return err
			}
			out.MFAByRole = append(out.MFAByRole, rc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func scanCounts(ctx context.Context, tx pgx.Tx, sql, tenantID string, dst map[string]int) error {
	rows, err := tx.Query(ctx, sql, tenantID)
	if err != nil {
		return fmt.Errorf("identity summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k *string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return err
		}
		if k != nil {
			dst[*k] = n
		}
	}
	return rows.Err()
}

// ErrInvalidFilter reports a filter value the store cannot express.
var ErrInvalidFilter = errors.New("invalid filter value")

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// projectDecision keeps the console read models in step with the decision log:
// called by appendDecision inside the same transaction, so an authentication
// outcome and its sign-in event commit or roll back together.
//
// AUTHENTICATE and the step-up / login second factor become rows of
// authentication_events; a factor activated or retired becomes a row of
// principal_assurance_history carrying the principal's strongest remaining
// factor.
func (s *PgStore) projectDecision(ctx context.Context, tx pgx.Tx,
	principalID, tenantID, actionType, outcome, basis, correlationID string, at time.Time) error {
	var eventType, level string
	switch actionType {
	case "AUTHENTICATE":
		eventType, level = "LOGIN", "PASSWORD"
		if outcome != "ALLOWED" {
			eventType = "FAILED_LOGIN"
		}
	case "MFA_STEP_UP":
		eventType, level = "STEP_UP", "MFA"
		if strings.HasPrefix(basis, "passkey") {
			level = "PASSKEY"
		}
	case "MFA_LOGIN":
		eventType, level = "MFA_CHALLENGE", "MFA"
		if strings.HasPrefix(basis, "passkey") {
			level = "PASSKEY"
		}
	case "MFA_ACTIVATE", "MFA_REVOKE", "MFA_RESET":
		if outcome != "ALLOWED" {
			return nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO principal_assurance_history
			    (history_id, principal_id, tenant_id, assurance_level, changed_at, changed_by, reason)
			SELECT $1::text, $2::text, $3::text,
			       CASE WHEN EXISTS (SELECT 1 FROM webauthn_credentials w
			                          WHERE w.principal_id = $2::text AND w.tenant_id = $3::text AND w.status = 'ACTIVE') THEN 'PASSKEY'
			            WHEN EXISTS (SELECT 1 FROM principal_credentials c
			                          WHERE c.principal_id = $2::text AND c.tenant_id = $3::text
			                            AND c.credential_type = 'TOTP' AND c.status = 'ACTIVE') THEN 'TOTP'
			            ELSE 'PASSWORD' END,
			       $4::timestamptz, $5::text, $6::text`,
			ulid.Make().String(), principalID, tenantID, at, correlationID, truncateText(actionType+":"+basis, 255))
		if err != nil {
			return fmt.Errorf("append assurance history: %w", err)
		}
		return nil
	default:
		return nil
	}
	result := "SUCCESS"
	if outcome != "ALLOWED" {
		result = "FAILURE"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO authentication_events
		    (event_id, tenant_id, principal_id, event_type, outcome, assurance_level, occurred_at, correlation_id, details)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), jsonb_build_object('basis', $9::text, 'decision', $10::text))`,
		ulid.Make().String(), tenantID, principalID, eventType, result, level, at, correlationID, basis, outcome); err != nil {
		return fmt.Errorf("append authentication event: %w", err)
	}
	return nil
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
