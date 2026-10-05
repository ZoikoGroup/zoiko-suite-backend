package housekeeping

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Store defines the database capabilities required by the housekeeping worker.
type Store interface {
	FindTenantsWithExpiredActionTokens(ctx context.Context, now time.Time, limit int) ([]string, error)
	ExpireActionTokensForTenant(ctx context.Context, tenantID string, now time.Time) (int64, error)
	FindTenantsWithPurgeableActionTokens(ctx context.Context, olderThan time.Time, limit int) ([]string, error)
	PurgeActionTokensForTenant(ctx context.Context, tenantID string, olderThan time.Time) (int64, error)
	FindTenantsWithStaleIntents(ctx context.Context, staleCutoff time.Time, limit int) ([]string, error)
	ExpireStaleIntentsForTenant(ctx context.Context, tenantID string, staleCutoff time.Time) (int64, error)
}

// Options configures the housekeeping worker.
type Options struct {
	Interval             time.Duration
	BatchSize            int
	TokenRetention       time.Duration // Age after which terminal tokens are purged (e.g. 30 days)
	StaleIntentThreshold time.Duration // Age after which stuck PENDING/RENDERING intents are DROPPED (e.g. 24h)
}

// Stats returns the summary counts of records processed during a housekeeping pass.
type Stats struct {
	ExpiredTokens int64
	PurgedTokens  int64
	StaleIntents  int64
}

// Worker periodically runs automated expiry and retention cleanup across all tenants.
type Worker struct {
	store Store
	opts  Options
	log   *zap.Logger
}

func NewWorker(store Store, opts Options, log *zap.Logger) *Worker {
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Minute
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 50
	}
	if opts.TokenRetention <= 0 {
		opts.TokenRetention = 30 * 24 * time.Hour
	}
	if opts.StaleIntentThreshold <= 0 {
		opts.StaleIntentThreshold = 24 * time.Hour
	}
	if log == nil {
		log = zap.NewNop()
	}

	return &Worker{
		store: store,
		opts:  opts,
		log:   log,
	}
}

// Start runs the periodic housekeeping worker until the context is cancelled.
func (w *Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.opts.Interval)
	defer ticker.Stop()

	w.log.Info("delivery ledger housekeeping worker started",
		zap.Duration("interval", w.opts.Interval),
		zap.Int("batch_size", w.opts.BatchSize),
		zap.Duration("token_retention", w.opts.TokenRetention),
		zap.Duration("stale_intent_threshold", w.opts.StaleIntentThreshold),
	)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("delivery ledger housekeeping worker stopped")
			return
		case <-ticker.C:
			stats, err := w.RunOnce(ctx)
			if err != nil {
				w.log.Error("housekeeping pass encountered error", zap.Error(err))
			} else if stats.ExpiredTokens > 0 || stats.PurgedTokens > 0 || stats.StaleIntents > 0 {
				w.log.Info("housekeeping pass completed with mutations",
					zap.Int64("expired_tokens", stats.ExpiredTokens),
					zap.Int64("purged_tokens", stats.PurgedTokens),
					zap.Int64("stale_intents", stats.StaleIntents),
				)
			}
		}
	}
}

// RunOnce performs a complete, idempotent housekeeping cycle and returns statistics.
func (w *Worker) RunOnce(ctx context.Context) (Stats, error) {
	var stats Stats
	now := time.Now().UTC()

	// 1. Transition past-due ACTIVE action tokens to EXPIRED
	tokenTenants, err := w.store.FindTenantsWithExpiredActionTokens(ctx, now, w.opts.BatchSize)
	if err != nil {
		w.log.Error("housekeeping: failed to discover tenants with expired action tokens", zap.Error(err))
		return stats, err
	}
	for _, tenantID := range tokenTenants {
		count, err := w.store.ExpireActionTokensForTenant(ctx, tenantID, now)
		if err != nil {
			w.log.Error("housekeeping: failed to expire action tokens for tenant",
				zap.String("tenant_id", tenantID),
				zap.Error(err),
			)
			continue
		}
		stats.ExpiredTokens += count
	}

	// 2. Purge terminal action tokens older than TokenRetention
	if w.opts.TokenRetention > 0 {
		tokenPurgeCutoff := now.Add(-w.opts.TokenRetention)
		purgeTenants, err := w.store.FindTenantsWithPurgeableActionTokens(ctx, tokenPurgeCutoff, w.opts.BatchSize)
		if err != nil {
			w.log.Error("housekeeping: failed to discover tenants with purgeable action tokens", zap.Error(err))
			return stats, err
		}
		for _, tenantID := range purgeTenants {
			count, err := w.store.PurgeActionTokensForTenant(ctx, tenantID, tokenPurgeCutoff)
			if err != nil {
				w.log.Error("housekeeping: failed to purge terminal action tokens for tenant",
					zap.String("tenant_id", tenantID),
					zap.Error(err),
				)
				continue
			}
			stats.PurgedTokens += count
		}
	}

	// 3. Expire stale message intents stuck in PENDING/RENDERING
	if w.opts.StaleIntentThreshold > 0 {
		staleCutoff := now.Add(-w.opts.StaleIntentThreshold)
		staleTenants, err := w.store.FindTenantsWithStaleIntents(ctx, staleCutoff, w.opts.BatchSize)
		if err != nil {
			w.log.Error("housekeeping: failed to discover tenants with stale intents", zap.Error(err))
			return stats, err
		}
		for _, tenantID := range staleTenants {
			count, err := w.store.ExpireStaleIntentsForTenant(ctx, tenantID, staleCutoff)
			if err != nil {
				w.log.Error("housekeeping: failed to expire stale intents for tenant",
					zap.String("tenant_id", tenantID),
					zap.Error(err),
				)
				continue
			}
			stats.StaleIntents += count
		}
	}

	// There is deliberately no step that deletes delivery-ledger records. One
	// used to purge concluded intents older than 90 days, and the foreign-key
	// cascade took their renders, attempts and delivery events with them: the
	// evidence of what was sent to whom, destroyed on a timer, with no legal
	// hold check. ZS-SVC-Y-001 keeps that evidence (§8.4, §9.2, INV-28) and
	// gives its retention to DRC; migration 000023 makes the tables refuse
	// DELETE so the purge cannot quietly come back.

	return stats, nil
}
