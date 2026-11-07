//go:build integration

package asyncpgx_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/identity-envelope/apicontract"
	"zoiko.io/identity-envelope/apicontract/asyncpgx"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ASYNC_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:55432/apicontract_test?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func setup(t *testing.T) *asyncpgx.Store {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS async_operations"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := asyncpgx.ApplySchema(ctx, pool); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS async_operations")
	})
	return asyncpgx.NewPool(pool)
}

func queued(tenant string) apicontract.Operation {
	return apicontract.Operation{
		ID: apicontract.NewOperationID(), TenantID: tenant, PrincipalID: "p1", Route: "/v1/exports",
		Type: "export", State: apicontract.OperationQueued, Cancelable: true,
		IdempotencyKey: "idem-1", RequestHash: "hash-1",
	}
}

func TestCreateReplaysIdempotentRequest(t *testing.T) {
	store := setup(t)
	ctx := context.Background()
	op := queued("t1")

	first, created, err := store.Create(ctx, op)
	if err != nil || !created {
		t.Fatalf("create = %+v, %v, %v", first, created, err)
	}
	if first.CreatedAt.IsZero() || !first.UpdatedAt.Equal(first.CreatedAt) {
		t.Fatalf("timestamps = %v / %v", first.CreatedAt, first.UpdatedAt)
	}

	replay, created, err := store.Create(ctx, op)
	if err != nil || created {
		t.Fatalf("replay = %+v, %v, %v", replay, created, err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay id = %q, want %q", replay.ID, first.ID)
	}

	conflict := op
	conflict.RequestHash = "hash-2"
	if _, _, err := store.Create(ctx, conflict); !errors.Is(err, apicontract.ErrOperationConflict) {
		t.Fatalf("conflict err = %v", err)
	}
}

func TestGetIsTenantScoped(t *testing.T) {
	store := setup(t)
	ctx := context.Background()
	op, _, err := store.Create(ctx, queued("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "t2", op.ID); !errors.Is(err, apicontract.ErrOperationNotFound) {
		t.Fatalf("cross-tenant err = %v", err)
	}
	got, err := store.Get(ctx, "t1", op.ID)
	if err != nil || got.ID != op.ID {
		t.Fatalf("get = %+v, %v", got, err)
	}
}

func TestAdvanceLifecycle(t *testing.T) {
	store := setup(t)
	ctx := context.Background()
	op, _, err := store.Create(ctx, queued("t1"))
	if err != nil {
		t.Fatal(err)
	}

	progress := &apicontract.OperationProgress{CompletedUnits: 3, TotalUnits: 10}
	running, err := store.Advance(ctx, "t1", op.ID, apicontract.OperationRunning, progress, nil, "")
	if err != nil {
		t.Fatalf("advance running: %v", err)
	}
	if running.State != apicontract.OperationRunning || running.StartedAt == nil {
		t.Fatalf("running = %+v", running)
	}
	if running.Progress == nil || running.Progress.CompletedUnits != 3 {
		t.Fatalf("progress = %+v", running.Progress)
	}

	bad := &apicontract.Problem{Status: 500, Code: "export_failed", Retryable: true}
	if _, err := store.Advance(ctx, "t1", op.ID, apicontract.OperationQueued, nil, nil, ""); !errors.Is(err, apicontract.ErrOperationConflict) {
		t.Fatalf("backwards transition err = %v", err)
	}

	done, err := store.Advance(ctx, "t1", op.ID, apicontract.OperationFailed, nil, bad, "res_1")
	if err != nil {
		t.Fatalf("advance failed: %v", err)
	}
	if done.State != apicontract.OperationFailed || done.FinishedAt == nil || done.Cancelable {
		t.Fatalf("done = %+v", done)
	}
	if done.Problem == nil || done.Problem.Code != "export_failed" {
		t.Fatalf("problem = %+v", done.Problem)
	}
	if done.ResourceRef != "res_1" {
		t.Fatalf("resource ref = %q", done.ResourceRef)
	}
	if _, err := store.RequestCancel(ctx, "t1", op.ID); !errors.Is(err, apicontract.ErrOperationNotCancelable) {
		t.Fatalf("cancel terminal err = %v", err)
	}
}

func TestRequestCancel(t *testing.T) {
	store := setup(t)
	ctx := context.Background()
	op, _, err := store.Create(ctx, queued("t1"))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := store.RequestCancel(ctx, "t1", op.ID)
	if err != nil || cancelled.State != apicontract.OperationCancelRequested {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	again, err := store.RequestCancel(ctx, "t1", op.ID)
	if err != nil || again.State != apicontract.OperationCancelRequested {
		t.Fatalf("repeat cancel = %+v, %v", again, err)
	}
	final, err := store.Advance(ctx, "t1", op.ID, apicontract.OperationCancelled, nil, nil, "")
	if err != nil || final.State != apicontract.OperationCancelled {
		t.Fatalf("final cancel = %+v, %v", final, err)
	}
}

func TestFindByKey(t *testing.T) {
	store := setup(t)
	ctx := context.Background()
	op, _, err := store.Create(ctx, queued("t1"))
	if err != nil {
		t.Fatal(err)
	}
	found, ok, err := store.FindByKey(ctx, "t1", "p1", "/v1/exports", "idem-1")
	if err != nil || !ok || found.ID != op.ID {
		t.Fatalf("find = %+v, %v, %v", found, ok, err)
	}
	if _, ok, err := store.FindByKey(ctx, "t1", "p2", "/v1/exports", "idem-1"); err != nil || ok {
		t.Fatalf("other principal found = %v, %v", ok, err)
	}
	if _, ok, err := store.FindByKey(ctx, "t1", "p1", "/v1/exports", ""); err != nil || ok {
		t.Fatalf("empty key found = %v, %v", ok, err)
	}
}

func TestCreateWithoutIdempotencyKey(t *testing.T) {
	store := setup(t)
	ctx := context.Background()
	op := queued("t1")
	op.IdempotencyKey = ""
	first, created, err := store.Create(ctx, op)
	if err != nil || !created {
		t.Fatalf("first = %+v, %v, %v", first, created, err)
	}
	op.ID = apicontract.NewOperationID()
	second, created, err := store.Create(ctx, op)
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("second = %+v, %v, %v", second, created, err)
	}
}

func TestFinishTimeOrdering(t *testing.T) {
	store := setup(t)
	ctx := context.Background()
	op, _, err := store.Create(ctx, queued("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(ctx, "t1", op.ID, apicontract.OperationRunning, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	done, err := store.Advance(ctx, "t1", op.ID, apicontract.OperationSucceeded, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if done.StartedAt == nil || done.FinishedAt == nil || done.FinishedAt.Before(*done.StartedAt) {
		t.Fatalf("started/finished = %v / %v", done.StartedAt, done.FinishedAt)
	}
}
