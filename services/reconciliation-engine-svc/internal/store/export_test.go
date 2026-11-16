package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// Test-only hooks for store_test: the rollback test needs to enqueue inside a
// tenant transaction it then fails on purpose, which no exported write can do.

// WithTenantTx runs fn in the same tenant-scoped transaction every write uses.
func (s *PgStore) WithTenantTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return s.withTenant(ctx, tenantID, fn)
}

// EnqueueRunStarted enqueues a reconciliation-run.started event on tx through
// the same helper every write uses.
func (s *PgStore) EnqueueRunStarted(ctx context.Context, tx pgx.Tx, tenantID, legalEntityID, runID, actor string,
	claim domain.IdempotencyClaim, data any) error {
	return s.enqueueEvent(ctx, tx, evRunStarted, tenantID, legalEntityID, runID, actor, claim, data)
}
