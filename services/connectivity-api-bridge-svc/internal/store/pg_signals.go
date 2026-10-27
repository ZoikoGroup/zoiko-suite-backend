package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/connectivity-api-bridge-svc/internal/signals"
)

// SignalsRepo reads the operational signals (ZS-SVC-T-001 §17.1) from the real tables. A tenant snapshot is scoped by
// row-level security; the platform snapshot reads across tenants through the worker role and returns counts only.
type SignalsRepo struct{ p *PgStore }

// NewSignalsRepo wraps a PgStore.
func NewSignalsRepo(p *PgStore) *SignalsRepo { return &SignalsRepo{p: p} }

var _ signals.Source = (*SignalsRepo)(nil)

// Snapshot implements signals.Source for the tenant in ctx.
func (r *SignalsRepo) Snapshot(ctx context.Context, since time.Time) (*signals.Snapshot, error) {
	var s *signals.Snapshot
	err := r.p.withTenant(ctx, func(tx pgx.Tx) error {
		var err error
		s, err = readSnapshot(ctx, tx, since, true)
		return err
	})
	return s, err
}

// Platform implements signals.Source.
func (r *SignalsRepo) Platform(ctx context.Context, since time.Time) (*signals.Snapshot, error) {
	var s *signals.Snapshot
	tx, err := r.p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true), set_config('app.xic_worker', 'true', true)`); err != nil {
		return nil, err
	}
	s, err = readSnapshot(ctx, tx, since, false)
	if err != nil {
		return nil, err
	}
	return s, tx.Commit(ctx)
}

func counts(ctx context.Context, tx pgx.Tx, q string, args ...any) (map[string]int, error) {
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

func readSnapshot(ctx context.Context, tx pgx.Tx, since time.Time, withSecurity bool) (*signals.Snapshot, error) {
	now := time.Now().UTC()
	s := &signals.Snapshot{GeneratedAt: now, WindowSeconds: int(now.Sub(since).Seconds())}
	var err error
	sum := func(m map[string]int) int {
		n := 0
		for _, v := range m {
			n += v
		}
		return n
	}

	if s.Connections.ByStatus, err = counts(ctx, tx, `SELECT status, count(*)::int FROM xic_connections GROUP BY status`); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(DISTINCT connection_id)::int FROM xic_credential_bindings WHERE status <> 'REVOKED' AND expires_at IS NOT NULL AND expires_at <= now() + interval '14 days'`).Scan(&s.Connections.ExpiringCredentials); err != nil {
		return nil, err
	}

	if s.Inbound.ByState, err = counts(ctx, tx, `SELECT state, count(*)::int FROM xic_inbound_exchanges WHERE received_at >= $1 GROUP BY state`, since); err != nil {
		return nil, err
	}
	s.Inbound.Total = sum(s.Inbound.ByState)
	if s.Inbound.QuarantineByClass, err = counts(ctx, tx, `SELECT quarantine_class, count(*)::int FROM xic_inbound_exchanges WHERE received_at >= $1 AND state = 'QUARANTINED' AND quarantine_class IS NOT NULL GROUP BY 1`, since); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM updated_at - received_at)), 0),
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM updated_at - received_at)), 0)
		FROM xic_inbound_exchanges WHERE received_at >= $1 AND state = 'HANDED_OFF'`, since).Scan(&s.Inbound.HandoffP50Seconds, &s.Inbound.HandoffP95Seconds); err != nil {
		return nil, err
	}

	if s.Outbound.ByState, err = counts(ctx, tx, `SELECT state, count(*)::int FROM xic_outbound_attempts WHERE created_at >= $1 GROUP BY state`, since); err != nil {
		return nil, err
	}
	s.Outbound.Total = sum(s.Outbound.ByState)
	if s.Outbound.ResolutionMethods, err = counts(ctx, tx, `SELECT resolution_method, count(*)::int FROM xic_outbound_attempts WHERE created_at >= $1 AND resolution_method <> '' GROUP BY 1`, since); err != nil {
		return nil, err
	}
	// ambiguity is not bounded by the window: an attempt that is still unresolved is a signal however old it is
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(extract(epoch FROM now() - created_at))::int, 0), count(*) FILTER (WHERE escalated_at IS NOT NULL)::int
		FROM xic_outbound_attempts WHERE state IN ('UNKNOWN', 'PENDING')`).Scan(&s.Outbound.OldestAmbiguousSeconds, &s.Outbound.Escalated); err != nil {
		return nil, err
	}

	if s.Callbacks.ByState, err = counts(ctx, tx, `SELECT state, count(*)::int FROM xic_callback_receipts WHERE received_at >= $1 GROUP BY state`, since); err != nil {
		return nil, err
	}
	if s.Callbacks.InvalidByReason, err = counts(ctx, tx, `SELECT reason_code, count(*)::int FROM xic_callback_receipts WHERE received_at >= $1 AND state = 'REJECTED' AND reason_code <> '' GROUP BY 1`, since); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*)::int FROM xic_callback_receipts WHERE needs_reconciliation`).Scan(&s.Callbacks.NeedsReconciliation); err != nil {
		return nil, err
	}

	if s.Batch.ByState, err = counts(ctx, tx, `SELECT state, count(*)::int FROM xic_batches WHERE received_at >= $1 GROUP BY state`, since); err != nil {
		return nil, err
	}
	if s.Batch.RowsByState, err = counts(ctx, tx, `SELECT r.state, count(*)::int FROM xic_batch_rows r JOIN xic_batches b ON b.batch_id = r.batch_id WHERE b.received_at >= $1 GROUP BY 1`, since); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(variance_rows), 0)::int FROM xic_batches WHERE received_at >= $1`, since).Scan(&s.Batch.ManifestVariances); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(extract(epoch FROM now() - received_at))::int, 0) FROM xic_batches WHERE NOT processing_done AND state IN ('RECEIVED', 'VALIDATING', 'ACCEPTED', 'PROCESSING')`).Scan(&s.Batch.OldestUnprocessedSeconds); err != nil {
		return nil, err
	}

	if s.Reconciliation.RunsByState, err = counts(ctx, tx, `SELECT state, count(*)::int FROM xic_reconciliation_runs WHERE created_at >= $1 GROUP BY state`, since); err != nil {
		return nil, err
	}
	var matched, evaluated int
	if err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE variance_count = 0)::int, count(*)::int FROM xic_reconciliation_runs WHERE created_at >= $1 AND evaluated_at IS NOT NULL`, since).Scan(&matched, &evaluated); err != nil {
		return nil, err
	}
	if evaluated > 0 {
		s.Reconciliation.MatchRate = float64(matched) / float64(evaluated)
	}
	if err = tx.QueryRow(ctx, `SELECT count(*)::int, COALESCE(max(extract(epoch FROM now() - created_at))::int, 0), count(*) FILTER (WHERE repeat_count > 1)::int
		FROM xic_reconciliation_variances WHERE state IN ('OPEN', 'ESCALATED')`).Scan(&s.Reconciliation.OpenVariances, &s.Reconciliation.OldestOpenSeconds, &s.Reconciliation.RepeatedVariances); err != nil {
		return nil, err
	}
	if s.Reconciliation.OpenBySeverity, err = counts(ctx, tx, `SELECT severity, count(*)::int FROM xic_reconciliation_variances WHERE state IN ('OPEN', 'ESCALATED') GROUP BY 1`); err != nil {
		return nil, err
	}
	if s.Reconciliation.OpenByType, err = counts(ctx, tx, `SELECT variance_type, count(*)::int FROM xic_reconciliation_variances WHERE state IN ('OPEN', 'ESCALATED') GROUP BY 1`); err != nil {
		return nil, err
	}
	if err = tx.QueryRow(ctx, `SELECT count(*)::int FROM xic_reconciliation_runs WHERE created_at >= $1 AND origin = 'SCHEDULED'`, since).Scan(&s.Reconciliation.ScheduledRunsInWindow); err != nil {
		return nil, err
	}

	s.Security.Events = map[string]int{}
	if withSecurity {
		if s.Security.Events, err = counts(ctx, tx, `SELECT event_type, count(*)::int FROM eventing_outbox WHERE created_at >= $1 AND event_type LIKE 'com.zoikosuite.integration.security.%' GROUP BY 1`, since); err != nil {
			return nil, err
		}
	}
	return s, nil
}
