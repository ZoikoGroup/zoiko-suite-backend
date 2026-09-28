package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// FindTenantsWithExpiredActionTokens discovers tenant IDs having unexpired ACTIVE tokens that passed expires_at.
// It executes under app.platform_scope (SELECT-only) as a cross-tenant discovery hatch.
func (s *PgStore) FindTenantsWithExpiredActionTokens(ctx context.Context, now time.Time, limit int) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}

	const query = `
		SELECT DISTINCT tenant_id
		FROM action_tokens
		WHERE status = 'ACTIVE' AND expires_at < $1
		LIMIT $2;
	`
	rows, err := tx.Query(ctx, query, now, limit)
	if err != nil {
		return nil, fmt.Errorf("query expired action token tenants: %w", err)
	}
	defer rows.Close()

	var tenants []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, err
		}
		tenants = append(tenants, tid)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit platform scope tx: %w", err)
	}

	return tenants, nil
}

// ExpireActionTokensForTenant marks all past-due ACTIVE tokens for a specific tenant as EXPIRED under tenant RLS.
func (s *PgStore) ExpireActionTokensForTenant(ctx context.Context, tenantID string, now time.Time) (int64, error) {
	if strings.TrimSpace(tenantID) == "" {
		return 0, fmt.Errorf("missing tenant_id")
	}

	var count int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			UPDATE action_tokens
			SET status = 'EXPIRED'
			WHERE tenant_id = $1 AND status = 'ACTIVE' AND expires_at < $2;
		`
		tag, err := tx.Exec(ctx, sql, tenantID, now)
		if err != nil {
			return fmt.Errorf("update expired action tokens: %w", err)
		}
		count = tag.RowsAffected()
		return nil
	})

	if err != nil {
		return 0, err
	}
	return count, nil
}

// FindTenantsWithPurgeableActionTokens discovers tenants having terminal tokens (EXPIRED, CONSUMED, REVOKED) older than cutoff.
// It executes under app.platform_scope (SELECT-only) as a cross-tenant discovery hatch.
func (s *PgStore) FindTenantsWithPurgeableActionTokens(ctx context.Context, olderThan time.Time, limit int) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}

	const query = `
		SELECT DISTINCT tenant_id
		FROM action_tokens
		WHERE status IN ('EXPIRED', 'CONSUMED', 'REVOKED') AND created_at < $1
		LIMIT $2;
	`
	rows, err := tx.Query(ctx, query, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("query purgeable action token tenants: %w", err)
	}
	defer rows.Close()

	var tenants []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, err
		}
		tenants = append(tenants, tid)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit platform scope tx: %w", err)
	}

	return tenants, nil
}

// PurgeActionTokensForTenant permanently deletes terminal (CONSUMED, EXPIRED, REVOKED) action tokens older than cutoff under tenant RLS.
func (s *PgStore) PurgeActionTokensForTenant(ctx context.Context, tenantID string, olderThan time.Time) (int64, error) {
	if strings.TrimSpace(tenantID) == "" {
		return 0, fmt.Errorf("missing tenant_id")
	}

	var count int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			DELETE FROM action_tokens
			WHERE tenant_id = $1
			  AND status IN ('EXPIRED', 'CONSUMED', 'REVOKED')
			  AND created_at < $2;
		`
		tag, err := tx.Exec(ctx, sql, tenantID, olderThan)
		if err != nil {
			return fmt.Errorf("purge terminal action tokens: %w", err)
		}
		count = tag.RowsAffected()
		return nil
	})

	if err != nil {
		return 0, err
	}
	return count, nil
}

// FindTenantsWithStaleIntents discovers tenants having intents stuck in PENDING or RENDERING older than staleThreshold.
// It executes under app.platform_scope (SELECT-only) as a cross-tenant discovery hatch.
func (s *PgStore) FindTenantsWithStaleIntents(ctx context.Context, staleCutoff time.Time, limit int) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}

	const query = `
		SELECT DISTINCT tenant_id
		FROM message_intents
		WHERE status IN ('PENDING', 'RENDERING') AND created_at < $1
		LIMIT $2;
	`
	rows, err := tx.Query(ctx, query, staleCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("query stale intent tenants: %w", err)
	}
	defer rows.Close()

	var tenants []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, err
		}
		tenants = append(tenants, tid)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit platform scope tx: %w", err)
	}

	return tenants, nil
}

// ExpireStaleIntentsForTenant transitions stuck intents to DROPPED under tenant RLS.
func (s *PgStore) ExpireStaleIntentsForTenant(ctx context.Context, tenantID string, staleCutoff time.Time) (int64, error) {
	if strings.TrimSpace(tenantID) == "" {
		return 0, fmt.Errorf("missing tenant_id")
	}

	var count int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			UPDATE message_intents
			SET status = 'FAILED',
			    failure_reason = 'stale intent expired by housekeeping worker',
			    updated_at = now()
			WHERE tenant_id = $1
			  AND status IN ('PENDING', 'RENDERING')
			  AND created_at < $2;
		`
		tag, err := tx.Exec(ctx, sql, tenantID, staleCutoff)
		if err != nil {
			return fmt.Errorf("expire stale intents: %w", err)
		}
		count = tag.RowsAffected()
		return nil
	})

	if err != nil {
		return 0, err
	}
	return count, nil
}

// FindTenantsWithCompletedIntents finds tenants with concluded intents older than completedCutoff for retention archiving.
func (s *PgStore) FindTenantsWithCompletedIntents(ctx context.Context, completedCutoff time.Time, limit int) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.platform_scope', 'true', true)"); err != nil {
		return nil, fmt.Errorf("set platform scope: %w", err)
	}

	const query = `
		SELECT DISTINCT tenant_id
		FROM message_intents
		WHERE status IN ('DISPATCHED', 'DROPPED', 'SUPPRESSED', 'FAILED') AND created_at < $1
		LIMIT $2;
	`
	rows, err := tx.Query(ctx, query, completedCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("query completed intent tenants: %w", err)
	}
	defer rows.Close()

	var tenants []string
	for rows.Next() {
		var tid string
		if err := rows.Scan(&tid); err != nil {
			return nil, err
		}
		tenants = append(tenants, tid)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit platform scope tx: %w", err)
	}

	return tenants, nil
}

// PurgeCompletedLedgerRecordsForTenant deletes concluded intents older than cutoff under tenant RLS.
// Foreign key cascades automatically remove associated message_renders, delivery_attempts, and delivery_events.
func (s *PgStore) PurgeCompletedLedgerRecordsForTenant(ctx context.Context, tenantID string, olderThan time.Time) (int64, error) {
	if strings.TrimSpace(tenantID) == "" {
		return 0, fmt.Errorf("missing tenant_id")
	}

	var count int64
	err := s.withRLS(ctx, tenantID, func(tx pgx.Tx) error {
		const sql = `
			DELETE FROM message_intents
			WHERE tenant_id = $1
			  AND status IN ('DISPATCHED', 'DROPPED', 'SUPPRESSED', 'FAILED')
			  AND created_at < $2;
		`
		tag, err := tx.Exec(ctx, sql, tenantID, olderThan)
		if err != nil {
			return fmt.Errorf("purge completed message_intents: %w", err)
		}
		count = tag.RowsAffected()
		return nil
	})

	if err != nil {
		return 0, err
	}
	return count, nil
}
