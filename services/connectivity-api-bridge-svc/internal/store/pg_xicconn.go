package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/registry"
	"zoiko.io/connectivity-api-bridge-svc/internal/xicconn"
)

// XICConnRepo is the Postgres implementation of xicconn.Repo (migration 005).
type XICConnRepo struct{ p *PgStore }

// NewXICConnRepo wraps a PgStore.
func NewXICConnRepo(p *PgStore) *XICConnRepo { return &XICConnRepo{p: p} }

var _ xicconn.Repo = (*XICConnRepo)(nil)

func (r *XICConnRepo) enqueue(ctx context.Context, tx pgx.Tx, tenant string, e xicconn.OutEvent) error {
	return r.p.enqueue(ctx, tx, tenant, domain.OutboxEvent{Type: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		ActorID: e.ActorID, CorrelationID: e.CorrelationID, Data: e.Data})
}

// withWorker runs fn as the cross-tenant reader: no tenant, only the worker flag, which the policies let SELECT.
func (r *XICConnRepo) withWorker(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := r.p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true), set_config('app.xic_worker', 'true', true)`); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// connErr maps what the database refused to the error a caller can act on.
func connErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch pgCode(err) {
	case "23505":
		switch {
		case strings.Contains(msg, "xic_bind_ref_exclusive"):
			return xicconn.ErrCrossTenantCredential
		case strings.Contains(msg, "xic_conn_one_per_account"):
			return xicconn.ErrExists
		}
	case "23000":
		switch {
		case strings.Contains(msg, "passed validation"):
			return xicconn.ErrNotValidated
		case strings.Contains(msg, "certified"):
			return xicconn.ErrNotCertified
		case strings.Contains(msg, "status cannot go"), strings.Contains(msg, "terminal"):
			return xicconn.ErrTransition
		case strings.Contains(msg, "PUBLISHED"), strings.Contains(msg, "does not belong"):
			return xicconn.ErrContractUnavailable
		}
	case "23514":
		if strings.Contains(msg, "xic_conn_live_is_activated") {
			return xicconn.ErrSelfAction
		}
	}
	return err
}

const xicConnCols = `connection_id::text, tenant_id, environment, connector_id, contract_version_id::text, status, status_reason, region,
	endpoints, provider_account_hash, external_org_hash, COALESCE(bridge_id, ''), config_fingerprint, capability_snapshot, created_by,
	consent_actor, activated_by, activated_at, suspended_at, revoked_at, version, created_at, updated_at`

func scanXICConn(row pgx.Row, c *xicconn.Connection) error {
	var endpoints, caps []byte
	if err := row.Scan(&c.ConnectionID, &c.TenantID, &c.Environment, &c.ConnectorID, &c.ContractVersionID, &c.Status, &c.StatusReason,
		&c.Region, &endpoints, &c.ProviderAccountHash, &c.ExternalOrgHash, &c.BridgeID, &c.ConfigFingerprint, &caps, &c.CreatedBy,
		&c.ConsentActor, &c.ActivatedBy, &c.ActivatedAt, &c.SuspendedAt, &c.RevokedAt, &c.Version, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return err
	}
	c.Endpoints = map[string]string{}
	if err := json.Unmarshal(endpoints, &c.Endpoints); err != nil {
		return err
	}
	if len(caps) > 0 && string(caps) != "null" {
		c.CapabilitySnapshot = json.RawMessage(caps)
	}
	c.Bindings = []xicconn.Binding{}
	return nil
}

func loadBindings(ctx context.Context, tx pgx.Tx, id string) ([]xicconn.Binding, error) {
	rows, err := tx.Query(ctx, `SELECT binding_id::text, auth_profile, current_ref, COALESCE(previous_ref, ''), previous_grace_until, granted_scopes,
		scope_fingerprint, excess_scope_ack_by, status, expires_at, rotation_due_at, bound_by, bound_at
		FROM xic_credential_bindings WHERE connection_id=$1::uuid ORDER BY auth_profile`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []xicconn.Binding{}
	for rows.Next() {
		var b xicconn.Binding
		if err := rows.Scan(&b.BindingID, &b.AuthProfile, &b.CurrentRef, &b.PreviousRef, &b.PreviousGraceUntil, &b.GrantedScopes, &b.ScopeFingerprint,
			&b.ExcessScopeAckBy, &b.Status, &b.ExpiresAt, &b.RotationDueAt, &b.BoundBy, &b.BoundAt); err != nil {
			return nil, err
		}
		if b.GrantedScopes == nil {
			b.GrantedScopes = []string{}
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func loadConn(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*xicconn.Connection, error) {
	q := `SELECT ` + xicConnCols + ` FROM xic_connections WHERE connection_id=$1::uuid`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var c xicconn.Connection
	if err := scanXICConn(tx.QueryRow(ctx, q, id), &c); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, xicconn.ErrNotFound
		}
		return nil, err
	}
	bs, err := loadBindings(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	c.Bindings = bs
	return &c, nil
}

func (r *XICConnRepo) claimIdem(ctx context.Context, tx pgx.Tx, key, hash, resource string) (string, error) {
	if key == "" {
		return "", nil
	}
	tenant := middleware.GetTenantID(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO xic_conn_idempotency (tenant_id, scope, idem_key, request_hash, resource_id) VALUES ($1,'connection',$2,$3,$4)
		ON CONFLICT (tenant_id, scope, idem_key) DO NOTHING`, tenant, key, hash, resource)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 1 {
		return "", nil
	}
	var h, res string
	if err := tx.QueryRow(ctx, `SELECT request_hash, resource_id FROM xic_conn_idempotency WHERE tenant_id=$1 AND scope='connection' AND idem_key=$2`,
		tenant, key).Scan(&h, &res); err != nil {
		return "", err
	}
	if h != hash {
		return "", xicconn.ErrIdempotency
	}
	return res, nil
}

func (r *XICConnRepo) Create(ctx context.Context, c *xicconn.Connection, idemKey, idemHash string, ev xicconn.OutEvent) (bool, error) {
	created := true
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		existing, err := r.claimIdem(ctx, tx, idemKey, idemHash, c.ConnectionID)
		if err != nil {
			return err
		}
		if existing != "" {
			created = false
			cur, err := loadConn(ctx, tx, existing, false)
			if err != nil {
				return err
			}
			*c = *cur
			return nil
		}
		endpoints, _ := json.Marshal(c.Endpoints)
		var bridge any
		if c.BridgeID != "" {
			bridge = c.BridgeID
		}
		if _, err := tx.Exec(ctx, `INSERT INTO xic_connections (connection_id, tenant_id, environment, connector_id, contract_version_id, status, region, endpoints,
			provider_account_hash, external_org_hash, bridge_id, config_fingerprint, created_by, version, created_at, updated_at)
			VALUES ($1::uuid,$2,$3,$4,$5::uuid,'DRAFT',$6,$7,$8,$9,$10,$11,$12,1,$13,$14)`,
			c.ConnectionID, c.TenantID, c.Environment, c.ConnectorID, c.ContractVersionID, c.Region, endpoints, c.ProviderAccountHash,
			c.ExternalOrgHash, bridge, c.ConfigFingerprint, c.CreatedBy, c.CreatedAt, c.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO xic_connection_events (connection_id, tenant_id, kind, to_status, actor, at) VALUES ($1::uuid,$2,'CREATE','DRAFT',$3,$4)`,
			c.ConnectionID, c.TenantID, c.CreatedBy, c.CreatedAt); err != nil {
			return err
		}
		return r.enqueue(ctx, tx, c.TenantID, ev)
	})
	if err != nil {
		return false, connErr(err)
	}
	return created, nil
}

func (r *XICConnRepo) Get(ctx context.Context, id string) (*xicconn.Connection, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, xicconn.ErrNotFound
	}
	var c *xicconn.Connection
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = loadConn(ctx, tx, id, false)
		return err
	})
	return c, err
}

func (r *XICConnRepo) GetAcrossTenants(ctx context.Context, id string) (*xicconn.Connection, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, xicconn.ErrNotFound
	}
	var c *xicconn.Connection
	err := r.withWorker(ctx, func(tx pgx.Tx) error {
		var err error
		c, err = loadConn(ctx, tx, id, false)
		return err
	})
	return c, err
}

func (r *XICConnRepo) List(ctx context.Context, f xicconn.Filter) ([]xicconn.Connection, error) {
	out := []xicconn.Connection{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+xicConnCols+` FROM xic_connections
			WHERE ($1='' OR status=$1) AND ($2='' OR connector_id=$2) AND ($3='' OR environment=$3) ORDER BY created_at, connection_id`,
			f.Status, f.ConnectorID, f.Environment)
		if err != nil {
			return err
		}
		var ids []xicconn.Connection
		for rows.Next() {
			var c xicconn.Connection
			if err := scanXICConn(rows, &c); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range ids {
			bs, err := loadBindings(ctx, tx, ids[i].ConnectionID)
			if err != nil {
				return err
			}
			ids[i].Bindings = bs
		}
		out = ids
		if out == nil {
			out = []xicconn.Connection{}
		}
		return nil
	})
	return out, err
}

func loadChecks(ctx context.Context, tx pgx.Tx, id string) ([]xicconn.Check, error) {
	rows, err := tx.Query(ctx, `SELECT check_id::text, kind, outcome, config_fingerprint, issues, detail, run_by, at FROM xic_connection_checks
		WHERE connection_id=$1::uuid ORDER BY ord`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []xicconn.Check{}
	for rows.Next() {
		var c xicconn.Check
		var issues, detail []byte
		if err := rows.Scan(&c.CheckID, &c.Kind, &c.Outcome, &c.ConfigFingerprint, &issues, &detail, &c.RunBy, &c.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(issues, &c.Issues); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			c.Detail = json.RawMessage(detail)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func loadCerts(ctx context.Context, tx pgx.Tx, id string) ([]xicconn.Certification, error) {
	rows, err := tx.Query(ctx, `SELECT certification_id::text, connection_id::text, environment, contract_version_id::text, scope_fingerprint, test_pack_version,
		passed_tests, outcome, certified_by, at, expires_at FROM xic_certifications WHERE connection_id=$1::uuid ORDER BY ord`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []xicconn.Certification{}
	for rows.Next() {
		var c xicconn.Certification
		if err := rows.Scan(&c.CertificationID, &c.ConnectionID, &c.Environment, &c.ContractVersionID, &c.ScopeFingerprint, &c.TestPackVersion,
			&c.PassedTests, &c.Outcome, &c.CertifiedBy, &c.At, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func loadRequests(ctx context.Context, tx pgx.Tx, id string) ([]xicconn.Request, error) {
	rows, err := tx.Query(ctx, `SELECT request_id::text, connection_id::text, kind, payload, reason, status, requested_by, COALESCE(decided_by, ''),
		decided_at, effective_at, expires_at, created_at FROM xic_connection_requests WHERE connection_id=$1::uuid ORDER BY created_at, request_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []xicconn.Request{}
	for rows.Next() {
		var q xicconn.Request
		var payload []byte
		if err := rows.Scan(&q.RequestID, &q.ConnectionID, &q.Kind, &payload, &q.Reason, &q.Status, &q.RequestedBy, &q.DecidedBy,
			&q.DecidedAt, &q.EffectiveAt, &q.ExpiresAt, &q.CreatedAt); err != nil {
			return nil, err
		}
		q.Payload = json.RawMessage(payload)
		out = append(out, q)
	}
	return out, rows.Err()
}

func (r *XICConnRepo) Mutate(ctx context.Context, id string, fn func(tx *xicconn.Tx) error) (*xicconn.Connection, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, xicconn.ErrNotFound
	}
	var out *xicconn.Connection
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		cur, err := loadConn(ctx, pg, id, true)
		if err != nil {
			return err
		}
		checks, err := loadChecks(ctx, pg, id)
		if err != nil {
			return err
		}
		certs, err := loadCerts(ctx, pg, id)
		if err != nil {
			return err
		}
		reqs, err := loadRequests(ctx, pg, id)
		if err != nil {
			return err
		}
		tx := &xicconn.Tx{C: cloneConn(cur), Checks: checks, Certs: certs, Requests: reqs}
		var sess xicconn.Session
		switch err := pg.QueryRow(ctx, `SELECT session_id::text, state_hash, redirect_uri, expires_at, consumed_at, created_by, created_at
			FROM xic_authorization_sessions WHERE connection_id=$1::uuid ORDER BY created_at DESC, session_id LIMIT 1`, id).
			Scan(&sess.SessionID, &sess.StateHash, &sess.RedirectURI, &sess.ExpiresAt, &sess.ConsumedAt, &sess.CreatedBy, &sess.CreatedAt); {
		case err == nil:
			tx.Session = &sess
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		w := tx.C
		tenant := cur.TenantID

		// evidence and new rows first
		if tx.NewEvent != nil {
			if _, err := pg.Exec(ctx, `INSERT INTO xic_connection_events (connection_id, tenant_id, kind, from_status, to_status, actor, reason, at)
				VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8)`, id, tenant, tx.NewEvent.Kind, tx.NewEvent.From, tx.NewEvent.To, tx.NewEvent.Actor, tx.NewEvent.Reason, tx.NewEvent.At); err != nil {
				return err
			}
		}
		if tx.NewCheck != nil {
			ck := tx.NewCheck
			issues, _ := json.Marshal(nonNilXICIssues(ck.Issues))
			if _, err := pg.Exec(ctx, `INSERT INTO xic_connection_checks (check_id, connection_id, tenant_id, kind, outcome, config_fingerprint, issues, detail, run_by, at)
				VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10)`, ck.CheckID, id, tenant, ck.Kind, ck.Outcome, ck.ConfigFingerprint, issues,
				nullableJSON(ck.Detail), ck.RunBy, ck.At); err != nil {
				return err
			}
		}
		if tx.NewCert != nil {
			c := tx.NewCert
			passed := c.PassedTests
			if passed == nil {
				passed = []string{}
			}
			if _, err := pg.Exec(ctx, `INSERT INTO xic_certifications (certification_id, connection_id, tenant_id, environment, contract_version_id, scope_fingerprint,
				test_pack_version, passed_tests, outcome, certified_by, at, expires_at) VALUES ($1::uuid,$2::uuid,$3,$4,$5::uuid,$6,$7,$8,$9,$10,$11,$12)`,
				c.CertificationID, id, tenant, c.Environment, c.ContractVersionID, c.ScopeFingerprint, c.TestPackVersion, passed, c.Outcome, c.CertifiedBy, c.At, c.ExpiresAt); err != nil {
				return err
			}
		}
		if tx.NewSession != nil {
			s := tx.NewSession
			if _, err := pg.Exec(ctx, `INSERT INTO xic_authorization_sessions (session_id, connection_id, tenant_id, state_hash, redirect_uri, expires_at, created_by, created_at)
				VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8)`, s.SessionID, id, tenant, s.StateHash, s.RedirectURI, s.ExpiresAt, s.CreatedBy, s.CreatedAt); err != nil {
				return err
			}
		}
		if tx.ConsumeSession && tx.Session != nil {
			if _, err := pg.Exec(ctx, `UPDATE xic_authorization_sessions SET consumed_at=now() WHERE session_id=$1::uuid AND consumed_at IS NULL`, tx.Session.SessionID); err != nil {
				return err
			}
		}
		if q := tx.NewRequest; q != nil {
			if _, err := pg.Exec(ctx, `INSERT INTO xic_connection_requests (request_id, connection_id, tenant_id, kind, payload, reason, status, requested_by, effective_at, expires_at, created_at)
				VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,'PENDING',$7,$8,$9,$10)`, q.RequestID, id, tenant, q.Kind, []byte(q.Payload), q.Reason, q.RequestedBy, q.EffectiveAt, q.ExpiresAt, q.CreatedAt); err != nil {
				return err
			}
		}
		if q := tx.UpdateRequest; q != nil {
			var decidedBy any
			if q.DecidedBy != "" {
				decidedBy = q.DecidedBy
			}
			if _, err := pg.Exec(ctx, `UPDATE xic_connection_requests SET status=$2, decided_by=$3, decided_at=$4 WHERE request_id=$1::uuid`,
				q.RequestID, q.Status, decidedBy, q.DecidedAt); err != nil {
				return err
			}
		}

		// bindings
		for _, b := range w.Bindings {
			var prev any
			if b.PreviousRef != "" {
				prev = b.PreviousRef
			}
			scopes := b.GrantedScopes
			if scopes == nil {
				scopes = []string{}
			}
			if _, err := pg.Exec(ctx, `INSERT INTO xic_credential_bindings (binding_id, connection_id, tenant_id, auth_profile, current_ref, previous_ref, previous_grace_until,
				granted_scopes, scope_fingerprint, excess_scope_ack_by, status, expires_at, rotation_due_at, bound_by, bound_at)
				VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
				ON CONFLICT (connection_id, auth_profile) DO UPDATE SET current_ref=EXCLUDED.current_ref, previous_ref=EXCLUDED.previous_ref,
				previous_grace_until=EXCLUDED.previous_grace_until, granted_scopes=EXCLUDED.granted_scopes, scope_fingerprint=EXCLUDED.scope_fingerprint,
				excess_scope_ack_by=EXCLUDED.excess_scope_ack_by, status=EXCLUDED.status, expires_at=EXCLUDED.expires_at,
				rotation_due_at=EXCLUDED.rotation_due_at, bound_by=EXCLUDED.bound_by, bound_at=EXCLUDED.bound_at`,
				b.BindingID, id, tenant, b.AuthProfile, b.CurrentRef, prev, b.PreviousGraceUntil, scopes, b.ScopeFingerprint, b.ExcessScopeAckBy,
				b.Status, b.ExpiresAt, b.RotationDueAt, b.BoundBy, b.BoundAt); err != nil {
				return err
			}
		}

		// the connection itself
		if tx.ApplyMigration != "" {
			if _, err := pg.Exec(ctx, `SELECT set_config('app.xic_migration', 'true', true)`); err != nil {
				return err
			}
		}
		endpoints, _ := json.Marshal(w.Endpoints)
		var capsJSON any
		if len(w.CapabilitySnapshot) > 0 {
			capsJSON = []byte(w.CapabilitySnapshot)
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_connections SET contract_version_id=$2::uuid, status=$3, status_reason=$4, region=$5, endpoints=$6,
			config_fingerprint=$7, capability_snapshot=$8, consent_actor=$9, activated_by=$10, activated_at=$11, suspended_at=$12, revoked_at=$13,
			version=version+1, updated_at=now() WHERE connection_id=$1::uuid`,
			id, w.ContractVersionID, w.Status, w.StatusReason, w.Region, endpoints, w.ConfigFingerprint, capsJSON, w.ConsentActor, w.ActivatedBy,
			w.ActivatedAt, w.SuspendedAt, w.RevokedAt); err != nil {
			return err
		}
		for _, e := range tx.Out {
			if err := r.enqueue(ctx, pg, tenant, e); err != nil {
				return err
			}
		}
		out, err = loadConn(ctx, pg, id, false)
		return err
	})
	if err != nil {
		return nil, connErr(err)
	}
	return out, nil
}

func cloneConn(c *xicconn.Connection) *xicconn.Connection {
	raw, _ := json.Marshal(c)
	var out xicconn.Connection
	_ = json.Unmarshal(raw, &out)
	if out.Bindings == nil {
		out.Bindings = []xicconn.Binding{}
	}
	if out.Endpoints == nil {
		out.Endpoints = map[string]string{}
	}
	return &out
}

func nonNilXICIssues(is []registry.Issue) []registry.Issue {
	if is == nil {
		return []registry.Issue{}
	}
	return is
}

func (r *XICConnRepo) ListChecks(ctx context.Context, id string) ([]xicconn.Check, error) {
	return listFor(ctx, r, id, loadChecks)
}
func (r *XICConnRepo) ListCertifications(ctx context.Context, id string) ([]xicconn.Certification, error) {
	return listFor(ctx, r, id, loadCerts)
}
func (r *XICConnRepo) ListRequests(ctx context.Context, id string) ([]xicconn.Request, error) {
	return listFor(ctx, r, id, loadRequests)
}

func listFor[T any](ctx context.Context, r *XICConnRepo, id string, load func(context.Context, pgx.Tx, string) ([]T, error)) ([]T, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, xicconn.ErrNotFound
	}
	var out []T
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_connections WHERE connection_id=$1::uuid`, id).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return xicconn.ErrNotFound
			}
			return err
		}
		var err error
		out, err = load(ctx, tx, id)
		return err
	})
	return out, err
}

func (r *XICConnRepo) ListEvents(ctx context.Context, id string) ([]xicconn.Event, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, xicconn.ErrNotFound
	}
	out := []xicconn.Event{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_connections WHERE connection_id=$1::uuid`, id).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return xicconn.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT kind, from_status, to_status, actor, reason, at FROM xic_connection_events WHERE connection_id=$1::uuid ORDER BY ord`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e xicconn.Event
			if err := rows.Scan(&e.Kind, &e.From, &e.To, &e.Actor, &e.Reason, &e.At); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

func (r *XICConnRepo) Due(ctx context.Context, now time.Time, window time.Duration, limit int) ([]xicconn.DueRef, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	var out []xicconn.DueRef
	err := r.withWorker(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT c.tenant_id, c.connection_id::text FROM xic_connections c
			WHERE c.status <> 'REVOKED' AND (
			      EXISTS (SELECT 1 FROM xic_credential_bindings b WHERE b.connection_id = c.connection_id AND b.status <> 'REVOKED'
			              AND ((b.expires_at IS NOT NULL AND b.expires_at <= $1::timestamptz + make_interval(secs => $2))
			                   OR (b.previous_grace_until IS NOT NULL AND b.previous_grace_until <= $1)
			                   OR b.status IN ('EXPIRING', 'EXPIRED')))
			   OR EXISTS (SELECT 1 FROM xic_connection_requests q WHERE q.connection_id = c.connection_id AND q.kind = 'MIGRATION'
			              AND q.status = 'APPROVED' AND q.effective_at IS NOT NULL AND q.effective_at <= $1))
			ORDER BY 1, 2 LIMIT $3`, now, window.Seconds(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d xicconn.DueRef
			if err := rows.Scan(&d.TenantID, &d.ConnectionID); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

func (r *XICConnRepo) RecordSignal(ctx context.Context, tenant string, ev xicconn.OutEvent) error {
	return r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return r.enqueue(ctx, tx, tenant, ev)
	})
}
