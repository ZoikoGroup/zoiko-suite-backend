package store_test

// Real-PostgreSQL tests for the idempotency remediation: the claim, the
// change, its audit row, its outbox event and the stored replay response commit
// in ONE transaction (RunIdempotent), scoped to (tenant-or-platform, principal,
// key).

import (
	"context"
	"errors"
	"sync"
	"testing"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
)

func sp(s string) *string { return &s }

func idemClaim(tenant *string, principal, key string) domain.IdempotencyRecord {
	return domain.IdempotencyRecord{IdempotencyKey: key, TenantID: tenant, PrincipalID: principal, Operation: "op", RequestHash: "h-" + key}
}

func TestRunIdempotent_ChangeAndClaimAndResponseCommitTogether(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	ctx := tctx(tenantA)
	existing, status, _, err := st.RunIdempotent(ctx, idemClaim(sp(tenantA), "p1", "k1"), func(c context.Context) (int, any, error) {
		_, v, err := st.CreatePurpose(c, tenantA, domain.CreatePurposeRequest{Statement: "s1", CompatibilityClass: "PRIMARY"}, "p1")
		return 201, v, err
	})
	if err != nil || existing != nil || status != 201 {
		t.Fatalf("first: existing=%v status=%d err=%v", existing, status, err)
	}
	var purposes, keys int
	_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM purposes`).Scan(&purposes)
	_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM purpose_registry_idempotency_keys WHERE idempotency_key='k1' AND response_status=201 AND response_body <> 'null'::jsonb`).Scan(&keys)
	if purposes != 1 || keys != 1 {
		t.Fatalf("purposes=%d keys=%d; both must exist", purposes, keys)
	}
	existing, _, _, err = st.RunIdempotent(ctx, idemClaim(sp(tenantA), "p1", "k1"), func(context.Context) (int, any, error) {
		t.Fatal("a duplicate must not execute")
		return 0, nil, nil
	})
	if err != nil || existing == nil || existing.ResponseStatus != 201 || existing.RequestHash != "h-k1" {
		t.Fatalf("duplicate: %+v %v", existing, err)
	}
}

func TestRunIdempotent_SaveFailureRollsBackTheChange_RetryCreatesOne(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	bg := context.Background()
	for _, stmt := range []string{
		`CREATE FUNCTION fail_idem_update() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected idempotency save failure'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_idem_update BEFORE UPDATE ON purpose_registry_idempotency_keys FOR EACH ROW EXECUTE FUNCTION fail_idem_update()`,
	} {
		if _, err := db.Admin.Exec(bg, stmt); err != nil {
			t.Fatal(err)
		}
	}
	op := func(c context.Context) (int, any, error) {
		_, v, err := st.CreatePurpose(c, tenantA, domain.CreatePurposeRequest{Statement: "s2", CompatibilityClass: "PRIMARY"}, "p1")
		return 201, v, err
	}
	if _, _, _, err := st.RunIdempotent(tctx(tenantA), idemClaim(sp(tenantA), "p1", "k-fail"), op); err == nil {
		t.Fatal("a failed idempotency save must fail the operation")
	}
	var purposes, keys int
	_ = db.Admin.QueryRow(bg, `SELECT count(*) FROM purposes`).Scan(&purposes)
	_ = db.Admin.QueryRow(bg, `SELECT count(*) FROM purpose_registry_idempotency_keys`).Scan(&keys)
	if purposes != 0 || keys != 0 {
		t.Fatalf("the change and the claim must roll back together (purposes=%d keys=%d)", purposes, keys)
	}
	if _, err := db.Admin.Exec(bg, `DROP TRIGGER fail_idem_update ON purpose_registry_idempotency_keys`); err != nil {
		t.Fatal(err)
	}
	existing, _, _, err := st.RunIdempotent(tctx(tenantA), idemClaim(sp(tenantA), "p1", "k-fail"), op)
	if err != nil || existing != nil {
		t.Fatalf("retry: %v %v", existing, err)
	}
	_ = db.Admin.QueryRow(bg, `SELECT count(*) FROM purposes`).Scan(&purposes)
	if purposes != 1 {
		t.Fatalf("the retry must create exactly one purpose, got %d", purposes)
	}
}

func TestRunIdempotent_OperationErrorRollsBackTheClaim(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	_, _, _, err := st.RunIdempotent(tctx(tenantA), idemClaim(sp(tenantA), "p1", "k-err"), func(c context.Context) (int, any, error) {
		v, err := st.PublishPurposeVersion(c, "00000000-0000-0000-0000-00000000dead", "00000000-0000-0000-0000-00000000beef", "p1", "corr")
		return 200, v, err
	})
	if !errors.Is(err, domain.ErrPurposeVersionNotFound) {
		t.Fatalf("the operation's domain error must reach the caller, got %v", err)
	}
	var keys int
	_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM purpose_registry_idempotency_keys`).Scan(&keys)
	if keys != 0 {
		t.Fatal("a refused operation must not consume the idempotency key")
	}
}

func TestRunIdempotent_ConcurrentIdenticalRequests_ExecuteOnce(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, replays := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			existing, _, _, err := st.RunIdempotent(tctx(tenantA), idemClaim(sp(tenantA), "p1", "race"), func(c context.Context) (int, any, error) {
				_, v, err := st.CreatePurpose(c, tenantA, domain.CreatePurposeRequest{Statement: "race", CompatibilityClass: "PRIMARY"}, "p1")
				return 201, v, err
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("RunIdempotent: %v", err)
			case existing != nil:
				replays++
			default:
				winners++
			}
		}()
	}
	wg.Wait()
	var purposes int
	_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM purposes`).Scan(&purposes)
	if winners != 1 || replays != n-1 || purposes != 1 {
		t.Fatalf("winners=%d replays=%d purposes=%d", winners, replays, purposes)
	}
}

func TestRunIdempotent_ScopedByTenantPlatformAndPrincipal(t *testing.T) {
	st, db := newStore(t)
	if st == nil {
		return
	}
	run := func(scope string, tenant *string, principal string) {
		t.Helper()
		existing, _, _, err := st.RunIdempotent(tctx(scope), idemClaim(tenant, principal, "same-key"), func(c context.Context) (int, any, error) {
			_, v, err := st.CreatePurpose(c, recordTenant(scope), domain.CreatePurposeRequest{Statement: "x", CompatibilityClass: "PRIMARY"}, principal)
			return 201, v, err
		})
		if err != nil || existing != nil {
			t.Fatalf("%s/%s: existing=%v err=%v", scope, principal, existing, err)
		}
	}
	run(tenantA, sp(tenantA), "p1")
	run(tenantA, sp(tenantA), "p2")
	run(tenantB, sp(tenantB), "p1")
	run(platform, nil, "p1") // the platform plane is its own scope
	var keys int
	_ = db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM purpose_registry_idempotency_keys WHERE idempotency_key='same-key'`).Scan(&keys)
	if keys != 4 {
		t.Fatalf("the same key under another principal, tenant or the platform plane is a different claim (keys=%d)", keys)
	}
}

func TestRunIdempotent_PlatformClaimFromATenantContextIsRefused(t *testing.T) {
	st, _ := newStore(t)
	if st == nil {
		return
	}
	// A tenant session cannot claim a platform (NULL-tenant) key: RLS refuses it.
	if _, _, _, err := st.RunIdempotent(tctx(tenantA), idemClaim(nil, "p1", "k"), func(context.Context) (int, any, error) {
		return 200, map[string]int{}, nil
	}); err == nil {
		t.Fatal("a tenant context stored a platform idempotency record")
	}
}
