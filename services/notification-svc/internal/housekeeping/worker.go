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
	FindTenantsWithCompletedIntents(ctx context.Context, completedCutoff time.Time, limit int) ([]string, error)
	PurgeCompletedLedgerRecordsForTenant(ctx context.Context, tenantID string, olderThan time.Time) (int64, error)
}

// Options configures the housekeeping worker.
type Options struct {
	Interval             time.Duration
	BatchSize            int
	TokenRetention       time.Duration // Age after which terminal tokens are purged (e.g. 30 days)
	LedgerRetention      time.Duration // Age after which completed ledger records are purged (e.g. 90 days)
	StaleIntentThreshold time.Duration // Age after which stuck PENDING/RENDERING intents are DROPPED (e.g. 24h)
}

// Stats returns the summary counts of records processed during a housekeeping pass.
type Stats struct {
	ExpiredTokens    int64
	PurgedTokens     int64
	StaleIntents     int64
	PurgedLedgerRows int64
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
	if opts.LedgerRetention <= 0 {
		opts.LedgerRetention = 90 * 24 * time.Hour
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
		zap.Duration("ledger_retention", w.opts.LedgerRetention),
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
			} else if stats.ExpiredTokens > 0 || stats.PurgedTokens > 0 || stats.StaleIntents > 0 || stats.PurgedLedgerRows > 0 {
				w.log.Info("housekeeping pass completed with mutations",
					zap.Int64("expired_tokens", stats.ExpiredTokens),
					zap.Int64("purged_tokens", stats.PurgedTokens),
					zap.Int64("stale_intents", stats.StaleIntents),
					zap.Int64("purged_ledger_rows", stats.PurgedLedgerRows),
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

	// 4. Purge completed delivery ledger records older than LedgerRetention
	if w.opts.LedgerRetention > 0 {
		ledgerPurgeCutoff := now.Add(-w.opts.LedgerRetention)
		completedTenants, err := w.store.FindTenantsWithCompletedIntents(ctx, ledgerPurgeCutoff, w.opts.BatchSize)
		if err != nil {
			w.log.Error("housekeeping: failed to discover tenants with completed intents for purge", zap.Error(err))
			return stats, err
		}
		for _, tenantID := range completedTenants {
			count, err := w.store.PurgeCompletedLedgerRecordsForTenant(ctx, tenantID, ledgerPurgeCutoff)
			if err != nil {
				w.log.Error("housekeeping: failed to purge completed ledger records for tenant",
					zap.String("tenant_id", tenantID),
					zap.Error(err),
				)
				continue
			}
			stats.PurgedLedgerRows += count
		}
	}

	return stats, nil
}
