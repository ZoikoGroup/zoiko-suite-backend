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

// appRole mirrors the deployed zoiko_app role: no superuser, no BYPASSRLS. The
// embedded cluster's own superuser bypasses row-level security entirely, so a
// suite that only ran as it would never see an RLS policy that locks out the
// outbox relay — the store and relay under test connect as this role instead.
const appRole = "mi_app"

var (
	superPool *pgxpool.Pool // inspection only; bypasses RLS
	appPool   *pgxpool.Pool // what the service actually runs as
)

// TestMain boots an embedded Postgres, applies every migration in order, and
// creates the RLS-respecting application role.
//
// Run: go test -tags=integration -count=1 -timeout=400s ./internal/store/
func TestMain(m *testing.M) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("free port: %v\n", err)
		os.Exit(1)
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()

	// A private runtime dir: the default is shared by every service's harness
	// and collides when two run at once.
	runtime, err := os.MkdirTemp("", "migration-integrity-pg-")
	if err != nil {
		fmt.Printf("temp dir: %v\n", err)
		os.Exit(1)
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		RuntimePath(runtime).
		Version(embeddedpostgres.V16).Port(port).Database("migration_integrity_test").
		Username("postgres").Password("postgres"))
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		_ = os.RemoveAll(runtime)
		os.Exit(1)
	}

	code := func() int {
		ctx := context.Background()
		superDSN := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/migration_integrity_test?sslmode=disable", port)
		superPool, err = pgxpool.New(ctx, superDSN)
		if err != nil {
			fmt.Printf("connect: %v\n", err)
			return 1
		}
		defer superPool.Close()
		for i := 0; i < 75; i++ {
			if err = superPool.Ping(ctx); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			fmt.Printf("embedded postgres not ready: %v\n", err)
			return 1
		}

		files, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
		sort.Strings(files)
		for _, f := range files {
			b, rerr := os.ReadFile(f)
			if rerr == nil {
				_, rerr = superPool.Exec(ctx, string(b))
			}
			if rerr != nil {
				fmt.Printf("migration %s: %v\n", f, rerr)
				return 1
			}
		}

		if _, err := superPool.Exec(ctx, `
			CREATE ROLE `+appRole+` WITH LOGIN PASSWORD 'mi_app' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
			GRANT USAGE ON SCHEMA public TO `+appRole+`;
			GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO `+appRole+`;`); err != nil {
			fmt.Printf("create app role: %v\n", err)
			return 1
		}
		appPool, err = pgxpool.New(ctx, fmt.Sprintf("postgres://%s:mi_app@localhost:%d/migration_integrity_test?sslmode=disable", appRole, port))
		if err != nil {
			fmt.Printf("connect as app role: %v\n", err)
			return 1
		}
		defer appPool.Close()

		return m.Run()
	}()

	_ = pg.Stop()
	_ = os.RemoveAll(runtime)
	os.Exit(code)
}
