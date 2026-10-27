package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/domain"
	"zoiko.io/connectivity-api-bridge-svc/internal/middleware"
	"zoiko.io/connectivity-api-bridge-svc/internal/recon"
)

// ReconRepo is the Postgres implementation of recon.Repo (migration 010). Its Facts methods read the real exchange,
// attempt, receipt and batch tables; nothing here writes to any of them.
type ReconRepo struct{ p *PgStore }

// NewReconRepo wraps a PgStore.
func NewReconRepo(p *PgStore) *ReconRepo { return &ReconRepo{p: p} }

var _ recon.Repo = (*ReconRepo)(nil)

func (r *ReconRepo) enqueue(ctx context.Context, tx pgx.Tx, tenant string, e recon.OutEvent) error {
	return r.p.enqueue(ctx, tx, tenant, domain.OutboxEvent{Type: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		ActorID: e.ActorID, CorrelationID: e.CorrelationID, Data: e.Data})
}

func (r *ReconRepo) workerTx(ctx context.Context, fn func(pgx.Tx) error) error {
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

func reconErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch pgCode(err) {
	case "23000":
		if strings.Contains(msg, "cannot go") || strings.Contains(msg, "is final") || strings.Contains(msg, "open variances") || strings.Contains(msg, "supplied once") {
			return recon.ErrState
		}
	case "23514":
		switch {
		case strings.Contains(msg, "xic_rr_distinct_suppliers"):
			return recon.ErrSelfAction
		case strings.Contains(msg, "xic_rr_independent"):
			return recon.ErrNotIndependent
		}
	}
	return err
}

const runCols = `run_id::text, tenant_id, connection_id::text, connector_id, family, origin, period_from, period_to, basis, state, external_source, external, external_supplied_by,
	awaiting_domain, domain_totals, domain_totals_by, dedupe_key, result, variance_count, open_variances, high_variances, evaluated_at, next_evaluate_at, lease_until,
	closed_by, close_note, requested_by, correlation_id, version, created_at, updated_at`

func scanRun(row pgx.Row, r *recon.Run) error {
	var ext, dom, res []byte
	if err := row.Scan(&r.RunID, &r.TenantID, &r.ConnectionID, &r.ConnectorID, &r.Family, &r.Origin, &r.PeriodFrom, &r.PeriodTo, &r.Basis, &r.State, &r.ExternalSource,
		&ext, &r.ExternalSuppliedBy, &r.AwaitingDomain, &dom, &r.DomainTotalsBy, &r.DedupeKey, &res, &r.VarianceCount, &r.OpenVariances, &r.HighVariances, &r.EvaluatedAt,
		&r.NextEvaluateAt, &r.LeaseUntil, &r.ClosedBy, &r.CloseNote, &r.RequestedBy, &r.CorrelationID, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return err
	}
	if len(ext) > 0 && string(ext) != "null" {
		r.External = json.RawMessage(ext)
	}
	if len(dom) > 0 && string(dom) != "null" {
		r.DomainTotals = json.RawMessage(dom)
	}
	if len(res) > 0 && string(res) != "null" {
		r.Result = json.RawMessage(res)
	}
	return nil
}

func loadRun(ctx context.Context, tx pgx.Tx, id string, forUpdate bool) (*recon.Run, error) {
	var r recon.Run
	q := `SELECT ` + runCols + ` FROM xic_reconciliation_runs WHERE run_id=$1::uuid`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	if err := scanRun(tx.QueryRow(ctx, q, id), &r); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, recon.ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

func insertReconEvent(ctx context.Context, tx pgx.Tx, tenant, runID string, ev recon.Event) error {
	var d any
	if len(ev.Detail) > 0 {
		d = []byte(ev.Detail)
	}
	at := ev.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `INSERT INTO xic_reconciliation_events (event_id, tenant_id, run_id, variance_id, kind, actor_id, detail, created_at)
		VALUES ($1::uuid, $2, $3::uuid, $4::uuid, $5, $6, $7, $8)`, uuid.NewString(), tenant, runID, nulStr(ev.VarianceID), ev.Kind, ev.Actor, d, at)
	return err
}

func jsonOrNil(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// InsertRun implements recon.Repo.
func (r *ReconRepo) InsertRun(ctx context.Context, run *recon.Run, ev recon.Event, out []recon.OutEvent) (*recon.Run, bool, error) {
	var held *recon.Run
	var existing bool
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO xic_reconciliation_runs (run_id, tenant_id, connection_id, connector_id, family, origin, period_from, period_to, basis, state,
			external_source, external, external_supplied_by, awaiting_domain, dedupe_key, next_evaluate_at, requested_by, correlation_id, version, created_at, updated_at)
			VALUES ($1::uuid,$2,$3::uuid,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) ON CONFLICT (tenant_id, dedupe_key) DO NOTHING`,
			run.RunID, run.TenantID, run.ConnectionID, run.ConnectorID, run.Family, run.Origin, run.PeriodFrom, run.PeriodTo, run.Basis, run.State, run.ExternalSource,
			jsonOrNil(run.External), run.ExternalSuppliedBy, run.AwaitingDomain, run.DedupeKey, run.NextEvaluateAt, run.RequestedBy, run.CorrelationID, run.Version, run.CreatedAt, run.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var h recon.Run
			if err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM xic_reconciliation_runs WHERE tenant_id=$1 AND dedupe_key=$2`, run.TenantID, run.DedupeKey), &h); err != nil {
				return err
			}
			held, existing = &h, true
			return nil
		}
		if err := insertReconEvent(ctx, tx, run.TenantID, run.RunID, ev); err != nil {
			return err
		}
		for _, o := range out {
			if err := r.enqueue(ctx, tx, run.TenantID, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, reconErr(err)
	}
	return held, existing, nil
}

// GetRun implements recon.Repo.
func (r *ReconRepo) GetRun(ctx context.Context, id string) (*recon.Run, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, recon.ErrNotFound
	}
	var out *recon.Run
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = loadRun(ctx, tx, id, false)
		return err
	})
	return out, err
}

// ListRuns implements recon.Repo.
func (r *ReconRepo) ListRuns(ctx context.Context, f recon.RunFilter) ([]recon.Run, error) {
	limit := f.Limit
	if limit < 1 || limit > 500 {
		limit = 100
	}
	var conn any
	if f.ConnectionID != "" {
		if _, err := uuid.Parse(f.ConnectionID); err != nil {
			return []recon.Run{}, nil
		}
		conn = f.ConnectionID
	}
	out := []recon.Run{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+runCols+` FROM xic_reconciliation_runs WHERE ($1 = '' OR state = $1) AND ($2::uuid IS NULL OR connection_id = $2::uuid)
			ORDER BY created_at DESC, run_id LIMIT $3`, f.State, conn, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x recon.Run
			if err := scanRun(rows, &x); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

// RunEvents implements recon.Repo.
func (r *ReconRepo) RunEvents(ctx context.Context, id string) ([]recon.Event, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, recon.ErrNotFound
	}
	out := []recon.Event{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM xic_reconciliation_runs WHERE run_id=$1::uuid`, id).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return recon.ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT kind, COALESCE(variance_id::text, ''), actor_id, detail, created_at FROM xic_reconciliation_events WHERE run_id=$1::uuid ORDER BY created_at, event_id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev recon.Event
			var d []byte
			if err := rows.Scan(&ev.Kind, &ev.VarianceID, &ev.Actor, &d, &ev.At); err != nil {
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

const varCols = `variance_id::text, tenant_id, run_id::text, last_run_id::text, connection_id::text, variance_type, severity, state, fingerprint, refs, repeat_count, case_ref,
	resolution, resolution_note, resolved_by, resolved_at, escalated_at, version, created_at, updated_at`

func scanVariance(row pgx.Row, v *recon.Variance) error {
	var refs []byte
	if err := row.Scan(&v.VarianceID, &v.TenantID, &v.RunID, &v.LastRunID, &v.ConnectionID, &v.Type, &v.Severity, &v.State, &v.Fingerprint, &refs, &v.RepeatCount,
		&v.CaseRef, &v.Resolution, &v.ResolutionNote, &v.ResolvedBy, &v.ResolvedAt, &v.EscalatedAt, &v.Version, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return err
	}
	v.Refs = json.RawMessage(refs)
	return nil
}

// MutateRun implements recon.Repo.
func (r *ReconRepo) MutateRun(ctx context.Context, id string, fn func(tx *recon.RunTx) error) (*recon.Run, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, recon.ErrNotFound
	}
	var out *recon.Run
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		cur, err := loadRun(ctx, pg, id, true)
		if err != nil {
			return err
		}
		w := *cur
		tx := &recon.RunTx{Run: &w}
		if err := fn(tx); err != nil {
			return err
		}
		if len(tx.Event) == 0 && len(tx.Out) == 0 && len(tx.Variances) == 0 && reflect.DeepEqual(*cur, w) {
			out = cur
			return nil
		}
		for _, v := range tx.Variances {
			var held recon.Variance
			err := scanVariance(pg.QueryRow(ctx, `SELECT `+varCols+` FROM xic_reconciliation_variances WHERE tenant_id=$1 AND connection_id=$2::uuid AND fingerprint=$3
				AND state IN ('OPEN', 'ESCALATED') FOR UPDATE`, cur.TenantID, v.ConnectionID, v.Fingerprint), &held)
			switch {
			case err == nil:
				if _, err := pg.Exec(ctx, `UPDATE xic_reconciliation_variances SET repeat_count = repeat_count + 1, last_run_id=$2::uuid, version=version+1, updated_at=now() WHERE variance_id=$1::uuid`,
					held.VarianceID, id); err != nil {
					return err
				}
				held.RepeatCount++
				held.LastRunID = id
				if tx.VarianceEvent != nil {
					if err := r.enqueue(ctx, pg, cur.TenantID, tx.VarianceEvent(held, true)); err != nil {
						return err
					}
				}
			case errors.Is(err, pgx.ErrNoRows):
				if _, err := pg.Exec(ctx, `INSERT INTO xic_reconciliation_variances (variance_id, tenant_id, run_id, last_run_id, connection_id, variance_type, severity, state, fingerprint,
					refs, repeat_count, escalated_at, version, created_at, updated_at) VALUES ($1::uuid,$2,$3::uuid,$3::uuid,$4::uuid,$5,$6,$7,$8,$9,1,$10,1,$11,$11)`,
					v.VarianceID, cur.TenantID, id, v.ConnectionID, v.Type, v.Severity, v.State, v.Fingerprint, []byte(v.Refs), v.EscalatedAt, v.CreatedAt); err != nil {
					return err
				}
				if tx.VarianceEvent != nil {
					if err := r.enqueue(ctx, pg, cur.TenantID, tx.VarianceEvent(v, false)); err != nil {
						return err
					}
				}
			default:
				return err
			}
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_reconciliation_runs SET state=$2, awaiting_domain=$3, domain_totals=$4, domain_totals_by=$5, result=$6, variance_count=$7,
			open_variances=$8, high_variances=$9, evaluated_at=$10, next_evaluate_at=$11, lease_until=$12, closed_by=$13, close_note=$14, version=version+1, updated_at=now()
			WHERE run_id=$1::uuid`, id, w.State, w.AwaitingDomain, jsonOrNil(w.DomainTotals), w.DomainTotalsBy, jsonOrNil(w.Result), w.VarianceCount, w.OpenVariances,
			w.HighVariances, w.EvaluatedAt, w.NextEvaluateAt, w.LeaseUntil, w.ClosedBy, w.CloseNote); err != nil {
			return err
		}
		for _, ev := range tx.Event {
			if err := insertReconEvent(ctx, pg, cur.TenantID, id, ev); err != nil {
				return err
			}
		}
		for _, o := range tx.Out {
			if err := r.enqueue(ctx, pg, cur.TenantID, o); err != nil {
				return err
			}
		}
		out, err = loadRun(ctx, pg, id, false)
		return err
	})
	if err != nil {
		return nil, reconErr(err)
	}
	return out, nil
}

// GetVariance implements recon.Repo.
func (r *ReconRepo) GetVariance(ctx context.Context, id string) (*recon.Variance, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, recon.ErrNotFound
	}
	var v recon.Variance
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		if err := scanVariance(tx.QueryRow(ctx, `SELECT `+varCols+` FROM xic_reconciliation_variances WHERE variance_id=$1::uuid`, id), &v); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return recon.ErrNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListVariances implements recon.Repo.
func (r *ReconRepo) ListVariances(ctx context.Context, f recon.VarianceFilter) ([]recon.Variance, error) {
	limit := f.Limit
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	var run, conn any
	for _, p := range []struct {
		in  string
		out *any
	}{{f.RunID, &run}, {f.ConnectionID, &conn}} {
		if p.in != "" {
			if _, err := uuid.Parse(p.in); err != nil {
				return []recon.Variance{}, nil
			}
			*p.out = p.in
		}
	}
	out := []recon.Variance{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+varCols+` FROM xic_reconciliation_variances WHERE ($1 = '' OR state = $1) AND ($2::uuid IS NULL OR run_id = $2::uuid OR last_run_id = $2::uuid)
			AND ($3::uuid IS NULL OR connection_id = $3::uuid) AND ($4 = '' OR severity = $4) ORDER BY created_at, variance_id LIMIT $5`, f.State, run, conn, f.Severity, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v recon.Variance
			if err := scanVariance(rows, &v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, err
}

// MutateVariance implements recon.Repo. When the change settles the last open variance of a run, the run closes in
// the same transaction.
func (r *ReconRepo) MutateVariance(ctx context.Context, id string, fn func(tx *recon.VarTx) error) (*recon.Variance, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, recon.ErrNotFound
	}
	var out *recon.Variance
	err := r.p.withTenant(ctx, func(pg pgx.Tx) error {
		var cur recon.Variance
		if err := scanVariance(pg.QueryRow(ctx, `SELECT `+varCols+` FROM xic_reconciliation_variances WHERE variance_id=$1::uuid FOR UPDATE`, id), &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return recon.ErrNotFound
			}
			return err
		}
		run, err := loadRun(ctx, pg, cur.RunID, false)
		if err != nil {
			return err
		}
		w := cur
		tx := &recon.VarTx{V: &w, Run: run}
		if err := fn(tx); err != nil {
			return err
		}
		if _, err := pg.Exec(ctx, `UPDATE xic_reconciliation_variances SET state=$2, case_ref=$3, resolution=$4, resolution_note=$5, resolved_by=$6, resolved_at=$7, escalated_at=$8,
			version=version+1, updated_at=now() WHERE variance_id=$1::uuid`, id, w.State, w.CaseRef, w.Resolution, w.ResolutionNote, w.ResolvedBy, w.ResolvedAt, w.EscalatedAt); err != nil {
			return err
		}
		for _, ev := range tx.Event {
			if err := insertReconEvent(ctx, pg, cur.TenantID, cur.RunID, ev); err != nil {
				return err
			}
		}
		for _, o := range tx.Out {
			if err := r.enqueue(ctx, pg, cur.TenantID, o); err != nil {
				return err
			}
		}
		if w.Final() {
			for _, rid := range distinct(cur.RunID, cur.LastRunID) {
				var open int
				if err := pg.QueryRow(ctx, `SELECT count(*) FROM xic_reconciliation_variances WHERE (run_id=$1::uuid OR last_run_id=$1::uuid) AND state IN ('OPEN', 'ESCALATED')`, rid).Scan(&open); err != nil {
					return err
				}
				rr, err := loadRun(ctx, pg, rid, true)
				if err != nil {
					return err
				}
				closing := open == 0 && (rr.State == recon.RunVariance || rr.State == recon.RunEscalated)
				state, closedBy, note := rr.State, rr.ClosedBy, rr.CloseNote
				if closing {
					state, closedBy, note = recon.RunClosed, w.ResolvedBy, "all variances settled"
				}
				if _, err := pg.Exec(ctx, `UPDATE xic_reconciliation_runs SET open_variances=$2, state=$3, closed_by=$4, close_note=$5, version=version+1, updated_at=now() WHERE run_id=$1::uuid`,
					rid, open, state, closedBy, note); err != nil {
					return err
				}
				if closing {
					rr.State, rr.ClosedBy = state, closedBy
					if err := insertReconEvent(ctx, pg, cur.TenantID, rid, recon.Event{Kind: "CLOSED", Actor: closedBy, At: time.Now().UTC()}); err != nil {
						return err
					}
					if tx.RunClosed != nil {
						if err := r.enqueue(ctx, pg, cur.TenantID, tx.RunClosed(rr)); err != nil {
							return err
						}
					}
				}
			}
		}
		return scanVariance(pg.QueryRow(ctx, `SELECT `+varCols+` FROM xic_reconciliation_variances WHERE variance_id=$1::uuid`, id), &cur)
	})
	if err != nil {
		return nil, reconErr(err)
	}
	var v recon.Variance
	if gerr := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return scanVariance(tx.QueryRow(ctx, `SELECT `+varCols+` FROM xic_reconciliation_variances WHERE variance_id=$1::uuid`, id), &v)
	}); gerr != nil {
		return nil, gerr
	}
	out = &v
	return out, nil
}

func distinct(a, b string) []string {
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}

// DueRuns implements recon.Repo.
func (r *ReconRepo) DueRuns(ctx context.Context, now time.Time, limit int) ([]recon.DueRef, error) {
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	var out []recon.DueRef
	err := r.workerTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, run_id::text FROM xic_reconciliation_runs WHERE state = 'OPEN' AND NOT awaiting_domain
			AND (next_evaluate_at IS NULL OR next_evaluate_at <= $1) AND (lease_until IS NULL OR lease_until <= $1) ORDER BY created_at, run_id LIMIT $2`, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d recon.DueRef
			if err := rows.Scan(&d.TenantID, &d.ID); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// LastScheduledEnd implements recon.Repo.
func (r *ReconRepo) LastScheduledEnd(ctx context.Context, tenant, connectionID string) (time.Time, error) {
	var last *time.Time
	err := r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT max(period_to) FROM xic_reconciliation_runs WHERE connection_id=$1::uuid AND origin='SCHEDULED'`, connectionID).Scan(&last)
	})
	if err != nil || last == nil {
		return time.Time{}, err
	}
	return *last, nil
}

// RecordSignal implements recon.Repo.
func (r *ReconRepo) RecordSignal(ctx context.Context, tenant string, ev recon.OutEvent) error {
	return r.p.withTenant(middleware.WithTenant(ctx, tenant), func(tx pgx.Tx) error {
		return r.enqueue(ctx, tx, tenant, ev)
	})
}

// ── the platform's side of a comparison ─────────────────────────────────────

// InboundCounts implements recon.Facts.
func (r *ReconRepo) InboundCounts(ctx context.Context, connectionID string, from, to time.Time) (map[string]int, error) {
	out := map[string]int{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var recv, dup, acc, rej, quar int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state <> 'DUPLICATE'), count(*) FILTER (WHERE state = 'DUPLICATE'),
			count(*) FILTER (WHERE domain_outcome = 'ACCEPTED'), count(*) FILTER (WHERE state = 'REJECTED' OR domain_outcome = 'REJECTED'), count(*) FILTER (WHERE state = 'QUARANTINED')
			FROM xic_inbound_exchanges WHERE connection_id=$1::uuid AND received_at >= $2 AND received_at < $3`, connectionID, from, to).Scan(&recv, &dup, &acc, &rej, &quar); err != nil {
			return err
		}
		out["received"], out["duplicate"], out["accepted"], out["rejected"], out["quarantined"] = recv, dup, acc, rej, quar
		return nil
	})
	return out, err
}

// OutboundCounts implements recon.Facts.
func (r *ReconRepo) OutboundCounts(ctx context.Context, connectionID string, from, to time.Time) (map[string]int, error) {
	out := map[string]int{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var sub, set, fail, unk, pend, canc int
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE submit_count >= 1), count(*) FILTER (WHERE state = 'SETTLED'), count(*) FILTER (WHERE state = 'FAILED'),
			count(*) FILTER (WHERE state = 'UNKNOWN'), count(*) FILTER (WHERE state = 'PENDING'), count(*) FILTER (WHERE state = 'CANCELLED')
			FROM xic_outbound_attempts WHERE connection_id=$1::uuid AND created_at >= $2 AND created_at < $3`, connectionID, from, to).Scan(&sub, &set, &fail, &unk, &pend, &canc); err != nil {
			return err
		}
		out["submitted"], out["settled"], out["failed"], out["unknown"], out["pending"], out["cancelled"] = sub, set, fail, unk, pend, canc
		return nil
	})
	return out, err
}

// References implements recon.Facts.
func (r *ReconRepo) References(ctx context.Context, connectionID string, from, to time.Time, limit int) ([]string, error) {
	out := []string{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT ref FROM (
			SELECT external_batch_id AS ref FROM xic_batches WHERE connection_id=$1::uuid AND received_at >= $2 AND received_at < $3 AND state NOT IN ('REJECTED', 'QUARANTINED')
			UNION
			SELECT provider_reference FROM xic_outbound_attempts WHERE connection_id=$1::uuid AND created_at >= $2 AND created_at < $3 AND provider_reference <> ''
		) x ORDER BY ref LIMIT $4`, connectionID, from, to, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// Attempts implements recon.Facts.
func (r *ReconRepo) Attempts(ctx context.Context, connectionID string, from, to time.Time, limit int) ([]recon.AttemptFact, error) {
	out := []recon.AttemptFact{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT a.attempt_id::text, a.state, a.provider_reference, a.resolution_method, a.created_at, a.updated_at, a.escalated_at,
			COALESCE((SELECT c.target_state FROM xic_callback_receipts c WHERE c.attempt_id = a.attempt_id AND c.target_state <> '' ORDER BY c.received_at DESC, c.receipt_id DESC LIMIT 1), ''),
			EXISTS (SELECT 1 FROM xic_callback_receipts c WHERE c.attempt_id = a.attempt_id AND c.needs_reconciliation)
			FROM xic_outbound_attempts a WHERE a.connection_id=$1::uuid AND a.created_at >= $2 AND a.created_at < $3 ORDER BY a.created_at, a.attempt_id LIMIT $4`, connectionID, from, to, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a recon.AttemptFact
			if err := rows.Scan(&a.AttemptID, &a.State, &a.ProviderReference, &a.ResolutionMethod, &a.CreatedAt, &a.UpdatedAt, &a.EscalatedAt, &a.LatestReceiptState, &a.NeedsReconcile); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// Batches implements recon.Facts.
func (r *ReconRepo) Batches(ctx context.Context, connectionID string, from, to time.Time, limit int) ([]recon.BatchFact, error) {
	out := []recon.BatchFact{}
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT batch_id::text, external_batch_id, state, declared_rows, admitted_rows, accepted_rows, rejected_rows, quarantined_rows, domain_rejected_rows,
			pending_rows, variance_rows, processing_done, received_at FROM xic_batches WHERE connection_id=$1::uuid AND received_at >= $2 AND received_at < $3
			AND state NOT IN ('REJECTED', 'QUARANTINED') ORDER BY received_at, batch_id LIMIT $4`, connectionID, from, to, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b recon.BatchFact
			if err := rows.Scan(&b.BatchID, &b.ExternalBatchID, &b.State, &b.DeclaredRows, &b.AdmittedRows, &b.AcceptedRows, &b.RejectedRows, &b.QuarantinedRows, &b.DomainRejected,
				&b.PendingRows, &b.VarianceRows, &b.ProcessingDone, &b.ReceivedAt); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// LateInbound implements recon.Facts.
func (r *ReconRepo) LateInbound(ctx context.Context, connectionID string, from, to, cutoff time.Time) (int, error) {
	var n int
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM xic_inbound_exchanges WHERE connection_id=$1::uuid AND received_at > $2 AND received_at >= $3 AND received_at < $4 AND state <> 'DUPLICATE'`,
			connectionID, cutoff, from, to).Scan(&n)
	})
	return n, err
}
