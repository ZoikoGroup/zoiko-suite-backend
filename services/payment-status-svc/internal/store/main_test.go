//go:build integration

package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/payment-status-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
	testDSN   string
)

// TestMain provisions a real, throwaway embedded Postgres instance and
// applies every migration in filename order — this service had no
// real-Postgres test harness at all before this file, only a stub store
// exercised by internal/handler/handler_test.go, which cannot prove a
// database-level trigger or the transactional outbox actually works.
//
// RuntimePath is set explicitly to a pid-derived private directory: the
// default (~/.embedded-postgres-go/extracted) is shared across every
// service's test harness and collides on Windows when two run at once
// ("Access is denied" cleaning up the shared extraction directory) — a
// real bug found and fixed in financial-close-svc and bank-reconciliation-svc
// earlier this session; built in here from the start rather than hit later.
func TestMain(m *testing.M) {
	ctx := context.Background()
	stop := func() {}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dbPort := uint32(15961 + uint32(os.Getpid()%499))
		pg := embeddedpostgres.NewDatabase(
			embeddedpostgres.DefaultConfig().
				Version(embeddedpostgres.V16).
				Port(dbPort).
				Database("payment_status_test").
				Username("postgres").
				Password("postgres").
				RuntimePath(filepath.Join(os.TempDir(), fmt.Sprintf("epg-payment-status-%d", dbPort))),
		)
		if err := pg.Start(); err != nil {
			fmt.Printf("failed to start embedded postgres: %v\n", err)
			os.Exit(1)
		}
		stop = func() { _ = pg.Stop() }
		dsn = fmt.Sprintf("host=localhost port=%d dbname=payment_status_test user=postgres password=postgres sslmode=disable", dbPort)
	}
	testDSN = dsn
	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("failed to connect: %v\n", err)
		stop()
		os.Exit(1)
	}
	for i := 0; i < 75; i++ {
		if err = testPool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Printf("postgres did not become ready: %v\n", err)
		testPool.Close()
		stop()
		os.Exit(1)
	}
	if os.Getenv("TEST_DATABASE_URL") != "" {
		// a shared server: start from an empty schema so the migrations run from nothing
		if _, err := testPool.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
			fmt.Printf("reset schema: %v\n", err)
			os.Exit(1)
		}
	}

	// Every migration in filename order, never a hardcoded list.
	migrations, err := filepath.Glob("../../deployments/migrations/*.up.sql")
	if err != nil || len(migrations) == 0 {
		fmt.Printf("no migrations found: %v\n", err)
		testPool.Close()
		stop()
		os.Exit(1)
	}
	sort.Strings(migrations)
	for _, migration := range migrations {
		sql, readErr := os.ReadFile(migration)
		if readErr != nil {
			fmt.Printf("read migration %s: %v\n", filepath.Base(migration), readErr)
			testPool.Close()
			stop()
			os.Exit(1)
		}
		if _, err := testPool.Exec(ctx, string(sql)); err != nil {
			fmt.Printf("apply migration %s: %v\n", filepath.Base(migration), err)
			testPool.Close()
			stop()
			os.Exit(1)
		}
	}
	testStore = store.NewPgStore(testPool, zap.NewNop(), store.WithEventRegion("uk"))

	code := m.Run()
	testPool.Close()
	stop()
	os.Exit(code)
}
