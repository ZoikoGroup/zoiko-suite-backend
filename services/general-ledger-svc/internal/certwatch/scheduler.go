package certwatch

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	svcmiddleware "zoiko.io/general-ledger-svc/internal/middleware"
)

// Scope is one (tenant, legal entity) that has certifications on the watch list.
type Scope struct {
	TenantID      string
	LegalEntityID string
}

// ScopeLister lists every scope with anything to sweep, across tenants (a capability-scoped read, migration 000037).
type ScopeLister interface {
	ListScopes(ctx context.Context) ([]Scope, error)
}

// Locker lets one replica at a time run the sweep. TryLock returns ok=false when another replica holds it, and a release function
// otherwise; a lock that cannot be taken is not an error.
type Locker interface {
	TryLock(ctx context.Context) (release func(), ok bool, err error)
}

// Authorizer is authorization-svc's decision for the principal on the legal entity.
type Authorizer interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, action string) error
}

// SweepAction is the permission a scheduled sweep is authorized under, for each entity it sweeps: the same one an operator needs to
// sweep by hand. No service self-authorizes, so the job's principal must be granted it like anyone else, and a scope it is not
// granted is skipped and reported.
const SweepAction = "GL_CERTIFICATION_WATCH_SWEEP"

// RunSummary is a scheduled run's account of itself.
type RunSummary struct {
	Scopes    int           `json:"scopes"`
	Swept     int           `json:"swept"`
	Skipped   int           `json:"skipped"` // not authorized, or the sweep itself failed
	Checked   int           `json:"checked"`
	Withdrawn int           `json:"withdrawn"`
	NewFlags  int           `json:"new_flags"`
	Failed    int           `json:"failed"` // certifications that could not be checked
	Locked    bool          `json:"locked"` // another replica held the lock: nothing was done
	Elapsed   time.Duration `json:"elapsed"`
}

// Scheduler runs the certification sweep on a timer, for every (tenant, entity) with something on the watch list, so a revocation
// is found without anyone remembering to ask. It is the sweep of sweeper.go, unchanged: what is new is who asks and when.
//
//   - It acts as ONE configured principal, authorized per legal entity like any caller; a scope it may not sweep is skipped, loudly.
//   - Replicas do not stampede: a lock lets one run at a time, and the sweep is idempotent anyway.
//   - One scope's failure (an unreachable service, a denied entity, a store error) never stops the others.
//   - A tick that finds nothing to do is cheap: it asks no issuing service about a withdrawn certification, only re-scans it.
//   - It is observable (Observer): a schedule that stops, is refused, or keeps failing shows up as a metric, not only a log line.
type Scheduler struct {
	Sweeper    *Sweeper
	Scopes     ScopeLister
	Authz      Authorizer
	Lock       Locker
	Principal  string
	Interval   time.Duration
	ScopeLimit time.Duration // bound on one scope's sweep; zero means no bound
	Log        *zap.Logger
	Observer   Observer
}

func (s *Scheduler) log() *zap.Logger {
	if s.Log != nil {
		return s.Log
	}
	return zap.NewNop()
}

// Run sweeps once at start and then every Interval, until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	if s.Interval <= 0 || s.Principal == "" {
		// Off is reported as off (interval 0), so an alert on a stalled schedule does not fire for a schedule that was never meant to run.
		orNoop(s.Observer).SweepScheduled(0)
		s.log().Info("certification sweep scheduler not started", zap.Duration("interval", s.Interval), zap.Bool("principal_configured", s.Principal != ""))
		return
	}
	orNoop(s.Observer).SweepScheduled(s.Interval)
	s.log().Info("certification sweep scheduler started", zap.Duration("interval", s.Interval), zap.String("principal", s.Principal))
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.log().Error("certification sweep run failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			s.log().Info("certification sweep scheduler stopped")
			return
		case <-t.C:
		}
	}
}

// RunOnce is one scheduled pass.
func (s *Scheduler) RunOnce(ctx context.Context) (*RunSummary, error) {
	obs := orNoop(s.Observer)
	started := time.Now()
	if s.Principal == "" {
		return nil, fmt.Errorf("%w: no principal is configured to run the sweep", ErrInvalid)
	}
	release, ok, err := s.Lock.TryLock(ctx)
	if err != nil {
		obs.SweepRun(RunError, time.Since(started))
		return nil, fmt.Errorf("take the sweep lock: %w", err)
	}
	sum := &RunSummary{}
	if !ok {
		sum.Locked = true
		obs.SweepRun(RunLocked, 0)
		return sum, nil
	}
	defer release()

	scopes, err := s.Scopes.ListScopes(ctx)
	if err != nil {
		obs.SweepRun(RunError, time.Since(started))
		return nil, fmt.Errorf("list the watched scopes: %w", err)
	}
	sum.Scopes = len(scopes)
	for _, sc := range scopes {
		if ctx.Err() != nil {
			break
		}
		s.sweepScope(ctx, sc, sum)
	}
	sum.Elapsed = time.Since(started)

	outcome := RunOK
	if sum.Skipped > 0 || sum.Failed > 0 {
		outcome = RunPartial
	}
	// A pass cut short by cancellation is neither a success nor a failure of the schedule: it is not reported as either.
	if ctx.Err() == nil {
		obs.SweepRun(outcome, sum.Elapsed)
		obs.CertificationsUnchecked(sum.Failed)
		obs.FlagsRaised(TriggerSchedule, sum.NewFlags)
	}
	log := s.log().With(zap.Int("scopes", sum.Scopes), zap.Int("swept", sum.Swept), zap.Int("skipped", sum.Skipped), zap.Int("checked", sum.Checked),
		zap.Int("withdrawn", sum.Withdrawn), zap.Int("new_flags", sum.NewFlags), zap.Int("failed", sum.Failed), zap.Duration("elapsed", sum.Elapsed))
	if sum.NewFlags > 0 {
		log.Warn("certification sweep raised flags: journals or postings rest on a revoked certification")
	} else if sum.Skipped > 0 || sum.Failed > 0 {
		log.Warn("certification sweep finished with scopes skipped or certifications unchecked")
	} else {
		log.Info("certification sweep finished")
	}
	return sum, nil
}

func (s *Scheduler) sweepScope(ctx context.Context, sc Scope, sum *RunSummary) {
	obs := orNoop(s.Observer)
	// The tenant travels on the context, as it does on a request, so authorization-svc is asked about the right tenant.
	ctx = svcmiddleware.WithTenant(ctx, sc.TenantID)
	if err := s.Authz.CheckAllowed(ctx, s.Principal, sc.LegalEntityID, SweepAction); err != nil {
		sum.Skipped++
		obs.SweepScope(ScopeUnauthorized)
		s.log().Warn("certification sweep skipped a scope: the principal is not authorized to sweep it", zap.String("tenant_id", sc.TenantID),
			zap.String("legal_entity_id", sc.LegalEntityID), zap.String("principal", s.Principal), zap.Error(err))
		return
	}
	if s.ScopeLimit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.ScopeLimit)
		defer cancel()
	}
	res, err := s.Sweeper.Sweep(ctx, sc.TenantID, s.Principal, "scheduled-certification-sweep", sc.LegalEntityID)
	if err != nil {
		sum.Skipped++
		obs.SweepScope(ScopeError)
		s.log().Error("certification sweep failed for a scope", zap.String("tenant_id", sc.TenantID), zap.String("legal_entity_id", sc.LegalEntityID), zap.Error(err))
		return
	}
	sum.Swept++
	obs.SweepScope(ScopeSwept)
	sum.Checked += res.Checked
	sum.Withdrawn += res.Withdrawn
	sum.NewFlags += res.NewFlags
	sum.Failed += res.Failed
	for _, o := range res.Outcomes {
		if o.Error != "" {
			s.log().Warn("a certification could not be checked", zap.String("tenant_id", sc.TenantID), zap.String("legal_entity_id", sc.LegalEntityID),
				zap.String("certification", o.Certification), zap.String("error", o.Error))
		}
	}
}
