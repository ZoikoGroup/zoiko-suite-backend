package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"zoiko.io/authorization-svc/internal/telemetry"
)

// outboxStuckAttempts is the publish-attempt count past which an unpublished
// outbox row counts as stuck. The relay retries forever (no dead-letter), so
// this is a signal, not a state.
const outboxStuckAttempts = 5

// OperationalSnapshot reads the counts behind the §26 gauges: outbox health
// and privileged sessions by kind and state. It reads every tenant's rows
// under app.platform_scope — the session tables are tenant-isolated (RLS), so
// an unscoped read would count zero — and returns counts only, never rows.
func (s *PgStore) OperationalSnapshot(ctx context.Context) (telemetry.OperationalSnapshot, error) {
	snap := telemetry.OperationalSnapshot{
		Sessions:     map[string]map[string]int64{},
		ExpiringSoon: map[string]int64{},
	}
	err := s.withPlatformScope(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT count(*),
			       COALESCE(EXTRACT(EPOCH FROM NOW() - min(created_at)), 0)::float8,
			       count(*) FILTER (WHERE publish_attempts >= $1)
			  FROM outbox_events
			 WHERE published_at IS NULL`, outboxStuckAttempts).
			Scan(&snap.OutboxPending, &snap.OutboxOldestPendingSeconds, &snap.OutboxStuck); err != nil {
			return fmt.Errorf("outbox snapshot: %w", err)
		}
		for kind, table := range map[string]string{
			"jit":         "privileged_sessions",
			"break_glass": "break_glass_sessions",
			"support":     "support_sessions",
		} {
			var active, pending, expiring int64
			// An ACTIVE row past its expires_at grants nothing (every session
			// read checks the window), so it is not counted as active.
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FILTER (WHERE status = 'ACTIVE' AND expires_at > NOW()),
				       count(*) FILTER (WHERE status = 'PENDING_APPROVAL'),
				       count(*) FILTER (WHERE status = 'ACTIVE' AND expires_at > NOW()
				                          AND expires_at <= NOW() + INTERVAL '15 minutes')
				  FROM `+table+`
				 WHERE status IN ('ACTIVE', 'PENDING_APPROVAL')`).
				Scan(&active, &pending, &expiring); err != nil {
				return fmt.Errorf("%s snapshot: %w", table, err)
			}
			snap.Sessions[kind] = map[string]int64{"ACTIVE": active, "PENDING_APPROVAL": pending}
			snap.ExpiringSoon[kind] = expiring
		}
		return nil
	})
	return snap, err
}
