package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/oklog/ulid/v2"

	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/federation"
)

// INT-10 federation persistence. Every state change is written with its append-only evidence row and its
// outbox event in ONE transaction, scoped to the tenant by row level security.

var _ federation.Repo = (*PgStore)(nil)

const versionColumns = `connection_id, tenant_id, version, status, issuer, audience, jwks_uri, allowed_algs,
	subject_claim, email_claim, mfa_amr_values, mfa_acr_values, nonce_required, max_token_age_seconds,
	clock_skew_seconds, outage_behavior, key_grace_seconds, jit_enabled, fingerprint, created_by, created_at,
	decided_by, decided_at, decision_reason`

const connectionColumns = `connection_id, tenant_id, name, protocol, status, status_reason, active_version,
	latest_version, created_by, created_at, updated_at`

func scanVersion(row pgx.Row) (*federation.Version, error) {
	var v federation.Version
	var decidedBy *string
	var nonceRequired bool
	err := row.Scan(&v.ConnectionID, &v.TenantID, &v.Version, &v.Status, &v.Config.Issuer, &v.Config.Audience,
		&v.Config.JWKSURI, &v.Config.AllowedAlgs, &v.Config.SubjectClaim, &v.Config.EmailClaim,
		&v.Config.MFAAMRValues, &v.Config.MFAACRValues, &nonceRequired, &v.Config.MaxTokenAgeSeconds,
		&v.Config.ClockSkewSeconds, &v.Config.OutageBehavior, &v.Config.KeyGraceSeconds, &v.Config.JITEnabled,
		&v.Fingerprint, &v.CreatedBy, &v.CreatedAt, &decidedBy, &v.DecidedAt, &v.DecisionReason)
	if err != nil {
		return nil, err
	}
	v.Config.NonceOptional = !nonceRequired
	if decidedBy != nil {
		v.DecidedBy = *decidedBy
	}
	return &v, nil
}

func scanConnection(row pgx.Row) (*federation.Connection, error) {
	var c federation.Connection
	err := row.Scan(&c.ConnectionID, &c.TenantID, &c.Name, &c.Protocol, &c.Status, &c.StatusReason,
		&c.ActiveVersion, &c.LatestVersion, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func insertVersion(ctx context.Context, tx pgx.Tx, v federation.Version) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO federation_connection_versions (connection_id, tenant_id, version, status, issuer, audience,
			jwks_uri, allowed_algs, subject_claim, email_claim, mfa_amr_values, mfa_acr_values, nonce_required,
			max_token_age_seconds, clock_skew_seconds, outage_behavior, key_grace_seconds, jit_enabled,
			fingerprint, created_by)
		VALUES ($1,$2,$3,'PENDING_APPROVAL',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		v.ConnectionID, v.TenantID, v.Version, v.Config.Issuer, v.Config.Audience, v.Config.JWKSURI,
		v.Config.AllowedAlgs, v.Config.SubjectClaim, v.Config.EmailClaim, v.Config.MFAAMRValues,
		v.Config.MFAACRValues, !v.Config.NonceOptional, v.Config.MaxTokenAgeSeconds, v.Config.ClockSkewSeconds,
		v.Config.OutageBehavior, v.Config.KeyGraceSeconds, v.Config.JITEnabled, v.Fingerprint, v.CreatedBy)
	return err
}

// fedEvent renders and enqueues one event inside tx.
func (s *PgStore) fedEvent(ctx context.Context, tx pgx.Tx, eventType, tenantID, actor, correlationID, key string, payload map[string]any) error {
	rec, err := events.Render(eventType, tenantID, "", actor, correlationID, key, payload)
	if err != nil {
		return err
	}
	return s.outboxEnqueueTx(ctx, tx, rec)
}

// fedTrail appends a connection lifecycle row.
func fedTrail(ctx context.Context, tx pgx.Tx, tenantID, connectionID, eventType string, version *int, actor, correlationID string, detail string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO federation_connection_events (event_id, tenant_id, connection_id, event_type, version, actor_id, correlation_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7, jsonb_build_object('note', $8::text))`,
		ulid.Make().String(), tenantID, connectionID, eventType, version, actor, correlationID, detail)
	return err
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateConnection inserts a connection and its first version, pending approval.
func (s *PgStore) CreateConnection(ctx context.Context, c federation.Connection, v federation.Version, actor, correlationID string) error {
	err := s.withRLS(ctx, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO federation_connections (connection_id, tenant_id, name, protocol, status, latest_version, created_by)
			VALUES ($1,$2,$3,'OIDC','PENDING_APPROVAL',1,$4)`,
			c.ConnectionID, c.TenantID, c.Name, c.CreatedBy); err != nil {
			return err
		}
		v.Version = 1
		if err := insertVersion(ctx, tx, v); err != nil {
			return err
		}
		one := 1
		if err := fedTrail(ctx, tx, c.TenantID, c.ConnectionID, "CONFIGURED", &one, actor, correlationID, v.Fingerprint); err != nil {
			return err
		}
		return s.fedEvent(ctx, tx, events.EventFederationVersionProposed, c.TenantID, actor, correlationID, c.ConnectionID,
			map[string]any{"connection_id": c.ConnectionID, "version": 1, "fingerprint": v.Fingerprint, "issuer": v.Config.Issuer})
	})
	if isUnique(err) {
		return federation.ErrConflict
	}
	return err
}

// AddVersion appends the next version; only one version may await a decision at a time.
func (s *PgStore) AddVersion(ctx context.Context, tenantID, connectionID string, v federation.Version, actor, correlationID string) (*federation.Version, error) {
	var out *federation.Version
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		var latest int
		if err := tx.QueryRow(ctx, `SELECT status, latest_version FROM federation_connections
			WHERE connection_id = $1 AND tenant_id = $2 FOR UPDATE`, connectionID, tenantID).Scan(&status, &latest); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		if status == federation.ConnRevoked {
			return fmt.Errorf("%w: the connection is revoked", federation.ErrTransition)
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM federation_connection_versions
			WHERE connection_id = $1 AND status = 'PENDING_APPROVAL'`, connectionID).Scan(&pending); err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("%w: a version is already awaiting approval", federation.ErrConflict)
		}
		v.ConnectionID, v.TenantID, v.Version = connectionID, tenantID, latest+1
		if err := insertVersion(ctx, tx, v); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_connections SET latest_version = $1, updated_at = NOW()
			WHERE connection_id = $2`, v.Version, connectionID); err != nil {
			return err
		}
		n := v.Version
		if err := fedTrail(ctx, tx, tenantID, connectionID, "VERSION_PROPOSED", &n, actor, correlationID, v.Fingerprint); err != nil {
			return err
		}
		if err := s.fedEvent(ctx, tx, events.EventFederationVersionProposed, tenantID, actor, correlationID, connectionID,
			map[string]any{"connection_id": connectionID, "version": n, "fingerprint": v.Fingerprint, "issuer": v.Config.Issuer}); err != nil {
			return err
		}
		stored, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionColumns+` FROM federation_connection_versions
			WHERE connection_id = $1 AND version = $2`, connectionID, n))
		out = stored
		return err
	})
	return out, err
}

// GetConnection returns a connection with its versions, newest first.
func (s *PgStore) GetConnection(ctx context.Context, tenantID, connectionID string) (*federation.ConnectionDetail, error) {
	var out *federation.ConnectionDetail
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		d, err := loadDetail(ctx, tx, tenantID, connectionID)
		out = d
		return err
	})
	return out, err
}

func loadDetail(ctx context.Context, tx pgx.Tx, tenantID, connectionID string) (*federation.ConnectionDetail, error) {
	c, err := scanConnection(tx.QueryRow(ctx, `SELECT `+connectionColumns+` FROM federation_connections
		WHERE connection_id = $1 AND tenant_id = $2`, connectionID, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, federation.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+versionColumns+` FROM federation_connection_versions
		WHERE connection_id = $1 ORDER BY version DESC`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d := &federation.ConnectionDetail{Connection: *c}
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		d.Versions = append(d.Versions, *v)
	}
	return d, rows.Err()
}

// ListConnections lists a tenant's connections.
func (s *PgStore) ListConnections(ctx context.Context, tenantID string) ([]federation.Connection, error) {
	var out []federation.Connection
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+connectionColumns+` FROM federation_connections
			WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanConnection(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	return out, err
}

// DecideVersion approves or rejects a pending version.
func (s *PgStore) DecideVersion(ctx context.Context, tenantID, connectionID string, version int, approve bool,
	actor, fingerprint, reason, correlationID string) (*federation.ConnectionDetail, error) {
	var out *federation.ConnectionDetail
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		var active *int
		if err := tx.QueryRow(ctx, `SELECT status, active_version FROM federation_connections
			WHERE connection_id = $1 AND tenant_id = $2 FOR UPDATE`, connectionID, tenantID).Scan(&status, &active); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		if status == federation.ConnRevoked {
			return fmt.Errorf("%w: the connection is revoked", federation.ErrTransition)
		}
		v, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionColumns+` FROM federation_connection_versions
			WHERE connection_id = $1 AND version = $2 FOR UPDATE`, connectionID, version))
		if errors.Is(err, pgx.ErrNoRows) {
			return federation.ErrNotFound
		}
		if err != nil {
			return err
		}
		if v.Status != federation.VerPending {
			return federation.ErrNotPending
		}
		if v.CreatedBy == actor {
			return federation.ErrSelfApproval
		}
		if !approve {
			if _, err := tx.Exec(ctx, `UPDATE federation_connection_versions SET status = 'REJECTED', decided_by = $1,
				decided_at = NOW(), decision_reason = $2 WHERE connection_id = $3 AND version = $4`,
				actor, reason, connectionID, version); err != nil {
				return err
			}
			if err := fedTrail(ctx, tx, tenantID, connectionID, "VERSION_REJECTED", &version, actor, correlationID, reason); err != nil {
				return err
			}
			if err := s.fedEvent(ctx, tx, events.EventFederationVersionRejected, tenantID, actor, correlationID, connectionID,
				map[string]any{"connection_id": connectionID, "version": version}); err != nil {
				return err
			}
			out, err = loadDetail(ctx, tx, tenantID, connectionID)
			return err
		}
		// the approver approves exactly the configuration they were shown
		if fingerprint == "" || fingerprint != v.Fingerprint {
			return federation.ErrFingerprint
		}
		// one live connection per issuer in a tenant: a token's issuer must select one configuration
		var clash int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM federation_connections c
			JOIN federation_connection_versions av ON av.connection_id = c.connection_id AND av.version = c.active_version
			WHERE c.tenant_id = $1 AND c.connection_id <> $2 AND c.status IN ('ACTIVE','DEGRADED','SUSPENDED')
			  AND av.issuer = $3`, tenantID, connectionID, v.Config.Issuer).Scan(&clash); err != nil {
			return err
		}
		if clash > 0 {
			return fmt.Errorf("%w: another connection of this tenant already trusts that issuer", federation.ErrConflict)
		}
		if active != nil {
			if _, err := tx.Exec(ctx, `UPDATE federation_connection_versions SET status = 'SUPERSEDED'
				WHERE connection_id = $1 AND version = $2`, connectionID, *active); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_connection_versions SET status = 'APPROVED', decided_by = $1,
			decided_at = NOW(), decision_reason = $2, approved_fingerprint = $3
			WHERE connection_id = $4 AND version = $5`, actor, reason, fingerprint, connectionID, version); err != nil {
			return err
		}
		next := status
		if status == federation.ConnPendingApproval {
			next = federation.ConnActive
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_connections SET active_version = $1, status = $2,
			status_reason = '', updated_at = NOW() WHERE connection_id = $3`, version, next, connectionID); err != nil {
			return err
		}
		if err := fedTrail(ctx, tx, tenantID, connectionID, "VERSION_APPROVED", &version, actor, correlationID, reason); err != nil {
			return err
		}
		eventType := events.EventFederationVersionApproved
		if status == federation.ConnPendingApproval {
			eventType = events.EventFederationActivated
		}
		if err := s.fedEvent(ctx, tx, eventType, tenantID, actor, correlationID, connectionID,
			map[string]any{"connection_id": connectionID, "version": version, "fingerprint": fingerprint, "issuer": v.Config.Issuer}); err != nil {
			return err
		}
		out, err = loadDetail(ctx, tx, tenantID, connectionID)
		return err
	})
	return out, err
}

// SetStatus performs the operator transitions: suspend, resume, revoke.
func (s *PgStore) SetStatus(ctx context.Context, tenantID, connectionID, to, actor, reason, correlationID string) (*federation.Connection, error) {
	var out *federation.Connection
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		var active *int
		if err := tx.QueryRow(ctx, `SELECT status, active_version FROM federation_connections
			WHERE connection_id = $1 AND tenant_id = $2 FOR UPDATE`, connectionID, tenantID).Scan(&status, &active); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		ok := false
		trail, evt := "", ""
		switch to {
		case federation.ConnSuspended:
			ok = status == federation.ConnActive || status == federation.ConnDegraded
			trail, evt = "SUSPENDED", events.EventFederationSuspended
		case federation.ConnActive: // resume
			ok = status == federation.ConnSuspended && active != nil
			trail, evt = "RESUMED", events.EventFederationResumed
		case federation.ConnRevoked:
			ok = status != federation.ConnRevoked
			trail, evt = "REVOKED", events.EventFederationRevoked
		}
		if !ok {
			return fmt.Errorf("%w: %s to %s", federation.ErrTransition, status, to)
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_connections SET status = $1, status_reason = $2, updated_at = NOW()
			WHERE connection_id = $3`, to, reason, connectionID); err != nil {
			return err
		}
		if err := fedTrail(ctx, tx, tenantID, connectionID, trail, nil, actor, correlationID, reason); err != nil {
			return err
		}
		if err := s.fedEvent(ctx, tx, evt, tenantID, actor, correlationID, connectionID,
			map[string]any{"connection_id": connectionID, "from": status, "to": to}); err != nil {
			return err
		}
		c, err := scanConnection(tx.QueryRow(ctx, `SELECT `+connectionColumns+` FROM federation_connections
			WHERE connection_id = $1`, connectionID))
		out = c
		return err
	})
	return out, err
}

// SetKeyHealth flips an approved connection between ACTIVE and DEGRADED when its signing keys become
// unobtainable or are obtained again. It touches nothing else: a SUSPENDED or REVOKED connection stays so.
func (s *PgStore) SetKeyHealth(ctx context.Context, tenantID, connectionID string, degraded bool, reason string) error {
	from, to, evt, trail := federation.ConnDegraded, federation.ConnActive, events.EventFederationRecovered, "RECOVERED"
	if degraded {
		from, to, evt, trail = federation.ConnActive, federation.ConnDegraded, events.EventFederationDegraded, "DEGRADED"
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE federation_connections SET status = $1, status_reason = $2, updated_at = NOW()
			WHERE connection_id = $3 AND tenant_id = $4 AND status = $5`, to, reason, connectionID, tenantID, from)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if err := fedTrail(ctx, tx, tenantID, connectionID, trail, nil, "system", "", reason); err != nil {
			return err
		}
		return s.fedEvent(ctx, tx, evt, tenantID, "system", "", connectionID,
			map[string]any{"connection_id": connectionID, "reason": reason})
	})
}

// FindLoginConnection returns the live connection of the tenant for a claimed issuer, with its approved version.
func (s *PgStore) FindLoginConnection(ctx context.Context, tenantID, issuer string) (*federation.Connection, *federation.Version, error) {
	var c *federation.Connection
	var v *federation.Version
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		conn, err := scanConnection(tx.QueryRow(ctx, `SELECT c.connection_id, c.tenant_id, c.name, c.protocol, c.status,
				c.status_reason, c.active_version, c.latest_version, c.created_by, c.created_at, c.updated_at
			FROM federation_connections c
			JOIN federation_connection_versions av ON av.connection_id = c.connection_id AND av.version = c.active_version
			WHERE c.tenant_id = $1 AND c.status IN ('ACTIVE','DEGRADED') AND av.status = 'APPROVED' AND av.issuer = $2`,
			tenantID, issuer))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ver, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionColumns+` FROM federation_connection_versions
			WHERE connection_id = $1 AND version = $2`, conn.ConnectionID, *conn.ActiveVersion))
		if err != nil {
			return err
		}
		c, v = conn, ver
		return nil
	})
	return c, v, err
}

// ── keys ─────────────────────────────────────────────────────────────────────

func (s *PgStore) LoadKeySet(ctx context.Context, tenantID, connectionID string) (*federation.KeySetRecord, error) {
	var rec *federation.KeySetRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var r federation.KeySetRecord
		err := tx.QueryRow(ctx, `SELECT connection_id, tenant_id, jwks_uri, jwks, key_count, fetched_at, last_attempt_at,
				last_error, consecutive_failures FROM federation_key_sets WHERE connection_id = $1 AND tenant_id = $2`,
			connectionID, tenantID).Scan(&r.ConnectionID, &r.TenantID, &r.JWKSURI, &r.JWKS, &r.KeyCount, &r.FetchedAt,
			&r.LastAttemptAt, &r.LastError, &r.ConsecutiveFailures)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rec = &r
		return nil
	})
	return rec, err
}

func (s *PgStore) SaveKeySet(ctx context.Context, rec federation.KeySetRecord) error {
	return s.withRLS(ctx, rec.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO federation_key_sets (connection_id, tenant_id, jwks_uri, jwks, key_count, fetched_at, last_attempt_at)
			VALUES ($1,$2,$3,$4,$5,$6,$6)
			ON CONFLICT (connection_id) DO UPDATE SET jwks_uri = EXCLUDED.jwks_uri, jwks = EXCLUDED.jwks,
				key_count = EXCLUDED.key_count, fetched_at = EXCLUDED.fetched_at, last_attempt_at = EXCLUDED.last_attempt_at,
				last_error = '', consecutive_failures = 0`,
			rec.ConnectionID, rec.TenantID, rec.JWKSURI, rec.JWKS, rec.KeyCount, rec.FetchedAt)
		return err
	})
}

func (s *PgStore) RecordKeyFetchFailure(ctx context.Context, tenantID, connectionID, jwksURI, message string, at time.Time) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE federation_key_sets SET last_attempt_at = $1, last_error = $2,
			consecutive_failures = consecutive_failures + 1 WHERE connection_id = $3 AND tenant_id = $4`,
			at, message, connectionID, tenantID)
		return err
	})
}

// ── links ────────────────────────────────────────────────────────────────────

const linkColumns = `link_id, tenant_id, connection_id, issuer, subject, principal_id, status, link_source,
	username, attributes, attributes_rev, deprovision_requested_at, deprovision_due_at, sessions_evicted_at,
	access_revoked_at, deprovision_escalated_at,
	linked_by, created_at, last_login_at, deprovisioned_at`

func scanLink(row pgx.Row) (*federation.Link, error) {
	var l federation.Link
	err := row.Scan(&l.LinkID, &l.TenantID, &l.ConnectionID, &l.Issuer, &l.Subject, &l.PrincipalID, &l.Status,
		&l.Source, &l.Username, &l.Attributes, &l.AttributesRev, &l.DeprovisionRequestedAt, &l.DeprovisionDueAt,
		&l.SessionsEvictedAt, &l.AccessRevokedAt, &l.DeprovisionEscalatedAt, &l.LinkedBy, &l.CreatedAt, &l.LastLoginAt, &l.DeprovisionedAt)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *PgStore) FindLink(ctx context.Context, tenantID, issuer, subject string) (*federation.Link, error) {
	var out *federation.Link
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		l, err := scanLink(tx.QueryRow(ctx, `SELECT `+linkColumns+` FROM federation_links
			WHERE tenant_id = $1 AND issuer = $2 AND subject = $3`, tenantID, issuer, subject))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		out = l
		return err
	})
	return out, err
}

func (s *PgStore) GetLink(ctx context.Context, tenantID, linkID string) (*federation.Link, error) {
	var out *federation.Link
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		l, err := scanLink(tx.QueryRow(ctx, `SELECT `+linkColumns+` FROM federation_links
			WHERE tenant_id = $1 AND link_id = $2`, tenantID, linkID))
		if errors.Is(err, pgx.ErrNoRows) {
			return federation.ErrLinkNotFound
		}
		out = l
		return err
	})
	return out, err
}

func (s *PgStore) ListLinks(ctx context.Context, tenantID, principalID, issuer string, limit int) ([]federation.Link, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var out []federation.Link
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+linkColumns+` FROM federation_links
			WHERE tenant_id = $1 AND ($2::text = '' OR principal_id = $2::text) AND ($3::text = '' OR issuer = $3::text)
			ORDER BY created_at DESC LIMIT $4`, tenantID, principalID, issuer, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			l, err := scanLink(rows)
			if err != nil {
				return err
			}
			out = append(out, *l)
		}
		return rows.Err()
	})
	return out, err
}

// CreateLink links an external subject to an existing principal (an administrator's deliberate act).
func (s *PgStore) CreateLink(ctx context.Context, l federation.Link, actor, correlationID string) (*federation.Link, error) {
	var out *federation.Link
	err := s.withRLS(ctx, l.TenantID, func(tx pgx.Tx) error {
		var iss string
		var cstatus string
		if err := tx.QueryRow(ctx, `
			SELECT av.issuer, c.status FROM federation_connections c
			JOIN federation_connection_versions av ON av.connection_id = c.connection_id AND av.version = c.active_version
			WHERE c.connection_id = $1 AND c.tenant_id = $2`, l.ConnectionID, l.TenantID).Scan(&iss, &cstatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		if cstatus == federation.ConnRevoked {
			return fmt.Errorf("%w: the connection is revoked", federation.ErrTransition)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE principal_id = $1 AND tenant_id = $2)`,
			l.PrincipalID, l.TenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return federation.ErrUnknownPrincipal
		}
		l.Issuer, l.LinkID, l.Status = iss, ulid.Make().String(), federation.LinkActive
		created, err := insertLink(ctx, tx, l)
		if err != nil {
			return err
		}
		if err := s.fedEvent(ctx, tx, events.EventIdentityLinkCreated, l.TenantID, actor, correlationID, l.PrincipalID,
			map[string]any{"link_id": created.LinkID, "connection_id": l.ConnectionID, "principal_id": l.PrincipalID,
				"issuer": iss, "link_source": l.Source}); err != nil {
			return err
		}
		out = created
		return nil
	})
	if isUnique(err) {
		return nil, federation.ErrLinkConflict
	}
	return out, err
}

func insertLink(ctx context.Context, tx pgx.Tx, l federation.Link) (*federation.Link, error) {
	return scanLink(tx.QueryRow(ctx, `
		INSERT INTO federation_links (link_id, tenant_id, connection_id, issuer, subject, principal_id, status, link_source, linked_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+linkColumns,
		l.LinkID, l.TenantID, l.ConnectionID, l.Issuer, l.Subject, l.PrincipalID, l.Status, l.Source, l.LinkedBy))
}

// ── login evidence ───────────────────────────────────────────────────────────

func insertLogin(ctx context.Context, tx pgx.Tx, rec federation.LoginRecord) error {
	if rec.LogID == "" {
		rec.LogID = ulid.Make().String()
	}
	var principal *string
	if rec.PrincipalID != "" {
		principal = &rec.PrincipalID
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO federation_login_log (log_id, tenant_id, connection_id, config_version, issuer, subject, principal_id,
			outcome, reason, key_id, mfa_asserted, degraded_keys, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		rec.LogID, rec.TenantID, rec.ConnectionID, rec.ConfigVersion, rec.Issuer, rec.Subject, principal,
		rec.Outcome, rec.Reason, rec.KeyID, rec.MFAAsserted, rec.DegradedKeys, rec.CorrelationID)
	return err
}

// CompleteLogin consumes the token, records the evidence, advances the link and enqueues the event.
func (s *PgStore) CompleteLogin(ctx context.Context, rec federation.LoginRecord, linkID, tokenHash string, tokenExpiry time.Time) error {
	return s.withRLS(ctx, rec.TenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO federation_token_uses (connection_id, tenant_id, token_hash, expires_at)
			VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, rec.ConnectionID, rec.TenantID, tokenHash, tokenExpiry)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return federation.ErrTokenReplayed
		}
		if err := insertLogin(ctx, tx, rec); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_links SET last_login_at = NOW()
			WHERE link_id = $1 AND tenant_id = $2`, linkID, rec.TenantID); err != nil {
			return err
		}
		return s.fedEvent(ctx, tx, events.EventFederationLoginSucceeded, rec.TenantID, rec.PrincipalID, rec.CorrelationID, rec.PrincipalID,
			map[string]any{"connection_id": rec.ConnectionID, "config_version": rec.ConfigVersion, "principal_id": rec.PrincipalID,
				"issuer": rec.Issuer, "mfa_asserted": rec.MFAAsserted, "degraded_keys": rec.DegradedKeys})
	})
}

// RecordDenied writes the evidence row for a verified-but-refused login.
func (s *PgStore) RecordDenied(ctx context.Context, rec federation.LoginRecord) error {
	rec.Outcome = federation.OutcomeDenied
	return s.withRLS(ctx, rec.TenantID, func(tx pgx.Tx) error {
		if err := insertLogin(ctx, tx, rec); err != nil {
			return err
		}
		return s.fedEvent(ctx, tx, events.EventFederationLoginDenied, rec.TenantID, rec.PrincipalID, rec.CorrelationID, rec.Subject,
			map[string]any{"connection_id": rec.ConnectionID, "config_version": rec.ConfigVersion, "issuer": rec.Issuer, "reason": rec.Reason})
	})
}

func (s *PgStore) ListLogins(ctx context.Context, tenantID, connectionID string, limit int) ([]federation.LoginRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []federation.LoginRecord
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT log_id, tenant_id, connection_id, config_version, issuer, subject,
				COALESCE(principal_id,''), outcome, reason, key_id, mfa_asserted, degraded_keys, correlation_id, created_at
			FROM federation_login_log WHERE tenant_id = $1 AND connection_id = $2 ORDER BY created_at DESC LIMIT $3`,
			tenantID, connectionID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r federation.LoginRecord
			if err := rows.Scan(&r.LogID, &r.TenantID, &r.ConnectionID, &r.ConfigVersion, &r.Issuer, &r.Subject,
				&r.PrincipalID, &r.Outcome, &r.Reason, &r.KeyID, &r.MFAAsserted, &r.DegradedKeys, &r.CorrelationID, &r.CreatedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
