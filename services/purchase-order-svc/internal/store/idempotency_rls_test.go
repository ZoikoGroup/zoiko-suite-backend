package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/store"
)

// 000009_idempotency_keys_rls, under the ordinary app role: the store's keys
// are confined to the caller's tenant by the policy itself, an unscoped session
// sees none, a cross-tenant insert is refused, and the retention purge (which
// has no tenant) still reaches every tenant through app.idempotency_purge.
func TestIdempotencyKeysRLS_TenantConfined_PurgeAcrossTenants(t *testing.T) {
	owner := openTestPool(t)
	app := appOnlyPool(t)
	s := store.New(app, zap.NewNop())
	ctx := context.Background()

	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	for _, tenant := range []string{tenantA, tenantB} {
		if _, created, err := s.BeginIdempotent(tctx(tenant), "approve:x", "k1", "h1"); err != nil || !created {
			t.Fatalf("claim in %s: created=%v err=%v", tenant, created, err)
		}
		if err := s.CompleteIdempotent(tctx(tenant), "approve:x", "k1", 200, []byte(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
	}
	// The store's own round trip still works under FORCE RLS.
	rec, created, err := s.BeginIdempotent(tctx(tenantA), "approve:x", "k1", "h1")
	if err != nil || created || rec == nil || !rec.Completed || rec.StatusCode != 200 {
		t.Fatalf("replay under RLS: rec=%+v created=%v err=%v", rec, created, err)
	}

	// Unscoped: nothing visible.
	var n int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("no tenant set: RLS must hide every idempotency key, saw %d", n)
	}

	// Scoped to A: only A's key, and B's key cannot be written from A.
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("scoped to tenant A: want exactly A's key, saw %d", n)
	}
	_, err = tx.Exec(ctx, `INSERT INTO idempotency_keys (tenant_key, scope, idem_key, request_hash) VALUES ($1, 's', 'k', 'h')`, tenantB)
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("writing another tenant's key must be refused by RLS, got %v", err)
	}
	_ = tx.Rollback(ctx)

	// The purge flag reads/deletes across tenants but cannot insert.
	tx2, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx) //nolint:errcheck
	if _, err := tx2.Exec(ctx, `SELECT set_config('app.idempotency_purge', 'true', true)`); err != nil {
		t.Fatal(err)
	}
	_, err = tx2.Exec(ctx, `INSERT INTO idempotency_keys (tenant_key, scope, idem_key, request_hash) VALUES ($1, 's', 'k', 'h')`, tenantB)
	if err == nil || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("the purge capability must not admit writes, got %v", err)
	}
	_ = tx2.Rollback(ctx)

	if _, err := owner.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '10 days'`); err != nil {
		t.Fatal(err)
	}
	purged, err := s.PurgeIdempotency(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if purged != 2 {
		t.Fatalf("purge across tenants removed %d keys, want 2", purged)
	}
}
