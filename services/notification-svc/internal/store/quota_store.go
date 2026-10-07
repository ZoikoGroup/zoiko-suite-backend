package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/notification-svc/internal/domain"
	"zoiko.io/notification-svc/internal/quota"
)

// ZS-SVC-Y-001 NCD-03 6.5 persistence: send quota counters (migration 000029).

// WithQuota turns send quotas on, with the given limits. Without it nothing is counted.
func (s *PgStore) WithQuota(l quota.Limits) *PgStore {
	s.quotaLimits = &l
	s.nowFn = time.Now
	return s
}

// consumeQuotaTx draws one send from every budget that applies, inside the caller's
// transaction. If any budget would go over its limit it returns a *quota.ExceededError, and the
// caller's transaction (including the communication it just inserted) rolls back with it, so a
// refused send consumes nothing at all.
func (s *PgStore) consumeQuotaTx(ctx context.Context, tx pgx.Tx, n *domain.Notification) error {
	var intentID string
	if n.IntentVersionID != "" {
		// Best effort: a send under an intent is counted against the intent, and an intent version
		// that cannot be read leaves only the tenant and recipient budgets.
		_ = tx.QueryRow(ctx, `SELECT intent_id::text FROM communication_intent_versions WHERE version_id::text = $1 AND tenant_id = $2`,
			n.IntentVersionID, n.TenantID).Scan(&intentID)
	}
	class := n.CommunicationClass
	if class == "" {
		class = "T0"
	}
	now := s.nowFn().UTC()
	for _, b := range quota.Budgets(*s.quotaLimits, quota.Request{Class: class, Channel: n.Channel, RecipientID: n.RecipientPrincipalID, IntentID: intentID}) {
		start := quota.WindowStart(now, b.Window)
		var count int
		if err := tx.QueryRow(ctx, `
			INSERT INTO send_quota_counters (tenant_id, bucket, window_start, count) VALUES ($1, $2, $3, 1)
			ON CONFLICT (tenant_id, bucket, window_start) DO UPDATE SET count = send_quota_counters.count + 1
			RETURNING count`, n.TenantID, b.Bucket, start).Scan(&count); err != nil {
			return err
		}
		if count > b.Limit {
			return &quota.ExceededError{Dimension: b.Dimension, Limit: b.Limit, Window: b.Window, RetryAfter: quota.RetryAfter(now, b.Window)}
		}
		// A new tenant window opened: this is a convenient moment to drop counters nobody needs.
		if b.Dimension == quota.DimTenant && count == 1 {
			if _, err := tx.Exec(ctx, `DELETE FROM send_quota_counters WHERE tenant_id = $1 AND window_start < $2`,
				n.TenantID, now.Add(-24*time.Hour)); err != nil {
				return err
			}
		}
	}
	return nil
}

// WithQuotaClock replaces the clock the quota windows use. It exists so a test can move through
// windows; production leaves the real clock in place.
func (s *PgStore) WithQuotaClock(now func() time.Time) *PgStore {
	s.nowFn = now
	return s
}
