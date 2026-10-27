package store_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/public-ai-svc/deployments/migrations"
	"zoiko.io/public-ai-svc/internal/store"
	"zoiko.io/public-ai-svc/internal/store/storetest"
)

func TestMemoryConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return store.NewMemory() })
}

// openTestDB returns a pool on TEST_DATABASE_URL with a freshly migrated
// public_ai schema, or skips. Only this package touches the database so
// parallel package test runs cannot interfere.
func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres test: TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := store.MigrateDown(ctx, pool, migrations.FS); err != nil {
		// First run: the migrations table may not exist yet.
		if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		if err := store.MigrateDown(ctx, pool, migrations.FS); err != nil {
			t.Fatalf("migrate down: %v", err)
		}
	}
	if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return pool
}

func TestPostgresConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return store.NewPostgres(openTestDB(t)) })
}

// Migrations apply cleanly up, down and up again.
func TestPostgresMigrationsRoundTrip(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	if err := store.MigrateDown(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := store.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatalf("idempotent up: %v", err)
	}
}

// The revocation ledger is append-only in the database itself (Doc 08 RB-02).
func TestPostgresRevocationsAppendOnly(t *testing.T) {
	pool := openTestDB(t)
	s := store.NewPostgres(pool)
	ctx := context.Background()
	if _, err := s.AddRevocation(ctx, store.Revocation{TargetKind: "claim", TargetID: "c1", ReasonCode: "TEST", EffectiveAt: time.Now(), CreatedAt: time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM public_ai.revocations`); err == nil {
		t.Fatal("deleting a revocation must be rejected")
	}
	if _, err := pool.Exec(ctx, `UPDATE public_ai.revocations SET target_id = 'other'`); err == nil {
		t.Fatal("updating a revocation must be rejected")
	}
}
