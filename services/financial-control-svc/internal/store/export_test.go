//go:build integration

package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"zoiko.io/financial-control-svc/internal/domain"
)

// Test-only hooks for store_test.

// CanonicalType exposes the §26 → ZS-EVENT-001 §5.1 type mapping.
var CanonicalType = canonicalType

// WithRLSTx runs fn in the same tenant-scoped transaction every write uses.
func (s *PgStore) WithRLSTx(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	return s.withRLS(ctx, tenantID, fn)
}

// EmitRunEvent enqueues a run event on tx through the same helper every write uses.
func (s *PgStore) EmitRunEvent(ctx context.Context, tx pgx.Tx, r domain.ControlRun, eventType, actor, correlationID string,
	payload map[string]any) error {
	return s.emit(ctx, tx, r, eventType, actor, correlationID, payload)
}
