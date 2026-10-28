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
	"zoiko.io/connectivity-api-bridge-svc/internal/rollout"
)

// RolloutRepo is the Postgres implementation of rollout.Repo (migration 011). Its Facts methods read the real
// exchange, attempt, batch and variance tables; nothing here writes to any of them.
type RolloutRepo struct{ p *PgStore }

// NewRolloutRepo wraps a PgStore.
func NewRolloutRepo(p *PgStore) *RolloutRepo { return &RolloutRepo{p: p} }

var _ rollout.Repo = (*RolloutRepo)(nil)

func (r *RolloutRepo) enqueue(ctx context.Context, tx pgx.Tx, tenant string, e rollout.OutEvent) error {
	return r.p.enqueue(ctx, tx, tenant, domain.OutboxEvent{Type: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		ActorID: e.ActorID, CorrelationID: e.CorrelationID, Data: e.Data})
}

func (r *RolloutRepo) workerTx(ctx context.Context, fn func(pgx.Tx) error) error {
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

func rolloutErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch pgCode(err) {
	case "23000":
		return rollout.ErrState
	case "23505":
		switch {
		case strings.Contains(msg, "xic_ro_live"):
			return rollout.ErrExists
		case strings.Contains(msg, "xic_lc_name"):
			return rollout.ErrExists
		case strings.Contains(msg, "xic_rq_one_open"):
			return rollout.ErrState
		}
	case "23514":
		if strings.Contains(msg, "xic_rq_decider") {
			return rollout.ErrSelfAction
		}
	}
	return err
}

func (r *RolloutRepo) insertEvent(ctx context.Context, tx pgx.Tx, tenant string, ev rollout.Event) error {
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_rollout_events (event_id, tenant_id, subject_kind, subject_id, kind, actor_id, detail, created_at)
		VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7, $8)`, uuid.NewString(), tenant, ev.SubjectKind, ev.SubjectID, ev.Kind, ev.Actor, jsonOrNil(ev.Detail), at)
	return err
}

func (r *RolloutRepo) finish(ctx context.Context, tx pgx.Tx, tenant string, evs []rollout.Event, outs []rollout.OutEvent) error {
	for _, ev := range evs {
		if err := r.insertEvent(ctx, tx, tenant, ev); err != nil {
			return err
		}
	}
	for _, o := range outs {
		if err := r.enqueue(ctx, tx, tenant, o); err != nil {
			return err
		}
	}
	return nil
}

// ── rollouts ───────────────────────────────────────────────────────────────────

const rolloutCols = `rollout_id::text, tenant_id, connection_id::text, connector_id, stage, pilot_ops, pilot_daily_cap, min_soak_hours, min_success, max_failure_pct, stage_since,
	COALESCE(advance_request::text, ''), rollback_reason, rolled_back_by, created_by, version, created_at, updated_at`

func scanRollout(row pgx.Row, x *rollout.Rollout) error {
	return row.Scan(&x.RolloutID, &x.TenantID, &x.ConnectionID, &x.ConnectorID, &x.Stage, &x.PilotOps, &x.PilotDailyCap, &x.MinSoakHours, &x.MinSuccess, &x.MaxFailurePct,
		&x.StageSince, &x.AdvanceRequest, &x.RollbackReason, &x.RolledBackBy, &x.CreatedBy, &x.Version, &x.CreatedAt, &x.UpdatedAt)
}

func loadRollout(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*rollout.Rollout, error) {
	var x rollout.Rollout
	q := `SELECT ` + rolloutCols + ` FROM xic_rollouts WHERE rollout_id=$1::uuid`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	if err := scanRollout(tx.QueryRow(ctx, q, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, rollout.ErrNotFound
		}
		return nil, err
	}
	return &x, nil
}

// InsertRollout implements rollout.Repo.
func (r *RolloutRepo) InsertRollout(ctx context.Context, x *rollout.Rollout, ev rollout.Event, out []rollout.OutEvent) error {
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO xic_rollouts (rollout_id, tenant_id, connection_id, connector_id, stage, pilot_ops, pilot_daily_cap, min_soak_hours, min_success,
			max_failure_pct, stage_since, created_by, version, created_at, updated_at) VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			x.RolloutID, x.TenantID, x.ConnectionID, x.ConnectorID, x.Stage, x.PilotOps, x.PilotDailyCap, x.MinSoakHours, x.MinSuccess, x.MaxFailurePct, x.StageSince,
			x.CreatedBy, x.Version, x.CreatedAt, x.UpdatedAt); err != nil {
			return err
		}
		return r.finish(ctx, tx, x.TenantID, []rollout.Event{ev}, out)
	})
	return rolloutErr(err)
}

// GetRollout implements rollout.Repo.
func (r *RolloutRepo) GetRollout(ctx context.Context, id string) (*rollout.Rollout, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, rollout.ErrNotFound
	}
	var x *rollout.Rollout
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		x, err = loadRollout(ctx, tx, id, false)
		return err
	})
	return x, err
}

// LatestRolloutFor implements rollout.Repo.
func (r *RolloutRepo) LatestRolloutFor(ctx context.Context, connectionID string) (*rollout.Rollout, error) {
	if _, err := uuid.Parse(connectionID); err != nil {
		return nil, rollout.ErrNotFound
	}
	var x rollout.Rollout
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		err := scanRollout(tx.QueryRow(ctx, `SELECT `+rolloutCols+` FROM xic_rollouts WHERE connection_id=$1::uuid ORDER BY created_at DESC, rollout_id LIMIT 1`, connectionID), &x)
		if errors.Is(err, pgx.ErrNoRows) {
			return rollout.ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &x, nil
}

// ListRollouts implements rollout.Repo.
func (r *RolloutRepo) ListRollouts(ctx context.Context, f rollout.RolloutFilter) ([]rollout.Rollout, error) {
	limit := f.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	var conn any
	if f.ConnectionID != "" {
		if _, err := uuid.Parse(f.ConnectionID); err != nil {
			return []rollout.Rollout{}, nil
		}
		conn = f.ConnectionID
	}
	out := []rollout.Rollout{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+rolloutCols+` FROM xic_rollouts WHERE ($1 = '' OR stage = $1) AND ($2::uuid IS NULL OR connection_id = $2::uuid)
			ORDER BY created_at DESC, rollout_id LIMIT $3`, f.Stage, conn, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x rollout.Rollout
			if err := scanRollout(rows, &x); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

// MutateRollout implements rollout.Repo.
func (r *RolloutRepo) MutateRollout(ctx context.Context, id string, fn func(tx *rollout.RolloutTx) error) (*rollout.Rollout, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, rollout.ErrNotFound
	}
	var out *rollout.Rollout
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		cur, err := loadRollout(ctx, pg, id, true)
		if err != nil {
			return err
		}
		w := *cur
		tx := &rollout.RolloutTx{R: &w}
		if err := fn(tx); err != nil {
			return err
		}
		if tx.Use != "" {
			tag, err := pg.Exec(ctx, `UPDATE xic_rollout_requests SET status='USED', version=version+1 WHERE request_id=$1::uuid AND subject_kind='ROLLOUT'
				AND subject_id=$2::uuid AND status='APPROVED'`, tx.Use, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return rollout.ErrState
			}
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_rollouts SET stage=$2, stage_since=$3, advance_request=$4::uuid, rollback_reason=$5, rolled_back_by=$6, version=version+1, updated_at=now()
			WHERE rollout_id=$1::uuid`, id, w.Stage, w.StageSince, nulStr(w.AdvanceRequest), w.RollbackReason, w.RolledBackBy); err != nil {
			return err
		}
		if err := r.finish(ctx, pg, cur.TenantID, tx.Event, tx.Out); err != nil {
			return err
		}
		out, err = loadRollout(ctx, pg, id, false)
		return err
	})
	if err != nil {
		return nil, rolloutErr(err)
	}
	return out, nil
}

// PilotRollouts implements rollout.Repo.
func (r *RolloutRepo) PilotRollouts(ctx context.Context, limit int) ([]rollout.DueRef, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	var out []rollout.DueRef
	err := r.workerTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, rollout_id::text FROM xic_rollouts WHERE stage = 'PILOT' ORDER BY stage_since, rollout_id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d rollout.DueRef
			if err := rows.Scan(&d.TenantID, &d.ID); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// Admit implements rollout.Repo: one atomic counter per rollout and day.
func (r *RolloutRepo) Admit(ctx context.Context, x *rollout.Rollout, day time.Time) (bool, error) {
	ok := false
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `INSERT INTO xic_rollout_usage (rollout_id, tenant_id, day, admitted) VALUES ($1::uuid, $2, $3::date, 1)
			ON CONFLICT (rollout_id, day) DO UPDATE SET admitted = xic_rollout_usage.admitted + 1 WHERE xic_rollout_usage.admitted < $4
			RETURNING admitted`, x.RolloutID, x.TenantID, day.UTC(), x.PilotDailyCap).Scan(&n)
		switch {
		case err == nil:
			ok = true
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			return nil // the day's budget is spent
		}
		return err
	})
	return ok, err
}

// UsedToday implements rollout.Repo.
func (r *RolloutRepo) UsedToday(ctx context.Context, rolloutID string, day time.Time) (int, error) {
	n := 0
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT admitted FROM xic_rollout_usage WHERE rollout_id=$1::uuid AND day=$2::date`, rolloutID, day.UTC()).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return n, err
}

// ── requests ───────────────────────────────────────────────────────────────────

const reqCols = `request_id::text, tenant_id, subject_kind, subject_id::text, from_state, to_state, status, attestations, readiness, reason, requested_by, decided_by,
	decision_note, expires_at, decided_at, created_at, version`

func scanRequest(row pgx.Row, q *rollout.Request) error {
	var att, ready []byte
	if err := row.Scan(&q.RequestID, &q.TenantID, &q.SubjectKind, &q.SubjectID, &q.FromState, &q.ToState, &q.Status, &att, &ready, &q.Reason, &q.RequestedBy,
		&q.DecidedBy, &q.DecisionNote, &q.ExpiresAt, &q.DecidedAt, &q.CreatedAt, &q.Version); err != nil {
		return err
	}
	q.Attestations = []string{}
	_ = json.Unmarshal(att, &q.Attestations)
	q.Readiness = json.RawMessage(ready)
	return nil
}

// InsertRequest implements rollout.Repo.
func (r *RolloutRepo) InsertRequest(ctx context.Context, q *rollout.Request, ev rollout.Event, out []rollout.OutEvent) error {
	att, _ := json.Marshal(q.Attestations)
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		ready := []byte(q.Readiness)
		if len(ready) == 0 {
			ready = []byte(`{}`)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO xic_rollout_requests (request_id, tenant_id, subject_kind, subject_id, from_state, to_state, status, attestations, readiness, reason,
			requested_by, expires_at, created_at, version) VALUES ($1::uuid,$2,$3,$4::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			q.RequestID, q.TenantID, q.SubjectKind, q.SubjectID, q.FromState, q.ToState, q.Status, att, ready, q.Reason, q.RequestedBy, q.ExpiresAt, q.CreatedAt, q.Version); err != nil {
			return err
		}
		return r.finish(ctx, tx, q.TenantID, []rollout.Event{ev}, out)
	})
	return rolloutErr(err)
}

// GetRequest implements rollout.Repo.
func (r *RolloutRepo) GetRequest(ctx context.Context, id string) (*rollout.Request, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, rollout.ErrNotFound
	}
	var q rollout.Request
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		err := scanRequest(tx.QueryRow(ctx, `SELECT `+reqCols+` FROM xic_rollout_requests WHERE request_id=$1::uuid`, id), &q)
		if errors.Is(err, pgx.ErrNoRows) {
			return rollout.ErrNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// ListRequests implements rollout.Repo.
func (r *RolloutRepo) ListRequests(ctx context.Context, f rollout.RequestFilter) ([]rollout.Request, error) {
	limit := f.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	var subj any
	if f.SubjectID != "" {
		if _, err := uuid.Parse(f.SubjectID); err != nil {
			return []rollout.Request{}, nil
		}
		subj = f.SubjectID
	}
	out := []rollout.Request{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+reqCols+` FROM xic_rollout_requests WHERE ($1 = '' OR subject_kind = $1) AND ($2::uuid IS NULL OR subject_id = $2::uuid)
			AND ($3 = '' OR status = $3) ORDER BY created_at DESC, request_id LIMIT $4`, f.SubjectKind, subj, f.Status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var q rollout.Request
			if err := scanRequest(rows, &q); err != nil {
				return err
			}
			out = append(out, q)
		}
		return rows.Err()
	})
	return out, err
}

// MutateRequest implements rollout.Repo.
func (r *RolloutRepo) MutateRequest(ctx context.Context, id string, fn func(tx *rollout.RequestTx) error) (*rollout.Request, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, rollout.ErrNotFound
	}
	var out rollout.Request
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		var cur rollout.Request
		if err := scanRequest(pg.QueryRow(ctx, `SELECT `+reqCols+` FROM xic_rollout_requests WHERE request_id=$1::uuid FOR UPDATE`, id), &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return rollout.ErrNotFound
			}
			return err
		}
		w := cur
		tx := &rollout.RequestTx{Q: &w}
		if err := fn(tx); err != nil {
			return err
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_rollout_requests SET status=$2, decided_by=$3, decision_note=$4, decided_at=$5, version=version+1 WHERE request_id=$1::uuid`,
			id, w.Status, w.DecidedBy, w.DecisionNote, w.DecidedAt); err != nil {
			return err
		}
		if err := r.finish(ctx, pg, cur.TenantID, tx.Event, tx.Out); err != nil {
			return err
		}
		return scanRequest(pg.QueryRow(ctx, `SELECT `+reqCols+` FROM xic_rollout_requests WHERE request_id=$1::uuid`, id), &out)
	})
	if err != nil {
		return nil, rolloutErr(err)
	}
	return &out, nil
}

// ── legacy connectors ──────────────────────────────────────────────────────────

const legacyCols = `legacy_id::text, tenant_id, name, owning_service, direction, description, state, COALESCE(connection_id::text, ''), state_since,
	COALESCE(advance_request::text, ''), created_by, version, created_at, updated_at`

func scanLegacy(row pgx.Row, l *rollout.Legacy) error {
	return row.Scan(&l.LegacyID, &l.TenantID, &l.Name, &l.OwningService, &l.Direction, &l.Description, &l.State, &l.ConnectionID, &l.StateSince,
		&l.AdvanceRequest, &l.CreatedBy, &l.Version, &l.CreatedAt, &l.UpdatedAt)
}

func loadLegacy(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*rollout.Legacy, error) {
	var l rollout.Legacy
	q := `SELECT ` + legacyCols + ` FROM xic_legacy_connectors WHERE legacy_id=$1::uuid`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	if err := scanLegacy(tx.QueryRow(ctx, q, id), &l); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, rollout.ErrNotFound
		}
		return nil, err
	}
	return &l, nil
}

// InsertLegacy implements rollout.Repo.
func (r *RolloutRepo) InsertLegacy(ctx context.Context, l *rollout.Legacy, ev rollout.Event, out []rollout.OutEvent) error {
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO xic_legacy_connectors (legacy_id, tenant_id, name, owning_service, direction, description, state, state_since, created_by, version, created_at, updated_at)
			VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, l.LegacyID, l.TenantID, l.Name, l.OwningService, l.Direction, l.Description, l.State, l.StateSince,
			l.CreatedBy, l.Version, l.CreatedAt, l.UpdatedAt); err != nil {
			return err
		}
		return r.finish(ctx, tx, l.TenantID, []rollout.Event{ev}, out)
	})
	return rolloutErr(err)
}

// GetLegacy implements rollout.Repo.
func (r *RolloutRepo) GetLegacy(ctx context.Context, id string) (*rollout.Legacy, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, rollout.ErrNotFound
	}
	var l *rollout.Legacy
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		l, err = loadLegacy(ctx, tx, id, false)
		return err
	})
	return l, err
}

// ListLegacy implements rollout.Repo.
func (r *RolloutRepo) ListLegacy(ctx context.Context, f rollout.LegacyFilter) ([]rollout.Legacy, error) {
	limit := f.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	out := []rollout.Legacy{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+legacyCols+` FROM xic_legacy_connectors WHERE ($1 = '' OR state = $1) ORDER BY created_at DESC, legacy_id LIMIT $2`, f.State, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var l rollout.Legacy
			if err := scanLegacy(rows, &l); err != nil {
				return err
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// MutateLegacy implements rollout.Repo.
func (r *RolloutRepo) MutateLegacy(ctx context.Context, id string, fn func(tx *rollout.LegacyTx) error) (*rollout.Legacy, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, rollout.ErrNotFound
	}
	var out *rollout.Legacy
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		cur, err := loadLegacy(ctx, pg, id, true)
		if err != nil {
			return err
		}
		w := *cur
		tx := &rollout.LegacyTx{L: &w}
		if err := fn(tx); err != nil {
			return err
		}
		if tx.Use != "" {
			tag, err := pg.Exec(ctx, `UPDATE xic_rollout_requests SET status='USED', version=version+1 WHERE request_id=$1::uuid AND subject_kind='LEGACY'
				AND subject_id=$2::uuid AND status='APPROVED'`, tx.Use, id)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return rollout.ErrState
			}
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_legacy_connectors SET state=$2, state_since=$3, connection_id=$4::uuid, advance_request=$5::uuid, version=version+1, updated_at=now()
			WHERE legacy_id=$1::uuid`, id, w.State, w.StateSince, nulStr(w.ConnectionID), nulStr(w.AdvanceRequest)); err != nil {
			return err
		}
		if err := r.finish(ctx, pg, cur.TenantID, tx.Event, tx.Out); err != nil {
			return err
		}
		out, err = loadLegacy(ctx, pg, id, false)
		return err
	})
	if err != nil {
		return nil, rolloutErr(err)
	}
	return out, nil
}

// RegisterExchanges implements rollout.Repo. The connector row is locked so registrations of one connector are
// serialized: the content-hash check and the insert cannot interleave with another call's.
func (r *RolloutRepo) RegisterExchanges(ctx context.Context, legacyID string, in []rollout.LegacyExchange, by string, _ time.Time) (int, int, error) {
	if _, err := uuid.Parse(legacyID); err != nil {
		return 0, 0, rollout.ErrNotFound
	}
	stored, dups := 0, 0
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		l, err := loadLegacy(ctx, tx, legacyID, true)
		if err != nil {
			return err
		}
		known := map[string]string{}
		for _, e := range in {
			h, ok := known[e.ExternalRef]
			if !ok {
				err := tx.QueryRow(ctx, `SELECT payload_hash FROM xic_legacy_exchanges WHERE legacy_id=$1::uuid AND external_ref=$2 LIMIT 1`, legacyID, e.ExternalRef).Scan(&h)
				switch {
				case err == nil:
				case errors.Is(err, pgx.ErrNoRows):
					h = e.PayloadHash
				default:
					return err
				}
				known[e.ExternalRef] = h
			}
			if h != e.PayloadHash {
				return rollout.ErrConflict
			}
			tag, err := tx.Exec(ctx, `INSERT INTO xic_legacy_exchanges (entry_id, tenant_id, legacy_id, external_ref, domain_request_ref, operation, outcome, payload_hash,
				occurred_at, registered_by, registered_at) VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6,$7,$8,$9,$10,clock_timestamp())
				ON CONFLICT (tenant_id, legacy_id, external_ref, outcome) DO NOTHING`,
				uuid.NewString(), l.TenantID, legacyID, e.ExternalRef, e.DomainRequestRef, e.Operation, e.Outcome, e.PayloadHash, e.OccurredAt, by)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				stored++
			} else {
				dups++
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, rolloutErr(err)
	}
	return stored, dups, nil
}

// LatestExchanges implements rollout.Repo.
func (r *RolloutRepo) LatestExchanges(ctx context.Context, legacyID string, from, to time.Time, limit int) ([]rollout.LegacyExchange, error) {
	if limit < 1 || limit > 10000 {
		limit = 5001
	}
	out := []rollout.LegacyExchange{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT entry_id::text, tenant_id, legacy_id::text, external_ref, domain_request_ref, operation, outcome, payload_hash, occurred_at, registered_by, registered_at
			FROM (SELECT DISTINCT ON (external_ref) * FROM xic_legacy_exchanges WHERE legacy_id=$1::uuid AND occurred_at >= $2 AND occurred_at < $3
			      ORDER BY external_ref, registered_at DESC, entry_id) x ORDER BY occurred_at, external_ref LIMIT $4`, legacyID, from, to, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e rollout.LegacyExchange
			if err := rows.Scan(&e.EntryID, &e.TenantID, &e.LegacyID, &e.ExternalRef, &e.DomainRequestRef, &e.Operation, &e.Outcome, &e.PayloadHash, &e.OccurredAt, &e.RegisteredBy, &e.RegisteredAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// InsertParity implements rollout.Repo.
func (r *RolloutRepo) InsertParity(ctx context.Context, p *rollout.ParityRun, ev rollout.Event, out []rollout.OutEvent) error {
	samples := []byte(p.Samples)
	if len(samples) == 0 {
		samples = []byte(`[]`)
	}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO xic_legacy_parity_runs (parity_id, tenant_id, legacy_id, window_from, window_to, compared, matched, missing_in_xic, outcome_differs,
			xic_only, samples, passed, run_by, created_at) VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, p.ParityID, p.TenantID, p.LegacyID, p.WindowFrom,
			p.WindowTo, p.Compared, p.Matched, p.MissingInXIC, p.OutcomeDiffers, p.XICOnly, samples, p.Passed, p.RunBy, p.CreatedAt); err != nil {
			return err
		}
		return r.finish(ctx, tx, p.TenantID, []rollout.Event{ev}, out)
	})
	return rolloutErr(err)
}

// ListParity implements rollout.Repo.
func (r *RolloutRepo) ListParity(ctx context.Context, legacyID string, limit int) ([]rollout.ParityRun, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	out := []rollout.ParityRun{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT parity_id::text, tenant_id, legacy_id::text, window_from, window_to, compared, matched, missing_in_xic, outcome_differs, xic_only, samples, passed, run_by, created_at
			FROM xic_legacy_parity_runs WHERE legacy_id=$1::uuid ORDER BY created_at DESC, parity_id LIMIT $2`, legacyID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p rollout.ParityRun
			var s []byte
			if err := rows.Scan(&p.ParityID, &p.TenantID, &p.LegacyID, &p.WindowFrom, &p.WindowTo, &p.Compared, &p.Matched, &p.MissingInXIC, &p.OutcomeDiffers, &p.XICOnly, &s, &p.Passed, &p.RunBy, &p.CreatedAt); err != nil {
				return err
			}
			p.Samples = json.RawMessage(s)
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// Events implements rollout.Repo.
func (r *RolloutRepo) Events(ctx context.Context, kind, id string) ([]rollout.Event, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, rollout.ErrNotFound
	}
	out := []rollout.Event{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT subject_kind, subject_id::text, kind, actor_id, detail, created_at FROM xic_rollout_events
			WHERE subject_kind=$1 AND subject_id=$2::uuid ORDER BY created_at, event_id`, kind, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev rollout.Event
			var d []byte
			if err := rows.Scan(&ev.SubjectKind, &ev.SubjectID, &ev.Kind, &ev.Actor, &d, &ev.At); err != nil {
				return err
			}
			if len(d) > 0 {
				ev.Detail = json.RawMessage(d)
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

// RecordSignal implements rollout.Repo.
func (r *RolloutRepo) RecordSignal(ctx context.Context, tenant string, ev rollout.OutEvent) error {
	return r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return r.enqueue(ctx, tx, tenant, ev)
	})
}

// ── the platform's side of readiness and parity ────────────────────────────────

// Outcomes implements rollout.Facts: outbound attempts by settled/failed/unknown state, inbound exchanges by what the
// owning domain did with them.
func (r *RolloutRepo) Outcomes(ctx context.Context, connectionID string, from, to time.Time) (rollout.Outcomes, error) {
	var o rollout.Outcomes
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var os, of, ou, is, ifl int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='SETTLED'), count(*) FILTER (WHERE state='FAILED'), count(*) FILTER (WHERE state='UNKNOWN')
			FROM xic_outbound_attempts WHERE connection_id=$1::uuid AND created_at >= $2 AND created_at < $3`, connectionID, from, to).Scan(&os, &of, &ou); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE domain_outcome='ACCEPTED'), count(*) FILTER (WHERE domain_outcome='REJECTED')
			FROM xic_inbound_exchanges WHERE connection_id=$1::uuid AND received_at >= $2 AND received_at < $3`, connectionID, from, to).Scan(&is, &ifl); err != nil {
			return err
		}
		o = rollout.Outcomes{Success: os + is, Failure: of + ifl, Unknown: ou}
		return nil
	})
	return o, err
}

// OpenHighVariances implements rollout.Facts.
func (r *RolloutRepo) OpenHighVariances(ctx context.Context, connectionID string) (int, error) {
	n := 0
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM xic_reconciliation_variances WHERE connection_id=$1::uuid AND severity='HIGH' AND state IN ('OPEN', 'ESCALATED')`, connectionID).Scan(&n)
	})
	return n, err
}

// OverdueUnknown implements rollout.Facts.
func (r *RolloutRepo) OverdueUnknown(ctx context.Context, connectionID string, cutoff time.Time) (int, error) {
	n := 0
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM xic_outbound_attempts WHERE connection_id=$1::uuid AND state='UNKNOWN' AND created_at < $2`, connectionID, cutoff).Scan(&n)
	})
	return n, err
}

// Counterparts implements rollout.Facts.
func (r *RolloutRepo) Counterparts(ctx context.Context, direction, connectionID string, refs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(refs) == 0 {
		return out, nil
	}
	q := `SELECT domain_request_ref, state FROM xic_outbound_attempts WHERE connection_id=$1::uuid AND domain_request_ref = ANY($2) ORDER BY created_at`
	if direction == rollout.DirBatch {
		q = `SELECT external_batch_id, state FROM xic_batches WHERE connection_id=$1::uuid AND external_batch_id = ANY($2) ORDER BY received_at`
	}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, connectionID, refs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref, st string
			if err := rows.Scan(&ref, &st); err != nil {
				return err
			}
			out[ref] = st
		}
		return rows.Err()
	})
	return out, err
}

// InboundCount implements rollout.Facts.
func (r *RolloutRepo) InboundCount(ctx context.Context, connectionID string, from, to time.Time) (int, error) {
	n := 0
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM xic_inbound_exchanges WHERE connection_id=$1::uuid AND state <> 'DUPLICATE' AND received_at >= $2 AND received_at < $3`,
			connectionID, from, to).Scan(&n)
	})
	return n, err
}

// XICOnly implements rollout.Facts.
func (r *RolloutRepo) XICOnly(ctx context.Context, direction, connectionID string, from, to time.Time, refs []string) (int, error) {
	if refs == nil {
		refs = []string{} // cardinality(NULL) would match nothing
	}
	q := `SELECT count(*) FROM xic_outbound_attempts WHERE connection_id=$1::uuid AND created_at >= $2 AND created_at < $3 AND domain_request_ref <> ALL($4)`
	if direction == rollout.DirBatch {
		q = `SELECT count(*) FROM xic_batches WHERE connection_id=$1::uuid AND received_at >= $2 AND received_at < $3 AND external_batch_id <> ALL($4)`
	}
	n := 0
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, q, connectionID, from, to, refs).Scan(&n)
	})
	return n, err
}
