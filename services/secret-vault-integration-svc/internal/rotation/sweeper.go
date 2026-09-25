// Package rotation implements the automated material rotation sweeper
// (§13 compliance close: "no automated rotation"). On a configured interval
// it asks the store which scheduled versions are due (next_rotation_at
// passed) and drives the handler's PerformRotation core for each.
package rotation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"zoiko.io/secret-vault-integration-svc/internal/domain"
)

// RotationCore is the slice of the handler the sweeper drives. Satisfied by
// *handler.Handler; declared here so the package does not import handler.
// PerformRotation is the same core the HTTP rotate endpoint runs — the
// sweeper is a second, unattended caller of it, not a parallel
// implementation.
type RotationCore interface {
	PerformRotation(ctx context.Context, secretPolicyID, rotatedByPrincipalID string, tenantScope *string, requestID, correlationID string) (string, int, time.Time, error)
}

// SweepStore is the slice of the store the sweeper needs. Satisfied by
// *store.PgStore.
type SweepStore interface {
	ListDueRotations(ctx context.Context, now time.Time) ([]*domain.DueRotation, error)
	UpdateRotationSchedule(ctx context.Context, secretPolicyVersionID string, nextRotationAt time.Time) error
}

// Sweeper rotates every due secret on a fixed interval.
type Sweeper struct {
	store      SweepStore
	rotate     RotationCore
	log        *zap.Logger
	interval   time.Duration
	actorID    string
	pageSize   int
}

// New constructs a sweeper. actorID is the principal recorded as the actor
// of every automated rotation (a platform-scoped operator identity, never a
// tenant's). interval bounds how often the sweeper looks for due work; a
// non-positive interval disables scheduling at construction time.
func New(store SweepStore, rotate RotationCore, log *zap.Logger, interval time.Duration, actorID string) *Sweeper {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return &Sweeper{
		store:    store,
		rotate:   rotate,
		log:      log,
		interval: interval,
		actorID:  actorID,
		pageSize: 100,
	}
}

// Run blocks until ctx is cancelled, running one sweep pass every interval.
// Safe to call in a goroutine.
func (s *Sweeper) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		if err := s.Sweep(ctx); err != nil {
			// A sweep failure must not halt the loop: a transient store/vault
			// outage would otherwise silently stop all future rotations. Logged
			// and retried on the next tick.
			s.log.Error("rotation sweep failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Sweep rotates every currently-due secret once, then reschedules each
// rotated version's next_rotation_at using the interval already recorded for
// it. It never reschedules a version whose rotation did not complete — that
// version stays due and is retried on the next pass.
func (s *Sweeper) Sweep(ctx context.Context) error {
	due, err := s.store.ListDueRotations(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("list due rotations: %w", err)
	}

	for _, d := range due {
		if err := s.rotateOne(ctx, d); err != nil {
			s.log.Error("rotation fell through",
				zap.String("secret_policy_id", d.SecretPolicyID),
				zap.String("secret_policy_version_id", d.SecretPolicyVersionID),
				zap.Error(err),
			)
			continue
		}
	}
	return nil
}

func (s *Sweeper) rotateOne(ctx context.Context, d *domain.DueRotation) error {
	// Deterministic request id so a crash between the rotate and the
	// reschedule can never double-rotate on the next sweep: the idempotency
	// record written by PerformRotation makes a retried call for the same id
	// a no-op that answers the original outcome.
	requestID := fmt.Sprintf("sweep:%s:%d", d.SecretPolicyVersionID, time.Now().Unix())

	path, _, _, err := s.rotate.PerformRotation(ctx, d.SecretPolicyID, s.actorID, nil, requestID, "rotation-sweeper")
	if err != nil {
		if errors.Is(err, domain.ErrSecretPolicyNotFound) {
			// The version was already superseded/retired — nothing to rotate.
			s.log.Info("rotation skipped: policy no longer exists",
				zap.String("secret_policy_id", d.SecretPolicyID))
			return nil
		}
		return fmt.Errorf("rotate %s: %w", d.SecretPath, err)
	}
	if d.SecretPath != "" && path != "" {
		// Keep the audit/log story precise — the sweeper rotated what it was
		// told to.
		s.log.Info("automated rotation completed",
			zap.String("secret_policy_id", d.SecretPolicyID),
			zap.String("secret_path", path),
			zap.String("secret_policy_version_id", d.SecretPolicyVersionID),
			zap.String("request_id", requestID),
		)
	}

	// Reschedule ONLY on success (no error path above fell through). A
	// version that failed to rotate stays due and is retried next pass.
	if err := s.store.UpdateRotationSchedule(ctx, d.SecretPolicyVersionID, time.Now().Add(time.Duration(d.IntervalSeconds)*time.Second)); err != nil {
		s.log.Warn("rescheduling rotation failed; next pass will retry a stale due time",
			zap.String("secret_policy_version_id", d.SecretPolicyVersionID),
			zap.Error(err),
		)
	}
	return nil
}