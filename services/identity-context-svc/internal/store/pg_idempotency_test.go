// Integration tests for the idempotency store, skipped unless
// TEST_DATABASE_URL is set, matching pg_store_test.go and sharing its
// destructive openTestPool.
//
// These exercise the SQL rather than a fake. The middleware's own tests prove
// the decision logic against an in-memory stand-in; nothing there touches the
// INSERT ... ON CONFLICT that makes a concurrent duplicate impossible, the
// CHECK that nearly rejected every claim, or the two row-level policies whose
// failure mode is silence.
package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/store"
)

const (
	idemTenantA  = "11111111-1111-1111-1111-111111111111"
	idemTenantB  = "22222222-2222-2222-2222-222222222222"
	idemEndpoint = "POST /v1/context/support"
)

func idemStore(t *testing.T) (*store.PgStore, *pgxpool.Pool) {
	t.Helper()
	pool := openTestPool(t)
	return store.New(pool, zap.NewNop()), pool
}

// The claim/execute/record cycle, against the real table.
func TestClaimIdempotencyKey_FirstClaimThenReplay(t *testing.T) {
	s, _ := idemStore(t)
	ctx := context.Background()

	rec, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k1", "fp-a")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if rec != nil {
		t.Fatalf("first claim should win the key and return nil, got %+v", rec)
	}

	// Before the command completes, a duplicate sees the in-flight sentinel.
	// This is the row the CHECK constraint originally rejected: response_status
	// is 0 until the handler answers, and a CHECK of 100..599 alone would have
	// failed every claim and 503'd every command.
	inflight, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k1", "fp-a")
	if err != nil {
		t.Fatalf("in-flight claim: %v", err)
	}
	if inflight == nil || inflight.ResponseStatus != 0 {
		t.Fatalf("expected an in-flight record with status 0, got %+v", inflight)
	}

	if err := s.CompleteIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k1", 201,
		[]byte(`{"support_context_id":"sc-1"}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	replay, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k1", "fp-a")
	if err != nil {
		t.Fatalf("replay claim: %v", err)
	}
	if replay == nil {
		t.Fatal("a completed key must replay, not re-claim")
	}
	if replay.ResponseStatus != 201 {
		t.Fatalf("replay must preserve 201, got %d", replay.ResponseStatus)
	}
	if string(replay.ResponseBody) != `{"support_context_id": "sc-1"}` &&
		string(replay.ResponseBody) != `{"support_context_id":"sc-1"}` {
		t.Fatalf("replay body not preserved: %s", replay.ResponseBody)
	}
}

// The case ErrCodeIdempotencyMismatch was declared for.
func TestClaimIdempotencyKey_DifferentFingerprintIsMismatch(t *testing.T) {
	s, _ := idemStore(t)
	ctx := context.Background()

	if _, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k1", "fp-a"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	_, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k1", "fp-DIFFERENT")
	if !errors.Is(err, store.ErrIdempotencyFingerprintMismatch) {
		t.Fatalf("expected store.ErrIdempotencyFingerprintMismatch, got %v", err)
	}
}

// Two goroutines racing the same key: exactly one may win. This is the
// property the ON CONFLICT DO NOTHING exists for and the one a fake cannot
// prove, because the fake holds a mutex the database does not.
func TestClaimIdempotencyKey_ConcurrentClaimsElectOneWinner(t *testing.T) {
	s, _ := idemStore(t)
	ctx := context.Background()

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0

	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			rec, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "race", "fp-a")
			if err != nil {
				return
			}
			if rec == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("exactly one claimant may execute the command, got %d", winners)
	}
}

// A 5xx releases the claim so a retry can genuinely retry. The DELETE is
// guarded on response_status = 0 so it can never remove a recorded answer.
func TestReleaseIdempotencyKey_OnlyDropsUncompletedClaims(t *testing.T) {
	s, _ := idemStore(t)
	ctx := context.Background()

	// An uncompleted claim is released.
	if _, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-open", "fp"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.ReleaseIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-open"); err != nil {
		t.Fatalf("release: %v", err)
	}
	rec, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-open", "fp")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if rec != nil {
		t.Fatal("a released key must be claimable again")
	}

	// A COMPLETED claim survives a release — otherwise a stray release would
	// silently reopen a command for re-execution.
	if err := s.CompleteIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-done", 204, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-done", "fp"); err != nil {
		t.Fatalf("claim k-done: %v", err)
	}
	if err := s.CompleteIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-done", 204, nil); err != nil {
		t.Fatalf("complete k-done: %v", err)
	}
	if err := s.ReleaseIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-done"); err != nil {
		t.Fatalf("release k-done: %v", err)
	}
	after, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-done", "fp")
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if after == nil || after.ResponseStatus != 204 {
		t.Fatalf("a completed record must survive release, got %+v", after)
	}
}

// A 204 stores no body. jsonb will not take an empty string, so it is written
// as JSON null and must round-trip as "no body" rather than as a crash.
func TestCompleteIdempotencyKey_NoContentRoundTrips(t *testing.T) {
	s, _ := idemStore(t)
	ctx := context.Background()

	if _, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-204", "fp"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.CompleteIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-204", 204, nil); err != nil {
		t.Fatalf("complete with empty body: %v", err)
	}
	rec, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-204", "fp")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if rec == nil || rec.ResponseStatus != 204 || string(rec.ResponseBody) != "null" {
		t.Fatalf("204 did not round-trip: %+v", rec)
	}
}

// RLS, run as a NOSUPERUSER NOBYPASSRLS role. As a superuser this test passes
// whatever the policy says, which is the trap FORCE ROW LEVEL SECURITY does not
// close: FORCE applies the policy to the table OWNER, never to a superuser.
func TestIdempotencyKeys_AreTenantIsolated(t *testing.T) {
	s, admin := idemStore(t)
	ctx := context.Background()

	if _, err := s.ClaimIdempotencyKey(ctx, idemTenantA, idemEndpoint, "k-iso", "fp"); err != nil {
		t.Fatalf("seed tenant A: %v", err)
	}

	ordinary := ordinaryRolePool(t, admin)
	tx, err := ordinary.Begin(ctx)
	if err != nil {
		t.Fatalf("begin as ordinary role: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Tenant B must not see tenant A's command responses.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, idemTenantB); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	var visible int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&visible); err != nil {
		t.Fatalf("count as tenant B: %v", err)
	}
	if visible != 0 {
		t.Fatalf("tenant B can see %d of tenant A's idempotency records", visible)
	}

	// Negative control: the row really is there for its own tenant. Without
	// this the test would pass just as well against an empty table.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, idemTenantA); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&visible); err != nil {
		t.Fatalf("count as tenant A: %v", err)
	}
	if visible != 1 {
		t.Fatalf("tenant A should see its own record, saw %d", visible)
	}
}

// The purge hatch. Its failure mode is silence: without app.retention_sweep
// the DELETE matches nothing, the sweep reports success, and the table grows
// forever.
func TestPurgeIdempotencyKeys_NeedsTheRetentionHatchAndWorksAcrossTenants(t *testing.T) {
	s, admin := idemStore(t)
	ctx := context.Background()

	for _, tenant := range []string{idemTenantA, idemTenantB} {
		if _, err := s.ClaimIdempotencyKey(ctx, tenant, idemEndpoint, "k-old", "fp"); err != nil {
			t.Fatalf("seed %s: %v", tenant, err)
		}
	}
	if _, err := admin.Exec(ctx,
		`UPDATE idempotency_keys SET created_at = NOW() - INTERVAL '30 days'`); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	// Without the hatch, as an ordinary role, the delete must match nothing.
	ordinary := ordinaryRolePool(t, admin)
	tx, err := ordinary.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, idemTenantA); err != nil {
		t.Fatalf("set tenant: %v", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < NOW()`)
	if err != nil {
		t.Fatalf("scoped delete: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("a tenant-scoped delete should reach only its own row, reached %d", tag.RowsAffected())
	}
	_ = tx.Rollback(ctx)

	// With the hatch, the sweep reaches every tenant.
	purged, err := s.PurgeIdempotencyKeysBefore(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if purged != 2 {
		t.Fatalf("the cross-tenant sweep should have purged 2 rows, purged %d", purged)
	}
}

// The reconciler hatch, added by the same migration. SELECT-only by design:
// the sweep reports, a human reviews.
func TestSupportReconcilerHatch_ReadsAcrossTenantsButCannotWrite(t *testing.T) {
	_, admin := idemStore(t)
	ctx := context.Background()

	past := time.Now().UTC().Add(-2 * time.Hour)
	for i, tenant := range []string{idemTenantA, idemTenantB} {
		if _, err := admin.Exec(ctx, `
			INSERT INTO support_contexts
				(support_context_id, tenant_id, support_principal_id, approver_principal_id,
				 reason_code, justification, ticket_ref, granted_at, expires_at,
				 evidence_id, correlation_id)
			VALUES ($1,$2,'p-support','p-approver','INCIDENT_RESPONSE',
			        'integration test seed','TICKET-1',$3,$4,'ev-1','corr-1')`,
			[]string{"sc-aaa", "sc-bbb"}[i], tenant, past.Add(-time.Hour), past); err != nil {
			t.Fatalf("seed support context for %s: %v", tenant, err)
		}
	}

	ordinary := ordinaryRolePool(t, admin)
	tx, err := ordinary.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `SELECT set_config('app.support_reconciler', 'true', true)`); err != nil {
		t.Fatalf("set hatch: %v", err)
	}
	var seen int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM support_contexts WHERE reviewed_at IS NULL`).Scan(&seen); err != nil {
		t.Fatalf("read under hatch: %v", err)
	}
	if seen != 2 {
		t.Fatalf("the reconciler must see every tenant's expired grants, saw %d", seen)
	}

	// SELECT-only: the sweep must not be able to mark anything reviewed. An
	// automatic review is not a review, and a hatch that allowed the write
	// would let a reporting job close its own findings.
	tag, err := tx.Exec(ctx, `UPDATE support_contexts SET reviewed_at = NOW()`)
	if err == nil && tag.RowsAffected() > 0 {
		t.Fatalf("the reconciler hatch granted UPDATE on %d rows; it must be SELECT-only", tag.RowsAffected())
	}
}
