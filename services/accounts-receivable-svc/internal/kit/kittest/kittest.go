// Package kittest starts an embedded Postgres with every AR migration applied,
// for the integration tests of the AR modules. The embedded superuser bypasses
// RLS, so tests that prove tenant isolation prove the explicit tenant_id
// predicates, not the policies (same posture as store's control-population tests).
//
// Typical use, in a module's TestMain:
//
//	pool, stop := kittest.Start("ar01_test", 17300)
//	defer stop()
package kittest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationsDir is the absolute path of deployments/migrations.
func MigrationsDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "deployments", "migrations")
}

// Start boots Postgres on basePort+pid%499 (so parallel package tests do not
// collide), applies every *.up.sql in order and returns the pool and a stop func.
// It exits the process on failure, which is what a TestMain wants.
func Start(database string, basePort uint32) (*pgxpool.Pool, func()) {
	port := basePort + uint32(os.Getpid()%499)
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).Port(port).Database(database).
		Username("postgres").Password("postgres"))
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}
	stop := func() { _ = pg.Stop() }

	ctx := context.Background()
	dsn := fmt.Sprintf("host=localhost port=%d dbname=%s user=postgres password=postgres sslmode=disable", port, database)
	pool, err := pgxpool.New(ctx, dsn)
	if err == nil {
		for i := 0; i < 75; i++ {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if err != nil {
		fmt.Printf("embedded postgres not ready: %v\n", err)
		stop()
		os.Exit(1)
	}
	files, _ := filepath.Glob(filepath.Join(MigrationsDir(), "*.up.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, rerr := os.ReadFile(f)
		if rerr == nil {
			_, rerr = pool.Exec(ctx, string(b))
		}
		if rerr != nil {
			fmt.Printf("migration %s: %v\n", filepath.Base(f), rerr)
			pool.Close()
			stop()
			os.Exit(1)
		}
	}
	return pool, func() { pool.Close(); stop() }
}
