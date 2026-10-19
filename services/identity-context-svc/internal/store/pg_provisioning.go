package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oklog/ulid/v2"

	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/federation"
)

// INT-10 provisioning persistence. Each method that changes state writes the change, its evidence and its
// outbox event in one transaction, scoped to the tenant by row level security. The only cross-tenant reads
// are the worker's named scans (ClaimDueAssignments, LinksAwaitingEviction, LateDeprovisions).

var _ federation.ProvisionRepo = (*PgStore)(nil)

// withWorkerScope runs fn in a transaction that names the federation worker scope: it lets the one scan inside
// read across tenants and nothing else.
func (s *PgStore) withWorkerScope(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.federation_worker', 'on', true)`); err != nil {
		return fmt.Errorf("set_config app.federation_worker: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── SCIM tokens ──────────────────────────────────────────────────────────────

func (s *PgStore) CreateSCIMToken(ctx context.Context, tenantID, connectionID, tokenHash, actor, correlationID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM federation_connections WHERE connection_id = $1 AND tenant_id = $2 FOR UPDATE`,
			connectionID, tenantID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		if status == federation.ConnRevoked || status == federation.ConnPendingApproval {
			return fmt.Errorf("%w: the connection is %s", federation.ErrTransition, status)
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_scim_tokens SET status = 'REVOKED', revoked_at = NOW()
			WHERE connection_id = $1 AND status = 'ACTIVE'`, connectionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_scim_tokens (token_hash, connection_id, tenant_id, created_by)
			VALUES ($1,$2,$3,$4)`, tokenHash, connectionID, tenantID, actor); err != nil {
			return err
		}
		if err := fedTrail(ctx, tx, tenantID, connectionID, "SCIM_TOKEN_ISSUED", nil, actor, correlationID, ""); err != nil {
			return err
		}
		return s.fedEvent(ctx, tx, events.EventFederationSCIMTokenIssued, tenantID, actor, correlationID, connectionID,
			map[string]any{"connection_id": connectionID})
	})
}

func (s *PgStore) RevokeSCIMTokens(ctx context.Context, tenantID, connectionID, actor, correlationID string) (int, error) {
	n := 0
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM federation_connections WHERE connection_id = $1 AND tenant_id = $2)`,
			connectionID, tenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return federation.ErrNotFound
		}
		tag, err := tx.Exec(ctx, `UPDATE federation_scim_tokens SET status = 'REVOKED', revoked_at = NOW()
			WHERE connection_id = $1 AND status = 'ACTIVE'`, connectionID)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		if n == 0 {
			return nil
		}
		if err := fedTrail(ctx, tx, tenantID, connectionID, "SCIM_TOKEN_REVOKED", nil, actor, correlationID, ""); err != nil {
			return err
		}
		return s.fedEvent(ctx, tx, events.EventFederationSCIMTokenRevoked, tenantID, actor, correlationID, connectionID,
			map[string]any{"connection_id": connectionID})
	})
	return n, err
}

func (s *PgStore) ValidSCIMToken(ctx context.Context, tenantID, connectionID, tokenHash string) (bool, error) {
	ok := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE federation_scim_tokens SET last_used_at = NOW()
			WHERE token_hash = $1 AND connection_id = $2 AND tenant_id = $3 AND status = 'ACTIVE'`, tokenHash, connectionID, tenantID)
		ok = err == nil && tag.RowsAffected() == 1
		return err
	})
	return ok, err
}

// ── mapping versions ─────────────────────────────────────────────────────────

const mappingColumns = `connection_id, tenant_id, version, status, config, fingerprint, created_by, created_at, decided_by, decided_at, decision_reason`

func scanMapping(row pgx.Row) (*federation.MappingVersion, error) {
	var m federation.MappingVersion
	var cfg []byte
	var decidedBy *string
	if err := row.Scan(&m.ConnectionID, &m.TenantID, &m.Version, &m.Status, &cfg, &m.Fingerprint, &m.CreatedBy, &m.CreatedAt,
		&decidedBy, &m.DecidedAt, &m.DecisionReason); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(cfg, &m.Config); err != nil {
		return nil, fmt.Errorf("stored mapping is unreadable: %w", err)
	}
	if decidedBy != nil {
		m.DecidedBy = *decidedBy
	}
	return &m, nil
}

func (s *PgStore) CreateMappingVersion(ctx context.Context, tenantID, connectionID string, cfg federation.MappingConfig, actor, correlationID string) (*federation.MappingVersion, error) {
	var out *federation.MappingVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM federation_connections WHERE connection_id = $1 AND tenant_id = $2 FOR UPDATE`,
			connectionID, tenantID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		if status == federation.ConnRevoked {
			return fmt.Errorf("%w: the connection is revoked", federation.ErrTransition)
		}
		var pending, latest int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'PENDING_APPROVAL'), COALESCE(max(version), 0)
			FROM federation_mapping_versions WHERE connection_id = $1`, connectionID).Scan(&pending, &latest); err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("%w: a mapping version is already awaiting approval", federation.ErrConflict)
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		v := latest + 1
		if _, err := tx.Exec(ctx, `INSERT INTO federation_mapping_versions (connection_id, tenant_id, version, config, fingerprint, created_by)
			VALUES ($1,$2,$3,$4,$5,$6)`, connectionID, tenantID, v, raw, cfg.Fingerprint(), actor); err != nil {
			return err
		}
		if err := fedTrail(ctx, tx, tenantID, connectionID, "MAPPING_PROPOSED", &v, actor, correlationID, cfg.Fingerprint()); err != nil {
			return err
		}
		if err := s.fedEvent(ctx, tx, events.EventFederationMappingProposed, tenantID, actor, correlationID, connectionID,
			map[string]any{"connection_id": connectionID, "version": v, "fingerprint": cfg.Fingerprint(), "entries": len(cfg.Entries)}); err != nil {
			return err
		}
		m, err := scanMapping(tx.QueryRow(ctx, `SELECT `+mappingColumns+` FROM federation_mapping_versions WHERE connection_id = $1 AND version = $2`, connectionID, v))
		out = m
		return err
	})
	return out, err
}

func (s *PgStore) DecideMappingVersion(ctx context.Context, tenantID, connectionID string, version int, approve bool,
	actor, fingerprint, reason, correlationID string) (*federation.MappingVersion, error) {
	var out *federation.MappingVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var status string
		var active *int
		if err := tx.QueryRow(ctx, `SELECT status, active_mapping_version FROM federation_connections
			WHERE connection_id = $1 AND tenant_id = $2 FOR UPDATE`, connectionID, tenantID).Scan(&status, &active); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		if status == federation.ConnRevoked {
			return fmt.Errorf("%w: the connection is revoked", federation.ErrTransition)
		}
		m, err := scanMapping(tx.QueryRow(ctx, `SELECT `+mappingColumns+` FROM federation_mapping_versions
			WHERE connection_id = $1 AND version = $2 FOR UPDATE`, connectionID, version))
		if errors.Is(err, pgx.ErrNoRows) {
			return federation.ErrNotFound
		}
		if err != nil {
			return err
		}
		if m.Status != federation.MapPending {
			return federation.ErrMappingState
		}
		if m.CreatedBy == actor {
			return federation.ErrSelfApproval
		}
		if !approve {
			if _, err := tx.Exec(ctx, `UPDATE federation_mapping_versions SET status = 'REJECTED', decided_by = $1, decided_at = NOW(),
				decision_reason = $2 WHERE connection_id = $3 AND version = $4`, actor, reason, connectionID, version); err != nil {
				return err
			}
			if err := fedTrail(ctx, tx, tenantID, connectionID, "MAPPING_REJECTED", &version, actor, correlationID, reason); err != nil {
				return err
			}
			if err := s.fedEvent(ctx, tx, events.EventFederationMappingRejected, tenantID, actor, correlationID, connectionID,
				map[string]any{"connection_id": connectionID, "version": version}); err != nil {
				return err
			}
		} else {
			if fingerprint == "" || fingerprint != m.Fingerprint {
				return federation.ErrFingerprint
			}
			if active != nil {
				if _, err := tx.Exec(ctx, `UPDATE federation_mapping_versions SET status = 'SUPERSEDED'
					WHERE connection_id = $1 AND version = $2`, connectionID, *active); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE federation_mapping_versions SET status = 'APPROVED', decided_by = $1, decided_at = NOW(),
				decision_reason = $2, approved_fingerprint = $3 WHERE connection_id = $4 AND version = $5`,
				actor, reason, fingerprint, connectionID, version); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE federation_connections SET active_mapping_version = $1, updated_at = NOW()
				WHERE connection_id = $2`, version, connectionID); err != nil {
				return err
			}
			if err := fedTrail(ctx, tx, tenantID, connectionID, "MAPPING_APPROVED", &version, actor, correlationID, reason); err != nil {
				return err
			}
			if err := s.fedEvent(ctx, tx, events.EventFederationMappingApproved, tenantID, actor, correlationID, connectionID,
				map[string]any{"connection_id": connectionID, "version": version, "fingerprint": fingerprint}); err != nil {
				return err
			}
		}
		decided, err := scanMapping(tx.QueryRow(ctx, `SELECT `+mappingColumns+` FROM federation_mapping_versions WHERE connection_id = $1 AND version = $2`, connectionID, version))
		out = decided
		return err
	})
	return out, err
}

func (s *PgStore) GetMappings(ctx context.Context, tenantID, connectionID string) (*federation.MappingVersion, []federation.MappingVersion, error) {
	var active *federation.MappingVersion
	var all []federation.MappingVersion
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var av *int
		if err := tx.QueryRow(ctx, `SELECT active_mapping_version FROM federation_connections WHERE connection_id = $1 AND tenant_id = $2`,
			connectionID, tenantID).Scan(&av); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+mappingColumns+` FROM federation_mapping_versions WHERE connection_id = $1 ORDER BY version DESC`, connectionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMapping(rows)
			if err != nil {
				return err
			}
			all = append(all, *m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if av != nil {
			for i := range all {
				if all[i].Version == *av {
					c := all[i]
					active = &c
				}
			}
		}
		return nil
	})
	return active, all, err
}

// ── the event ledger ─────────────────────────────────────────────────────────

const eventColumns = `event_id, tenant_id, connection_id, dedupe_key, source, operation, subject, payload_hash, status, reason, result, received_at, decided_at`

func scanEvent(row pgx.Row) (*federation.ProvEvent, error) {
	var e federation.ProvEvent
	var res []byte
	if err := row.Scan(&e.EventID, &e.TenantID, &e.ConnectionID, &e.DedupeKey, &e.Source, &e.Operation, &e.Subject, &e.PayloadHash,
		&e.Status, &e.Reason, &res, &e.ReceivedAt, &e.DecidedAt); err != nil {
		return nil, err
	}
	if len(res) > 0 {
		_ = json.Unmarshal(res, &e.Result)
	}
	return &e, nil
}

func (s *PgStore) BeginEvent(ctx context.Context, ev federation.ProvEvent) (*federation.ProvEvent, bool, error) {
	var out *federation.ProvEvent
	created := false
	err := s.withRLS(ctx, ev.TenantID, func(tx pgx.Tx) error {
		if ev.EventID == "" {
			ev.EventID = ulid.Make().String()
		}
		tag, err := tx.Exec(ctx, `INSERT INTO federation_provisioning_events (event_id, tenant_id, connection_id, dedupe_key, source, operation, subject, payload_hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (connection_id, dedupe_key) DO NOTHING`,
			ev.EventID, ev.TenantID, ev.ConnectionID, ev.DedupeKey, ev.Source, ev.Operation, ev.Subject, ev.PayloadHash)
		if err != nil {
			return err
		}
		created = tag.RowsAffected() == 1
		e, err := scanEvent(tx.QueryRow(ctx, `SELECT `+eventColumns+` FROM federation_provisioning_events
			WHERE connection_id = $1 AND dedupe_key = $2`, ev.ConnectionID, ev.DedupeKey))
		out = e
		return err
	})
	return out, created, err
}

func (s *PgStore) DecideEvent(ctx context.Context, tenantID, eventID, status, reason, exceptionKind string, result map[string]any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var connID, subject string
		err := tx.QueryRow(ctx, `UPDATE federation_provisioning_events SET status = $1, reason = $2, result = $3, decided_at = NOW()
			WHERE event_id = $4 AND tenant_id = $5 AND status IN ('RECEIVED','VALIDATED')
			RETURNING connection_id, subject`, status, reason, raw, eventID, tenantID).Scan(&connID, &subject)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already decided: a decision is made once
		}
		if err != nil {
			return err
		}
		if status != federation.ProvRejected {
			return nil
		}
		if exceptionKind != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO federation_sync_exceptions (exception_id, tenant_id, connection_id, event_id, kind, detail)
				VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, ulid.Make().String(), tenantID, connID, eventID, exceptionKind, trimTo(reason, 500)); err != nil {
				return err
			}
		}
		return s.fedEvent(ctx, tx, events.EventProvisioningRejected, tenantID, "scim", "", connID,
			map[string]any{"connection_id": connID, "event_id": eventID, "reason": reason, "subject": subject})
	})
}

func trimTo(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *PgStore) ListEvents(ctx context.Context, tenantID, connectionID, status string, limit int) ([]federation.ProvEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []federation.ProvEvent
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+eventColumns+` FROM federation_provisioning_events
			WHERE tenant_id = $1 AND connection_id = $2 AND ($3::text = '' OR status = $3::text)
			ORDER BY received_at DESC LIMIT $4`, tenantID, connectionID, status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEvent(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

// ── applying a decision to a person ──────────────────────────────────────────

func (s *PgStore) ApplyIdentity(ctx context.Context, in federation.ApplyInput) (*federation.ApplyResult, error) {
	var out *federation.ApplyResult
	err := s.withRLS(ctx, in.TenantID, func(tx pgx.Tx) error {
		r, err := s.applyIdentityTx(ctx, tx, in)
		out = r
		return err
	})
	return out, err
}

func (s *PgStore) applyIdentityTx(ctx context.Context, tx pgx.Tx, in federation.ApplyInput) (*federation.ApplyResult, error) {
	res := &federation.ApplyResult{}
	link, err := scanLink(tx.QueryRow(ctx, `SELECT `+linkColumns+` FROM federation_links
		WHERE tenant_id = $1 AND connection_id = $2 AND subject = $3 FOR UPDATE`, in.TenantID, in.ConnectionID, in.Subject))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		link = nil
	}
	deprovision := in.Operation == federation.OpDeprovision || !in.Active

	if link == nil {
		if deprovision {
			return nil, federation.ErrUserNotFound
		}
		return s.createIdentityTx(ctx, tx, in, res)
	}

	if in.ExpectedRev != nil && *in.ExpectedRev != link.AttributesRev {
		return nil, federation.ErrStaleLink
	}
	var p struct{ status, email, display string }
	if err := tx.QueryRow(ctx, `SELECT status, email, display_name FROM principals WHERE principal_id = $1 AND tenant_id = $2 FOR UPDATE`,
		link.PrincipalID, in.TenantID).Scan(&p.status, &p.email, &p.display); err != nil {
		return nil, err
	}
	res.Link, res.Email, res.DisplayName = *link, p.email, p.display

	if deprovision {
		return s.deprovisionTx(ctx, tx, in, link, p.status, res)
	}

	changed := false
	if link.Status == federation.LinkDeprovisioned {
		var queued int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM federation_assignments WHERE link_id = $1 AND status IN ('PLANNED','REVOKE_PENDING')`,
			link.LinkID).Scan(&queued); err != nil {
			return nil, err
		}
		if link.SessionsEvictedAt == nil || queued > 0 {
			return nil, federation.ErrDeprovisionInProgress
		}
		if p.status == "DISABLED" {
			if _, err := tx.Exec(ctx, `UPDATE principals SET status = 'ACTIVE', updated_at = NOW() WHERE principal_id = $1`, link.PrincipalID); err != nil {
				return nil, err
			}
			if err := s.decisionRow(ctx, tx, link.PrincipalID, in.TenantID, "FEDERATION_REACTIVATE", in.EventID, in.CorrelationID); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_links SET status = 'ACTIVE', deprovisioned_at = NULL, deprovision_requested_at = NULL,
			deprovision_due_at = NULL, sessions_evicted_at = NULL, access_revoked_at = NULL, deprovision_escalated_at = NULL
			WHERE link_id = $1`, link.LinkID); err != nil {
			return nil, err
		}
		link.Status = federation.LinkActive
		res.Reactivated, changed = true, true
	}

	// a changed address must not land on someone else's principal
	if in.Email != "" && !equalFold(in.Email, p.email) {
		if clash, err := s.emailTaken(ctx, tx, in.TenantID, in.Email, link.PrincipalID); err != nil {
			return nil, err
		} else if clash {
			return nil, federation.ErrAmbiguousIdentity
		}
	}
	email, display := p.email, p.display
	if in.Email != "" {
		email = in.Email
	}
	if in.DisplayName != "" {
		display = in.DisplayName
	}
	if email != p.email || display != p.display {
		if _, err := tx.Exec(ctx, `UPDATE principals SET email = $1, display_name = $2, updated_at = NOW() WHERE principal_id = $3`,
			email, display, link.PrincipalID); err != nil {
			if isUnique(err) {
				return nil, federation.ErrAmbiguousIdentity
			}
			return nil, err
		}
		changed = true
	}
	res.Email, res.DisplayName = email, display

	attrsChanged := !sameAttributes(link.Attributes, in.Attributes) || link.Username != in.Username
	if attrsChanged {
		raw, _ := json.Marshal(in.Attributes)
		if _, err := tx.Exec(ctx, `UPDATE federation_links SET attributes = $1, username = $2, attributes_rev = attributes_rev + 1 WHERE link_id = $3`,
			raw, in.Username, link.LinkID); err != nil {
			return nil, err
		}
		link.Attributes, link.Username, link.AttributesRev = in.Attributes, in.Username, link.AttributesRev+1
		changed = true
	}

	// only a live link keeps entitlements; a link an operator suspended is left alone
	if link.Status == federation.LinkActive {
		planned, revoking, err := s.reconcileAssignmentsTx(ctx, tx, in, link)
		if err != nil {
			return nil, err
		}
		res.Planned, res.Revoking = planned, revoking
		if planned+revoking > 0 {
			changed = true
		}
	}
	res.Link, res.Unchanged = *link, !changed
	if changed {
		if err := s.fedEvent(ctx, tx, events.EventProvisioningApplied, in.TenantID, in.Actor, in.CorrelationID, link.PrincipalID,
			map[string]any{"connection_id": in.ConnectionID, "event_id": in.EventID, "link_id": link.LinkID, "principal_id": link.PrincipalID,
				"operation": in.Operation, "reactivated": res.Reactivated, "planned": res.Planned, "revoking": res.Revoking}); err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (s *PgStore) createIdentityTx(ctx context.Context, tx pgx.Tx, in federation.ApplyInput, res *federation.ApplyResult) (*federation.ApplyResult, error) {
	if clash, err := s.emailTaken(ctx, tx, in.TenantID, in.Email, ""); err != nil {
		return nil, err
	} else if clash {
		return nil, federation.ErrAmbiguousIdentity
	}
	principalID := ulid.Make().String()
	if _, err := tx.Exec(ctx, `INSERT INTO principals (principal_id, tenant_id, principal_type, identity_provider_subject, email, display_name, status)
		VALUES ($1,$2,'HUMAN',$3,$4,$5,'ACTIVE')`, principalID, in.TenantID, "federated:"+in.ConnectionID+":"+in.Subject, in.Email, in.DisplayName); err != nil {
		if isUnique(err) {
			return nil, federation.ErrAmbiguousIdentity
		}
		return nil, err
	}
	if err := s.decisionRow(ctx, tx, principalID, in.TenantID, "FEDERATION_PROVISION", in.EventID, in.CorrelationID); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(in.Attributes)
	source := in.LinkSource
	if source == "" {
		source = federation.SourceSCIM
	}
	l, err := scanLink(tx.QueryRow(ctx, `INSERT INTO federation_links (link_id, tenant_id, connection_id, issuer, subject, principal_id, status, link_source, linked_by, username, attributes)
		VALUES ($1,$2,$3,$4,$5,$6,'ACTIVE',$7,$8,$9,$10) RETURNING `+linkColumns,
		ulid.Make().String(), in.TenantID, in.ConnectionID, in.Issuer, in.Subject, principalID, source, in.Actor, in.Username, raw))
	if err != nil {
		if isUnique(err) {
			return nil, federation.ErrAmbiguousIdentity
		}
		return nil, err
	}
	planned, _, err := s.reconcileAssignmentsTx(ctx, tx, in, l)
	if err != nil {
		return nil, err
	}
	res.Link, res.Email, res.DisplayName, res.Created, res.Planned = *l, in.Email, in.DisplayName, true, planned
	if err := s.fedEvent(ctx, tx, events.EventIdentityLinkCreated, in.TenantID, in.Actor, in.CorrelationID, principalID,
		map[string]any{"link_id": l.LinkID, "connection_id": in.ConnectionID, "principal_id": principalID, "issuer": in.Issuer, "link_source": source}); err != nil {
		return nil, err
	}
	if err := s.fedEvent(ctx, tx, events.EventProvisioningApplied, in.TenantID, in.Actor, in.CorrelationID, principalID,
		map[string]any{"connection_id": in.ConnectionID, "event_id": in.EventID, "link_id": l.LinkID, "principal_id": principalID,
			"operation": in.Operation, "created": true, "planned": planned}); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *PgStore) deprovisionTx(ctx context.Context, tx pgx.Tx, in federation.ApplyInput, link *federation.Link, principalStatus string, res *federation.ApplyResult) (*federation.ApplyResult, error) {
	if link.Status == federation.LinkDeprovisioned {
		res.Unchanged = true
		return res, nil
	}
	received := in.ReceivedAt
	if received.IsZero() {
		received = time.Now().UTC()
	}
	slo := in.DeprovisionSLO
	if slo <= 0 {
		slo = 5 * time.Minute
	}
	if _, err := tx.Exec(ctx, `UPDATE federation_links SET status = 'DEPROVISIONED', deprovisioned_at = NOW(), deprovision_requested_at = $1,
		deprovision_due_at = $2, sessions_evicted_at = NULL, access_revoked_at = NULL, deprovision_escalated_at = NULL WHERE link_id = $3`,
		received, received.Add(slo), link.LinkID); err != nil {
		return nil, err
	}
	// the priority path: the person cannot sign in from this transaction on
	if principalStatus != "DISABLED" {
		if _, err := tx.Exec(ctx, `UPDATE principals SET status = 'DISABLED', updated_at = NOW() WHERE principal_id = $1`, link.PrincipalID); err != nil {
			return nil, err
		}
		if err := s.decisionRow(ctx, tx, link.PrincipalID, in.TenantID, "FEDERATION_DEPROVISION", in.EventID, in.CorrelationID); err != nil {
			return nil, err
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE federation_assignments SET status = 'REVOKE_PENDING', next_attempt_at = NOW(), updated_at = NOW()
		WHERE link_id = $1 AND status IN ('PLANNED','PROVISIONED')`, link.LinkID)
	if err != nil {
		return nil, err
	}
	res.Deprovisioned, res.Revoking = true, int(tag.RowsAffected())
	now := time.Now().UTC()
	link.Status, link.DeprovisionRequestedAt = federation.LinkDeprovisioned, &received
	link.SessionsEvictedAt, link.AccessRevokedAt, link.DeprovisionEscalatedAt = nil, nil, nil
	link.DeprovisionedAt = &now
	res.Link = *link
	return res, s.fedEvent(ctx, tx, events.EventFederationDeprovisionApplied, in.TenantID, in.Actor, in.CorrelationID, link.PrincipalID,
		map[string]any{"connection_id": in.ConnectionID, "event_id": in.EventID, "link_id": link.LinkID, "principal_id": link.PrincipalID,
			"requested_at": received, "revoking": res.Revoking})
}

// reconcileAssignmentsTx makes the queued work match the desired set: new roles are planned, roles no longer
// entitled are queued for revocation. A role is identified by its role and entity, not by which attribute
// entitles it, so a person who changes groups but keeps a role does not lose and regain it.
func (s *PgStore) reconcileAssignmentsTx(ctx context.Context, tx pgx.Tx, in federation.ApplyInput, link *federation.Link) (planned, revoking int, err error) {
	rows, err := tx.Query(ctx, `SELECT assignment_id, entry_key FROM federation_assignments WHERE link_id = $1 AND status IN ('PLANNED','PROVISIONED')`, link.LinkID)
	if err != nil {
		return 0, 0, err
	}
	have := map[string]string{}
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			rows.Close()
			return 0, 0, err
		}
		have[key] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	want := map[string]bool{}
	for _, d := range in.Desired {
		want[d.EntryKey] = true
		if _, ok := have[d.EntryKey]; ok {
			continue
		}
		id := ulid.Make().String()
		if _, err := tx.Exec(ctx, `INSERT INTO federation_assignments (assignment_id, tenant_id, connection_id, link_id, principal_id, mapping_version,
				entry_key, role_definition_id, legal_entity_id, correlation_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, in.TenantID, in.ConnectionID, link.LinkID, link.PrincipalID, in.MappingVersion,
			d.EntryKey, d.RoleDefinitionID, d.LegalEntityID, "fed-"+id); err != nil {
			return 0, 0, err
		}
		planned++
	}
	keys := make([]string, 0, len(have))
	for k := range have {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if want[k] {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE federation_assignments SET status = 'REVOKE_PENDING', next_attempt_at = NOW(), updated_at = NOW()
			WHERE assignment_id = $1`, have[k]); err != nil {
			return 0, 0, err
		}
		revoking++
	}
	return planned, revoking, nil
}

func (s *PgStore) emailTaken(ctx context.Context, tx pgx.Tx, tenantID, email, exceptPrincipal string) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE tenant_id = $1 AND lower(email) = lower($2::text)
		AND ($3::text = '' OR principal_id <> $3::text))`, tenantID, email, exceptPrincipal).Scan(&taken)
	return taken, err
}

func (s *PgStore) decisionRow(ctx context.Context, tx pgx.Tx, principalID, tenantID, action, basis, correlationID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO access_decision_log (decision_log_id, principal_id, tenant_id, action_type, decision_outcome, decision_basis, correlation_id)
		VALUES ($1,$2,$3,$4,'APPLIED',$5,$6)`, ulid.Make().String(), principalID, tenantID, action, basis, correlationID)
	return err
}

func equalFold(a, b string) bool {
	return len(a) == len(b) && lowerASCII(a) == lowerASCII(b)
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func sameAttributes(a, b federation.Attributes) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || len(av) != len(bv) {
			return false
		}
		x, y := append([]string(nil), av...), append([]string(nil), bv...)
		sort.Strings(x)
		sort.Strings(y)
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
	}
	return true
}

// ── reads ────────────────────────────────────────────────────────────────────

func userView(ctx context.Context, tx pgx.Tx, link *federation.Link) (*federation.UserView, error) {
	v := &federation.UserView{Link: *link}
	err := tx.QueryRow(ctx, `SELECT display_name, email FROM principals WHERE principal_id = $1`, link.PrincipalID).Scan(&v.DisplayName, &v.Email)
	return v, err
}

func (s *PgStore) GetUser(ctx context.Context, tenantID, connectionID, linkID string) (*federation.UserView, error) {
	var out *federation.UserView
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		l, err := scanLink(tx.QueryRow(ctx, `SELECT `+linkColumns+` FROM federation_links WHERE tenant_id = $1 AND connection_id = $2 AND link_id = $3`,
			tenantID, connectionID, linkID))
		if errors.Is(err, pgx.ErrNoRows) {
			return federation.ErrUserNotFound
		}
		if err != nil {
			return err
		}
		out, err = userView(ctx, tx, l)
		return err
	})
	return out, err
}

func (s *PgStore) FindUsers(ctx context.Context, tenantID, connectionID, attr, value string, limit int) ([]federation.UserView, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	col := "subject"
	if attr == "username" {
		col = "lower(username)"
		value = lowerASCII(value)
	}
	var out []federation.UserView
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+linkColumns+` FROM federation_links WHERE tenant_id = $1 AND connection_id = $2 AND `+col+` = $3 LIMIT $4`,
			tenantID, connectionID, value, limit)
		if err != nil {
			return err
		}
		var links []federation.Link
		for rows.Next() {
			l, err := scanLink(rows)
			if err != nil {
				rows.Close()
				return err
			}
			links = append(links, *l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range links {
			v, err := userView(ctx, tx, &links[i])
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return nil
	})
	return out, err
}

func (s *PgStore) ActiveLinks(ctx context.Context, tenantID, connectionID string) ([]federation.Link, error) {
	var out []federation.Link
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+linkColumns+` FROM federation_links
			WHERE tenant_id = $1 AND connection_id = $2 AND status = 'ACTIVE' AND link_source IN ('SCIM','JIT') ORDER BY created_at`, tenantID, connectionID)
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

// ── the assignment work queue ────────────────────────────────────────────────

const assignmentColumns = `assignment_id, tenant_id, connection_id, link_id, principal_id, mapping_version, entry_key, role_definition_id,
	legal_entity_id, COALESCE(request_id,''), correlation_id, status, reason, attempts, next_attempt_at, last_error, created_at, updated_at`

func scanAssignment(row pgx.Row) (*federation.Assignment, error) {
	var a federation.Assignment
	err := row.Scan(&a.AssignmentID, &a.TenantID, &a.ConnectionID, &a.LinkID, &a.PrincipalID, &a.MappingVersion, &a.EntryKey, &a.RoleDefinitionID,
		&a.LegalEntityID, &a.RequestID, &a.CorrelationID, &a.Status, &a.Reason, &a.Attempts, &a.NextAttemptAt, &a.LastError, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *PgStore) PendingAssignments(ctx context.Context, tenantID, linkID string) ([]federation.Assignment, error) {
	var out []federation.Assignment
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+assignmentColumns+` FROM federation_assignments
			WHERE tenant_id = $1 AND link_id = $2 AND status IN ('PLANNED','REVOKE_PENDING')
			ORDER BY (status = 'REVOKE_PENDING') DESC, created_at`, tenantID, linkID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAssignment(rows)
			if err != nil {
				return err
			}
			out = append(out, *a)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ClaimDueAssignments(ctx context.Context, limit int, lease time.Duration) ([]federation.Assignment, error) {
	var out []federation.Assignment
	err := s.withWorkerScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH due AS (
				SELECT assignment_id FROM federation_assignments
				WHERE status IN ('PLANNED','REVOKE_PENDING') AND next_attempt_at <= NOW()
				ORDER BY (status = 'REVOKE_PENDING') DESC, next_attempt_at
				LIMIT $1 FOR UPDATE SKIP LOCKED)
			UPDATE federation_assignments a SET next_attempt_at = NOW() + ($2::bigint * INTERVAL '1 millisecond')
			FROM due WHERE a.assignment_id = due.assignment_id
			RETURNING a.assignment_id, a.tenant_id, a.connection_id, a.link_id, a.principal_id, a.mapping_version, a.entry_key, a.role_definition_id,
				a.legal_entity_id, COALESCE(a.request_id,''), a.correlation_id, a.status, a.reason, a.attempts, a.next_attempt_at, a.last_error, a.created_at, a.updated_at`,
			limit, lease.Milliseconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			a, err := scanAssignment(rows)
			if err != nil {
				return err
			}
			out = append(out, *a)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) RecordAssignmentResult(ctx context.Context, tenantID, assignmentID string, res federation.AssignmentResult) (*federation.Assignment, error) {
	var out *federation.Assignment
	retry := res.Status == federation.AsgPlanned || res.Status == federation.AsgRevokePending
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var next any
		if retry {
			next = res.RetryAt
		}
		a, err := scanAssignment(tx.QueryRow(ctx, `UPDATE federation_assignments SET status = $3::text,
				request_id = COALESCE(NULLIF($4::text,''), request_id), reason = $5::text, last_error = $6::text,
				attempts = attempts + CASE WHEN $7::boolean THEN 1 ELSE 0 END,
				next_attempt_at = COALESCE($8::timestamptz, next_attempt_at), updated_at = NOW()
			WHERE assignment_id = $1 AND tenant_id = $2 AND status IN ('PLANNED','REVOKE_PENDING')
			RETURNING `+assignmentColumns,
			assignmentID, tenantID, res.Status, res.RequestID, res.Reason, trimTo(res.Error, 500), retry, next))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already settled by another replica
		}
		out = a
		return err
	})
	return out, err
}

// ── deprovisioning follow-through ────────────────────────────────────────────

func (s *PgStore) MarkSessionsEvicted(ctx context.Context, tenantID, linkID string) error {
	return s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE federation_links SET sessions_evicted_at = NOW()
			WHERE link_id = $1 AND tenant_id = $2 AND status = 'DEPROVISIONED' AND sessions_evicted_at IS NULL`, linkID, tenantID)
		return err
	})
}

func (s *PgStore) MarkAccessRevokedIfDone(ctx context.Context, tenantID, linkID string) (bool, error) {
	done := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE federation_links SET access_revoked_at = NOW()
			WHERE link_id = $1 AND tenant_id = $2 AND status = 'DEPROVISIONED' AND access_revoked_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM federation_assignments WHERE link_id = $1 AND status IN ('PLANNED','REVOKE_PENDING'))`, linkID, tenantID)
		done = err == nil && tag.RowsAffected() == 1
		return err
	})
	return done, err
}

func (s *PgStore) scanLinks(ctx context.Context, q string, args ...any) ([]federation.Link, error) {
	var out []federation.Link
	err := s.withWorkerScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
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

func (s *PgStore) LinksAwaitingEviction(ctx context.Context, limit int) ([]federation.Link, error) {
	return s.scanLinks(ctx, `SELECT `+linkColumns+` FROM federation_links WHERE status = 'DEPROVISIONED' AND sessions_evicted_at IS NULL
		ORDER BY deprovision_requested_at LIMIT $1`, limit)
}

func (s *PgStore) LateDeprovisions(ctx context.Context, now time.Time, limit int) ([]federation.Link, error) {
	return s.scanLinks(ctx, `SELECT `+linkColumns+` FROM federation_links
		WHERE status = 'DEPROVISIONED' AND deprovision_escalated_at IS NULL AND deprovision_due_at < $1
		  AND (sessions_evicted_at IS NULL OR access_revoked_at IS NULL) ORDER BY deprovision_due_at LIMIT $2`, now, limit)
}

func (s *PgStore) EscalateDeprovision(ctx context.Context, tenantID, linkID, detail string) (bool, error) {
	done := false
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		var connID, principalID string
		err := tx.QueryRow(ctx, `UPDATE federation_links SET deprovision_escalated_at = NOW()
			WHERE link_id = $1 AND tenant_id = $2 AND status = 'DEPROVISIONED' AND deprovision_escalated_at IS NULL
			RETURNING connection_id, principal_id`, linkID, tenantID).Scan(&connID, &principalID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO federation_sync_exceptions (exception_id, tenant_id, connection_id, link_id, kind, detail)
			VALUES ($1,$2,$3,$4,'DEPROVISION_LATE',$5) ON CONFLICT DO NOTHING`, ulid.Make().String(), tenantID, connID, linkID, trimTo(detail, 500)); err != nil {
			return err
		}
		done = true
		return s.fedEvent(ctx, tx, events.EventFederationDeprovisionLate, tenantID, "system", "", principalID,
			map[string]any{"connection_id": connID, "link_id": linkID, "principal_id": principalID, "detail": trimTo(detail, 200)})
	})
	return done, err
}

// ── exceptions ───────────────────────────────────────────────────────────────

const exceptionColumns = `exception_id, tenant_id, connection_id, COALESCE(event_id,''), COALESCE(link_id,''), COALESCE(assignment_id,''), kind, detail,
	status, created_at, COALESCE(resolved_by,''), resolved_at, resolution`

func scanException(row pgx.Row) (*federation.Exception, error) {
	var e federation.Exception
	if err := row.Scan(&e.ExceptionID, &e.TenantID, &e.ConnectionID, &e.EventID, &e.LinkID, &e.AssignmentID, &e.Kind, &e.Detail,
		&e.Status, &e.CreatedAt, &e.ResolvedBy, &e.ResolvedAt, &e.Resolution); err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *PgStore) AddException(ctx context.Context, ex federation.Exception) error {
	return s.withRLS(ctx, ex.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO federation_sync_exceptions (exception_id, tenant_id, connection_id, event_id, link_id, assignment_id, kind, detail)
			VALUES ($1,$2,$3,NULLIF($4::text,''),NULLIF($5::text,''),NULLIF($6::text,''),$7,$8) ON CONFLICT DO NOTHING`,
			ulid.Make().String(), ex.TenantID, ex.ConnectionID, ex.EventID, ex.LinkID, ex.AssignmentID, ex.Kind, trimTo(ex.Detail, 500))
		return err
	})
}

func (s *PgStore) ListExceptions(ctx context.Context, tenantID, connectionID, status string, limit int) ([]federation.Exception, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []federation.Exception
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+exceptionColumns+` FROM federation_sync_exceptions
			WHERE tenant_id = $1 AND connection_id = $2 AND ($3::text = '' OR status = $3::text) ORDER BY created_at DESC LIMIT $4`,
			tenantID, connectionID, status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanException(rows)
			if err != nil {
				return err
			}
			out = append(out, *e)
		}
		return rows.Err()
	})
	return out, err
}

func (s *PgStore) ResolveException(ctx context.Context, tenantID, exceptionID, actor, resolution string) (*federation.Exception, error) {
	var out *federation.Exception
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		e, err := scanException(tx.QueryRow(ctx, `UPDATE federation_sync_exceptions SET status = 'RESOLVED', resolved_by = $3, resolved_at = NOW(), resolution = $4
			WHERE exception_id = $1 AND tenant_id = $2 AND status = 'OPEN' RETURNING `+exceptionColumns, exceptionID, tenantID, actor, trimTo(resolution, 500)))
		if errors.Is(err, pgx.ErrNoRows) {
			var exists bool
			if qerr := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM federation_sync_exceptions WHERE exception_id = $1 AND tenant_id = $2)`,
				exceptionID, tenantID).Scan(&exists); qerr != nil {
				return qerr
			}
			if exists {
				return federation.ErrExceptionState
			}
			return federation.ErrNotFound
		}
		out = e
		return err
	})
	return out, err
}

// ── status and latency ───────────────────────────────────────────────────────

func countBy(ctx context.Context, tx pgx.Tx, q string, args ...any) (map[string]int, error) {
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func (s *PgStore) ProvisioningStatus(ctx context.Context, tenantID, connectionID string) (*federation.ProvStatus, error) {
	st := &federation.ProvStatus{ConnectionID: connectionID}
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status, active_mapping_version FROM federation_connections WHERE connection_id = $1 AND tenant_id = $2`,
			connectionID, tenantID).Scan(&st.ConnectionStatus, &st.ActiveMapping); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return federation.ErrNotFound
			}
			return err
		}
		var err error
		if st.Links, err = countBy(ctx, tx, `SELECT status, count(*)::int FROM federation_links WHERE connection_id = $1 GROUP BY 1`, connectionID); err != nil {
			return err
		}
		if st.Events, err = countBy(ctx, tx, `SELECT status, count(*)::int FROM federation_provisioning_events WHERE connection_id = $1 GROUP BY 1`, connectionID); err != nil {
			return err
		}
		if st.Assignments, err = countBy(ctx, tx, `SELECT status, count(*)::int FROM federation_assignments WHERE connection_id = $1 GROUP BY 1`, connectionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM federation_sync_exceptions WHERE connection_id = $1 AND status = 'OPEN'`, connectionID).Scan(&st.OpenExceptions); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM federation_links WHERE connection_id = $1 AND status = 'DEPROVISIONED'
			AND (sessions_evicted_at IS NULL OR access_revoked_at IS NULL)`, connectionID).Scan(&st.DeprovisionBacklog); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT max(received_at) FROM federation_provisioning_events WHERE connection_id = $1`, connectionID).Scan(&st.LastEventAt)
	})
	return st, err
}

func (s *PgStore) DeprovisionLatency(ctx context.Context, tenantID, connectionID string, limit int) ([]federation.LatencyRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var out []federation.LatencyRow
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT link_id, subject, deprovision_requested_at, deprovisioned_at, sessions_evicted_at, access_revoked_at,
				deprovision_due_at, deprovision_escalated_at IS NOT NULL
			FROM federation_links WHERE tenant_id = $1 AND connection_id = $2 AND deprovision_requested_at IS NOT NULL
			ORDER BY deprovision_requested_at DESC LIMIT $3`, tenantID, connectionID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r federation.LatencyRow
			if err := rows.Scan(&r.LinkID, &r.Subject, &r.RequestedAt, &r.AppliedAt, &r.SessionsEvictedAt, &r.AccessRevokedAt, &r.DueAt, &r.Escalated); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
