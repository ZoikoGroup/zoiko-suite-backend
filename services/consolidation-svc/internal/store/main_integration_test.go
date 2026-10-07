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

// TestMain boots an embedded Postgres, applies every migration and points
// TEST_DATABASE_URL at it, so the service's existing Postgres tests (which skip
// when the variable is unset) and the control-population tests run for real.
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
		ctx := context.Background()
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			fmt.Printf("connect: %v\n", err)
			return 1
		}
		defer pool.Close()
		for i := 0; i < 75; i++ {
			if err = pool.Ping(ctx); err == nil {
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
				_, rerr = pool.Exec(ctx, string(b))
			}
			if rerr != nil {
				fmt.Printf("migration %s: %v\n", f, rerr)
				return 1
			}
		}
		os.Setenv("TEST_DATABASE_URL", dsn)
		return m.Run()
	}()

	_ = pg.Stop()
	_ = os.RemoveAll(runtime)
	os.Exit(code)
}
