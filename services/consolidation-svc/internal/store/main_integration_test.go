//go:build integration

package store_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMain prepares a disposable Postgres and points TEST_DATABASE_URL at it,
// so the service's existing Postgres tests (which skip when the variable is
// unset) and the control-population tests run for real.
//
// If TEST_DATABASE_URL is already set, that database is used instead of
// booting the embedded server — the way this suite is run against a fresh
// disposable container. It is reset (the same tables openTestPool drops) and
// every migration applied, so it must never point at a shared database.
//
// Run: go test -tags=integration -count=1 -timeout=400s ./internal/store/
func TestMain(m *testing.M) {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		if err := resetAndMigrate(dsn); err != nil {
			fmt.Printf("prepare external postgres: %v\n", err)
			os.Exit(1)
		}
		os.Exit(m.Run())
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("free port: %v\n", err)
		os.Exit(1)
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()

	// A private runtime dir: the default (~/.embedded-postgres-go/extracted) is
	// shared by every service's harness and collides when two run at once.
	runtime, err := os.MkdirTemp("", "consolidation-pg-")
	if err != nil {
		fmt.Printf("temp dir: %v\n", err)
		os.Exit(1)
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		RuntimePath(runtime).
		Version(embeddedpostgres.V16).Port(port).Database("consolidation_test").
		Username("postgres").Password("postgres"))
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		_ = os.RemoveAll(runtime)
		os.Exit(1)
	}
	dsn := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/consolidation_test?sslmode=disable", port)

	code := func() int {
		if err := resetAndMigrate(dsn); err != nil {
			fmt.Printf("prepare embedded postgres: %v\n", err)
			return 1
		}
		os.Setenv("TEST_DATABASE_URL", dsn)
		return m.Run()
	}()

	_ = pg.Stop()
	_ = os.RemoveAll(runtime)
	os.Exit(code)
}

// resetAndMigrate drops the service's tables and applies every migration in
// order, leaving a known-empty schema for the suite.
func resetAndMigrate(dsn string) error {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	var pingErr error
	for i := 0; i < 75; i++ {
		if pingErr = pool.Ping(ctx); pingErr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pingErr != nil {
		return fmt.Errorf("postgres not ready: %w", pingErr)
	}

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS
		command_idempotency, eventing_outbox, consolidation_eliminations, consolidation_adjustment_transitions, consolidation_adjustments, balance_contributions, balance_snapshots, consolidation_runs
		CASCADE;`); err != nil {
		return fmt.Errorf("reset schema: %w", err)
	}

	files, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
	sort.Strings(files)
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			return fmt.Errorf("read migration %s: %w", f, rerr)
		}
		if _, rerr := pool.Exec(ctx, string(b)); rerr != nil {
			return fmt.Errorf("migration %s: %w", f, rerr)
		}
	}
	return nil
}
