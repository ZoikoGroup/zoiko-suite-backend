package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ORG-01 reads: the subject identity, its assurance history and the authentication events. They read what the
// auth plane already records; nothing here writes.

// AssuranceObservation is one recorded assurance level of a person: how strongly they were shown to be who they
// say, when, by which methods, and what it was before. Never a code, token or secret.
type AssuranceObservation struct {
	ObservationID  string    `json:"observation_id"`
	PrincipalID    string    `json:"principal_id"`
	SessionID      string    `json:"session_id,omitempty"`
	ObservedAt     time.Time `json:"observed_at"`
	AssuranceLevel string    `json:"assurance_level"`
	PreviousLevel  string    `json:"previous_level,omitempty"`
	ChangeKind     string    `json:"change_kind"`
	AuthMethods    []string  `json:"auth_methods"`
	IdPID          string    `json:"idp_id,omitempty"`
	CorrelationID  string    `json:"correlation_id,omitempty"`
}

// SubjectIdentity is a person (or workload) with what the platform knows about their sign-in.
type SubjectIdentity struct {
	PrincipalDetail
	LinkCount        int                   `json:"link_count"`
	ActiveLinkCount  int                   `json:"active_link_count"`
	LiveSessions     int                   `json:"live_sessions"`
	CurrentAssurance *AssuranceObservation `json:"current_assurance,omitempty"`
}

// GetSubjectIdentity reads one subject with its link and session counts and its latest assurance; nil when the
// subject is not in the tenant.
func (s *PgStore) GetSubjectIdentity(ctx context.Context, tenantID, principalID string) (*SubjectIdentity, error) {
	var out *SubjectIdentity
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		p, err := scanPrincipalDetail(tx.QueryRow(ctx, `SELECT `+principalDetailCols+` FROM principals
			WHERE principal_id = $1 AND tenant_id = $2`, principalID, tenantID))
		if err != nil || p == nil {
			return err
		}
		out = &SubjectIdentity{PrincipalDetail: *p}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE status = 'ACTIVE')
			FROM federation_links WHERE tenant_id = $1 AND principal_id = $2`, tenantID, principalID).
			Scan(&out.LinkCount, &out.ActiveLinkCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM auth_sessions
			WHERE tenant_id = $1 AND principal_id = $2 AND ended_at IS NULL AND expires_at > NOW()`,
			tenantID, principalID).Scan(&out.LiveSessions); err != nil {
			return err
		}
		cur, err := scanObservation(tx.QueryRow(ctx, `SELECT `+observationCols+` FROM assurance_observations
			WHERE tenant_id = $1 AND principal_id = $2
			ORDER BY observed_at DESC, observation_id DESC LIMIT 1`, tenantID, principalID))
		if err != nil {
			return err
		}
		out.CurrentAssurance = cur
		return nil
	})
	return out, err
}

const observationCols = `observation_id, principal_id, COALESCE(session_id,''), observed_at, assurance_level,
	COALESCE(previous_level,''), change_kind, auth_methods, COALESCE(idp_id,''), correlation_id`

func scanObservation(row pgx.Row) (*AssuranceObservation, error) {
	var o AssuranceObservation
	err := row.Scan(&o.ObservationID, &o.PrincipalID, &o.SessionID, &o.ObservedAt, &o.AssuranceLevel,
		&o.PreviousLevel, &o.ChangeKind, &o.AuthMethods, &o.IdPID, &o.CorrelationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// Cursor is the position of the last row a page returned. Pages are keyset-ordered, newest first, so a row
// written between two requests cannot move one already seen.
type Cursor struct {
	At time.Time
	ID string
}

// Encode renders the cursor as the opaque token a client sends back.
func (c Cursor) Encode() string { return c.At.UTC().Format(time.RFC3339Nano) + "|" + c.ID }

// ParseCursor reads a token Encode produced; the zero Cursor and false for an empty or malformed one.
func ParseCursor(raw string) (Cursor, bool) {
	at, id, ok := strings.Cut(raw, "|")
	if !ok || id == "" {
		return Cursor{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return Cursor{}, false
	}
	return Cursor{At: t.UTC(), ID: id}, true
}

func clampLimit(n int) int {
	if n <= 0 || n > 200 {
		return 50
	}
	return n
}

// ListAssuranceObservations pages a subject's assurance history, newest first. The next cursor is empty on the
// last page.
func (s *PgStore) ListAssuranceObservations(ctx context.Context, tenantID, principalID string, after *Cursor, limit int) ([]AssuranceObservation, string, error) {
	limit = clampLimit(limit)
	var out []AssuranceObservation
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var at *time.Time
		var id string
		if after != nil {
			at, id = &after.At, after.ID
		}
		rows, err := tx.Query(ctx, `SELECT `+observationCols+` FROM assurance_observations
			WHERE tenant_id = $1 AND principal_id = $2
			  AND ($3::timestamptz IS NULL OR (observed_at, observation_id) < ($3, $4))
			ORDER BY observed_at DESC, observation_id DESC LIMIT $5`, tenantID, principalID, at, id, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanObservation(rows)
			if err != nil {
				return err
			}
			out = append(out, *o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = Cursor{At: out[limit-1].ObservedAt, ID: out[limit-1].ObservationID}.Encode()
	}
	return out, next, nil
}

// AuthenticationEvent is one sign-in or second-factor event of a subject, from whichever record holds it.
type AuthenticationEvent struct {
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	Outcome       string    `json:"outcome"`
	Reason        string    `json:"reason,omitempty"`
	Source        string    `json:"source"`
	OccurredAt    time.Time `json:"occurred_at"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	Issuer        string    `json:"issuer,omitempty"`
	ConnectionID  string    `json:"connection_id,omitempty"`
	MFAAsserted   *bool     `json:"mfa_asserted,omitempty"`
}

// authenticationActions are the decision-log actions that are a sign-in or a second-factor event. Status
// changes and creation are in the log too, and are not authentication.
var authenticationActions = []string{
	"AUTHENTICATE", "MFA_LOGIN", "MFA_STEP_UP", "MFA_ENROL", "MFA_ACTIVATE", "MFA_REVOKE", "MFA_RESET",
}

// AuthenticationEventFilter narrows the read model. Zero values select everything.
type AuthenticationEventFilter struct {
	EventType string
	Outcome   string
	Since     *time.Time
	Until     *time.Time
}

// ListAuthenticationEvents reads a subject's sign-in and second-factor events, newest first, as one read model
// over the decision log (password and second-factor outcomes), the federated login log and the ending of login
// sessions. A session that ended is an event because revocation is something a security review asks about.
func (s *PgStore) ListAuthenticationEvents(ctx context.Context, tenantID, principalID string, f AuthenticationEventFilter, after *Cursor, limit int) ([]AuthenticationEvent, string, error) {
	limit = clampLimit(limit)
	var out []AuthenticationEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var at *time.Time
		var id string
		if after != nil {
			at, id = &after.At, after.ID
		}
		rows, err := tx.Query(ctx, `
			SELECT event_id, event_type, outcome, reason, source, occurred_at, correlation_id, session_id, issuer,
			       connection_id, mfa_asserted
			  FROM (
			    SELECT decision_log_id AS event_id, action_type AS event_type, decision_outcome AS outcome,
			           decision_basis AS reason, 'DECISION_LOG' AS source, decided_at AS occurred_at,
			           correlation_id, ''::text AS session_id, ''::text AS issuer, ''::text AS connection_id,
			           NULL::boolean AS mfa_asserted
			      FROM access_decision_log
			     WHERE tenant_id = $1 AND principal_id = $2 AND action_type = ANY($3)
			    UNION ALL
			    SELECT log_id, 'FEDERATED_LOGIN', outcome, reason, 'FEDERATION_LOGIN_LOG', created_at,
			           correlation_id, ''::text, issuer, connection_id, mfa_asserted
			      FROM federation_login_log
			     WHERE tenant_id = $1 AND principal_id = $2
			    UNION ALL
			    SELECT 'end-' || sid, 'SESSION_ENDED', 'ENDED', end_reason, 'AUTH_SESSION', ended_at,
			           ''::text, sid, ''::text, ''::text, NULL::boolean
			      FROM auth_sessions
			     WHERE tenant_id = $1 AND principal_id = $2 AND ended_at IS NOT NULL
			  ) e
			 WHERE ($4::text = '' OR event_type = $4)
			   AND ($5::text = '' OR outcome = $5)
			   AND ($6::timestamptz IS NULL OR occurred_at >= $6)
			   AND ($7::timestamptz IS NULL OR occurred_at < $7)
			   AND ($8::timestamptz IS NULL OR (occurred_at, event_id) < ($8, $9))
			 ORDER BY occurred_at DESC, event_id DESC
			 LIMIT $10`,
			tenantID, principalID, authenticationActions, f.EventType, f.Outcome, f.Since, f.Until, at, id, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e AuthenticationEvent
			if err := rows.Scan(&e.EventID, &e.EventType, &e.Outcome, &e.Reason, &e.Source, &e.OccurredAt,
				&e.CorrelationID, &e.SessionID, &e.Issuer, &e.ConnectionID, &e.MFAAsserted); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", fmt.Errorf("list authentication events: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = Cursor{At: out[limit-1].OccurredAt, ID: out[limit-1].EventID}.Encode()
	}
	return out, next, nil
}
