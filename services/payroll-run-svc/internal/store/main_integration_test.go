//go:build integration

package store_test

import (
	"fmt"
	"os"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// TestMain boots an embedded Postgres and points TEST_DATABASE_URL at it, so
// the service's existing Postgres tests (which skip when the variable is unset)
// and the control-population tests run for real.
//
// Run: go test -tags=integration -count=1 -timeout=400s ./internal/store/
func TestMain(m *testing.M) {
	port := uint32(17501 + uint32(os.Getpid()%499))
	// A private runtime dir: the default (~/.embedded-postgres-go/extracted) is
	// shared by every service's harness and collides when two run at once.
	runtime, err := os.MkdirTemp("", "payroll-pg-")
	if err != nil {
		fmt.Printf("temp dir: %v\n", err)
		os.Exit(1)
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		RuntimePath(runtime).
		Version(embeddedpostgres.V16).Port(port).Database("payroll_test").
		Username("postgres").Password("postgres"))
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("TEST_DATABASE_URL", fmt.Sprintf("postgres://postgres:postgres@localhost:%d/payroll_test?sslmode=disable", port))
	code := m.Run()
	_ = pg.Stop()
	_ = os.RemoveAll(runtime)
	os.Exit(code)
}
