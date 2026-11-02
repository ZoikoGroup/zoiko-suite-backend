package idempotency_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/carta-svc/internal/idempotency"
	"zoiko.io/carta-svc/internal/testdb"
)

// A duplicate request is answered from the stored first response even by a
// NEW store on a NEW pool (a restarted process), the same key with a different
// body is a mismatch, and another tenant's identical key is a separate claim.
// Runs as the ordinary app role, so the FORCEd RLS policy is in force.
func TestPgStore_ReplaySurvivesRestartAndIsTenantScoped(t *testing.T) {
	pools := testdb.Open(t)
	app := pools.RequireApp(t)
	ctx := context.Background()
	tenantA, tenantB := "t-"+uuid.NewString(), "t-"+uuid.NewString()
	const endpoint = "POST /v1/x"
	key := "k-" + uuid.NewString()

	first := idempotency.NewPgStore(app)
	if rec, err := first.Claim(ctx, tenantA, endpoint, key, "fp1"); err != nil || rec != nil {
		t.Fatalf("first claim = %v, %v; want nil, nil", rec, err)
	}
	if err := first.Complete(ctx, tenantA, endpoint, key, 201, []byte(`{"id":"1"}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	restarted, err := pgxpool.New(ctx, pools.AppDSN)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer restarted.Close()
	second := idempotency.NewPgStore(restarted)

	rec, err := second.Claim(ctx, tenantA, endpoint, key, "fp1")
	if err != nil || rec == nil || rec.ResponseStatus != 201 || string(rec.ResponseBody) != `{"id":"1"}` {
		t.Fatalf("replay after restart = %+v, %v", rec, err)
	}
	if _, err := second.Claim(ctx, tenantA, endpoint, key, "fp2"); !errors.Is(err, idempotency.ErrFingerprintMismatch) {
		t.Fatalf("different body: err = %v, want ErrFingerprintMismatch", err)
	}
	if rec, err := second.Claim(ctx, tenantB, endpoint, key, "fp1"); err != nil || rec != nil {
		t.Fatalf("other tenant must get its own claim, got %+v, %v", rec, err)
	}

	// RLS: with no tenant installed, the app role sees none of these rows.
	var n int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("app role without app.tenant_id saw %d idempotency rows, want 0", n)
	}
}
