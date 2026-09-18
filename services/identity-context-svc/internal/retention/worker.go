// Package retention implements this service's GOV-09 obligations: it disposes
// of identity evidence whose retention period has elapsed, and refuses to
// dispose of anything a GOV-10 legal hold covers.
//
// THE DIVISION OF AUTHORITY MATTERS HERE. The spec's authority matrix says
// GOV-09 owns the "retention/disposition decision and certificate" but must
// never own the "legal-hold matter or privacy request authority", and
// invariant 7 says a hold "suspends disposition but never creates a new
// processing purpose".
//
// So this worker:
//
//   - decides WHEN evidence is due, from a configured period;
//   - ASKS the local legal-hold projection whether anything blocks it;
//   - disposes what is due and unheld, and issues a certificate;
//   - reports what was held back, separately and loudly.
//
// It cannot issue, release or narrow a hold. The projection it reads is
// populated exclusively by GOV-10's events — see the consumer.
package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/store"
	"zoiko.io/identity-context-svc/internal/telemetry"
)

// Store is the persistence contract the sweep needs.
type Store interface {
	TenantsWithDisposableRecords(ctx context.Context, due time.Time) ([]string, error)
	FindDisposableSessions(ctx context.Context, tenantID string, due time.Time, limit int) ([]store.DisposableSession, error)
	DisposeSessions(ctx context.Context, tenantID string, ids []string, at time.Time) (int, error)
	HasActiveLegalHold(ctx context.Context, tenantID, principalID string) (bool, error)
	PurgePublishedOutbox(ctx context.Context, before time.Time, limit int) (int, error)
}

// Config tunes the sweep.
type Config struct {
	// Interval is how often the sweep runs. Retention is measured in months;
	// there is nothing to gain from sweeping more than a few times a day, and
	// a tight interval on a large table is just load.
	Interval time.Duration
	// BatchSize bounds one tenant's pass.
	BatchSize int
	// OutboxRetention is how long delivered outbox rows are kept. They carry
	// no evidential weight of their own — the event is on the topic and the
	// fact it attests is in its own table — so this is short.
	OutboxRetention time.Duration
}

func DefaultConfig() Config {
	return Config{
		Interval:        6 * time.Hour,
		BatchSize:       500,
		OutboxRetention: 7 * 24 * time.Hour,
	}
}

// Worker runs the disposition sweep.
type Worker struct {
	store   Store
	events  *events.Publisher
	cfg     Config
	log     *zap.Logger
	metrics *telemetry.GovMetrics
}

// WithMetrics attaches the GOV-01 instruments.
//
// The disposed and held counters must be read together: a sweep that disposed
// nothing because everything was held by a legal hold looks identical to a
// quiet night if only one of them is plotted.
func (w *Worker) WithMetrics(m *telemetry.GovMetrics) *Worker {
	w.metrics = m
	return w
}

func New(s Store, publisher *events.Publisher, cfg Config, log *zap.Logger) *Worker {
	if cfg.Interval <= 0 {
		cfg = DefaultConfig()
	}
	return &Worker{store: s, events: publisher, cfg: cfg, log: log}
}

// Run sweeps until ctx is cancelled.
//
// The FIRST sweep is delayed by one interval rather than running at boot. A
// disposition sweep racing a rolling deploy — several replicas all starting at
// once, all sweeping immediately — is avoidable load on a table that is only
// ever a few hours from being swept anyway.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("retention worker started",
		zap.Duration("interval", w.cfg.Interval),
		zap.Duration("outbox_retention", w.cfg.OutboxRetention))

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("retention worker stopped")
			return
		case <-ticker.C:
			if err := w.SweepOnce(ctx); err != nil {
				w.log.Error("retention sweep failed", zap.Error(err))
			}
		}
	}
}

// SweepOnce runs one full pass. Exported so it can be driven deterministically
// in tests and from an operational runbook.
func (w *Worker) SweepOnce(ctx context.Context) error {
	now := time.Now().UTC()

	tenants, err := w.store.TenantsWithDisposableRecords(ctx, now)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	if len(tenants) == 0 {
		w.log.Debug("retention sweep: nothing due")
	}

	var firstErr error
	for _, tenantID := range tenants {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := w.sweepTenant(ctx, tenantID, now); err != nil {
			w.log.Error("retention sweep failed for tenant",
				zap.String("tenant_id", tenantID), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			// Keep going. One tenant's failure must not stop every other
			// tenant's retention obligation.
			continue
		}
	}

	// Outbox housekeeping. Separate from the evidence sweep because it is a
	// true delete rather than a redaction, and because it is not tenant-scoped.
	if w.cfg.OutboxRetention > 0 {
		purged, err := w.store.PurgePublishedOutbox(ctx, now.Add(-w.cfg.OutboxRetention), 1000)
		if err != nil {
			w.log.Error("outbox purge failed", zap.Error(err))
		} else if purged > 0 {
			w.log.Info("purged delivered outbox rows", zap.Int("rows", purged))
		}
	}

	return firstErr
}

// sweepTenant disposes one tenant's due evidence.
//
// The hold check runs PER PRINCIPAL rather than per row, and the result is
// memoised for the batch: a tenant-wide hold would otherwise cost one query
// per due row, and the answer cannot change mid-batch in any way that matters
// — a hold issued during the sweep applies to the next one.
func (w *Worker) sweepTenant(ctx context.Context, tenantID string, now time.Time) (*domain.DispositionOutcome, error) {
	due, err := w.store.FindDisposableSessions(ctx, tenantID, now, w.cfg.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("find disposable sessions: %w", err)
	}
	if len(due) == 0 {
		return nil, nil
	}

	held := make(map[string]bool, len(due))
	disposable := make([]string, 0, len(due))
	heldBack := 0

	for _, row := range due {
		blocked, seen := held[row.PrincipalID]
		if !seen {
			blocked, err = w.store.HasActiveLegalHold(ctx, tenantID, row.PrincipalID)
			if err != nil {
				// FAIL CLOSED — and "closed" here means DO NOT DELETE. A hold
				// check that did not run is not a check that passed, and the
				// cost of getting this wrong is destroying evidence during
				// litigation. Skipping the row costs a few hours until the
				// next sweep.
				w.log.Error("legal hold check failed — refusing to dispose",
					zap.String("tenant_id", tenantID),
					zap.String("principal_id", row.PrincipalID),
					zap.Error(err))
				blocked = true
			}
			held[row.PrincipalID] = blocked
		}
		if blocked {
			heldBack++
			continue
		}
		disposable = append(disposable, row.SessionContextID)
	}

	disposed := 0
	if len(disposable) > 0 {
		disposed, err = w.store.DisposeSessions(ctx, tenantID, disposable, now)
		if err != nil {
			return nil, fmt.Errorf("dispose sessions: %w", err)
		}
	}

	outcome := domain.DispositionOutcome{
		RecordClass:   domain.RetentionClassSessionEvidence,
		Considered:    len(due),
		Disposed:      disposed,
		HeldBack:      heldBack,
		CertificateID: "cert-" + ulid.Make().String(),
		SweptAt:       now,
	}

	if w.metrics != nil {
		w.metrics.DispositionDisposed.Add(float64(disposed))
		w.metrics.DispositionHeld.Add(float64(heldBack))
	}

	// The certificate is GOV-09's named required output, and it is emitted
	// whether or not anything was disposed. A sweep that disposed nothing
	// because everything was held is a materially different fact from a sweep
	// that found nothing due, and both need to be on the record.
	if err := w.events.PublishDispositionExecuted(ctx, tenantID, outcome, outcome.CertificateID); err != nil {
		w.log.Error("disposition certificate could not be enqueued",
			zap.String("tenant_id", tenantID),
			zap.String("certificate_id", outcome.CertificateID),
			zap.Error(err))
	}

	if heldBack > 0 {
		// Reported separately and at WARN. A sweep that quietly disposed 40 of
		// 100 rows and said nothing about the other 60 would look like a
		// success in every dashboard.
		w.log.Warn("disposition blocked by active legal holds",
			zap.String("tenant_id", tenantID),
			zap.Int("held_back", heldBack),
			zap.Int("disposed", disposed))
		if err := w.events.PublishDispositionBlocked(
			ctx, tenantID, domain.RetentionClassSessionEvidence, heldBack, outcome.CertificateID,
		); err != nil {
			w.log.Error("disposition-blocked event could not be enqueued",
				zap.String("tenant_id", tenantID), zap.Error(err))
		}
	}

	w.log.Info("disposition sweep complete",
		zap.String("tenant_id", tenantID),
		zap.Int("considered", outcome.Considered),
		zap.Int("disposed", outcome.Disposed),
		zap.Int("held_back", outcome.HeldBack),
		zap.String("certificate_id", outcome.CertificateID))

	return &outcome, nil
}
