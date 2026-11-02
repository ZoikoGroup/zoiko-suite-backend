package store_test

import (
	"context"
	"errors"
	"testing"

	"zoiko.io/banking-connector-svc/internal/idempotency"
)

// TestIdempotencyPgStore_ReplayAndTenantIsolation exercises the real
// idempotency_keys table as the NOSUPERUSER NOBYPASSRLS app role: a claim is
// replayed with its stored answer, a different body is a mismatch, and
// another tenant can neither see nor collide with the claim.
func TestIdempotencyPgStore_ReplayAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	admin := openAdminPool(t)
	_, _ = admin.Exec(ctx, "DELETE FROM idempotency_keys")
	s := idempotency.NewPgStore(appRolePool(t, admin))

	const ep = "POST /v1/x"
	rec, err := s.Claim(ctx, "tenant-a", ep, "k1", "fp1")
	if err != nil || rec != nil {
		t.Fatalf("first claim = %v, %v; want nil, nil", rec, err)
	}
	if err := s.Complete(ctx, "tenant-a", ep, "k1", 201, []byte(`{"id":"1"}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	rec, err = s.Claim(ctx, "tenant-a", ep, "k1", "fp1")
	if err != nil || rec == nil || rec.ResponseStatus != 201 || string(rec.ResponseBody) != `{"id":"1"}` {
		t.Fatalf("replay = %+v, %v; want stored 201 answer", rec, err)
	}
	if _, err := s.Claim(ctx, "tenant-a", ep, "k1", "other"); !errors.Is(err, idempotency.ErrFingerprintMismatch) {
		t.Fatalf("different body: err = %v, want ErrFingerprintMismatch", err)
	}
	// Same key, other tenant: an independent fresh claim, not tenant A's answer.
	rec, err = s.Claim(ctx, "tenant-b", ep, "k1", "fp1")
	if err != nil || rec != nil {
		t.Fatalf("tenant B claim = %+v, %v; want a fresh claim", rec, err)
	}
	var visible int
	conn, err := appRolePool(t, admin).Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT set_config('app.tenant_id', 'tenant-b', false)"); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM idempotency_keys WHERE tenant_id = 'tenant-a'").Scan(&visible); err != nil {
		t.Fatalf("count: %v", err)
	}
	if visible != 0 {
		t.Fatalf("RLS: tenant B saw %d of tenant A's idempotency rows", visible)
	}
}
