package store

import (
	"context"
	"fmt"

	"zoiko.io/authorization-svc/internal/domain"
)

// SweepExpiredAssignments announces, once each, up to limit assignments that
// were granting and whose effective window has closed, by emitting
// iam.assignment.expired in the same transaction that marks them announced
// (000038; ORG-06 AssignmentExpired). It returns how many it announced; a
// result equal to limit means there may be more.
//
// Cross-tenant by design, like the retention purges: it is a background loop
// that belongs to no tenant, so the work is done by a SECURITY DEFINER function
// that does nothing else. It uses the pool directly rather than withRLS for the
// same reason.
func (s *PgStore) SweepExpiredAssignments(ctx context.Context, limit int) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT authz_sweep_expired_assignments($1);`, limit).Scan(&n); err != nil {
		return 0, fmt.Errorf("%w: %v", domain.ErrStoreUnavailable, err)
	}
	return n, nil
}
