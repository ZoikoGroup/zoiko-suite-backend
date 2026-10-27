// Package expiry expires authorizations that outlived their expires_at.
//
// Until this existed, ExpirePaymentAuthorization only ran when somebody
// called it, so an approval nobody used stayed valid indefinitely. The
// sweeper finds overdue PENDING/APPROVED authorizations across tenants and
// expires each one through the normal store path, so the state change, the
// local evidence row and the PaymentAuthorizationExpired outbox event commit
// together. Approve and Consume also refuse an overdue authorization on their
// own, so the sweeper is housekeeping and event publication, not the control.
package expiry

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/middleware"
	"zoiko.io/payment-authorization-svc/internal/store"
)

// Actor recorded as having expired an authorization.
const Actor = "system:expiry-sweeper"

const idempotencyRetention = 7 * 24 * time.Hour

type Sweeper struct {
	store    store.Store
	interval time.Duration
	batch    int
	log      *zap.Logger
}

func New(st store.Store, interval time.Duration, batch int, log *zap.Logger) *Sweeper {
	if interval <= 0 {
		interval = time.Minute
	}
	if batch <= 0 {
		batch = 100
	}
	return &Sweeper{store: st, interval: interval, batch: batch, log: log}
}

// Start sweeps once immediately and then every interval until ctx ends.
func (s *Sweeper) Start(ctx context.Context) {
	s.RunOnce(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RunOnce(ctx)
		}
	}
}

// RunOnce expires up to one batch of overdue authorizations and returns how
// many it expired. An authorization that another caller expired, used or
// revoked in the meantime (ErrInvalidTransition) is simply skipped.
func (s *Sweeper) RunOnce(ctx context.Context) int {
	refs, err := s.store.ListExpiredCandidates(ctx, s.batch)
	if err != nil {
		s.log.Error("expiry sweep: failed to list overdue authorizations", zap.Error(err))
		return 0
	}
	expired := 0
	for _, ref := range refs {
		tctx := middleware.WithTenant(ctx, ref.TenantID)
		_, err := s.store.ExpireAuthorization(tctx, ref.AuthorizationID, Actor)
		switch {
		case err == nil:
			expired++
		case errors.Is(err, domain.ErrInvalidTransition):
		default:
			s.log.Error("expiry sweep: failed to expire an authorization", zap.String("authorization_id", ref.AuthorizationID), zap.Error(err))
		}
	}
	if expired > 0 {
		s.log.Info("expiry sweep: expired overdue authorizations", zap.Int("count", expired))
	}
	if _, err := s.store.PurgeIdempotency(ctx, idempotencyRetention); err != nil {
		s.log.Warn("expiry sweep: failed to purge old idempotency keys", zap.Error(err))
	}
	return expired
}
