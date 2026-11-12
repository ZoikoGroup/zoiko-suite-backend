package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Test-only hooks for store_test.

// CanonicalType exposes the event name → ZS-EVENT-001 §5.1 type mapping.
var CanonicalType = canonicalType

// WithScopeTx runs fn in the tenant-scoped transaction the tenant-plane
// writes use (app.tenant_id = organizationID).
func (s *PgStore) WithScopeTx(ctx context.Context, organizationID string, fn func(pgx.Tx) error) error {
	return s.withScope(ctx, organizationID, fn)
}

// WithSellerPlaneTx runs fn in the seller-plane transaction the catalogue,
// usage and billing writes use.
func (s *PgStore) WithSellerPlaneTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return s.withSellerPlane(ctx, fn)
}

// Emit enqueues one event on tx through the same helper every write uses.
func (s *PgStore) Emit(ctx context.Context, tx pgx.Tx, legacyType, aggregateType, aggregateID, tenantID, actor string, data any) error {
	return s.emit(ctx, tx, legacyType, aggregateType, aggregateID, tenantID, actor, data)
}
