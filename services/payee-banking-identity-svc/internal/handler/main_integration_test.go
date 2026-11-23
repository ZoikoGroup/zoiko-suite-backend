//go:build integration

package handler_test

// Integration harness for the handler package's end-to-end tests: starts an
// embedded Postgres for the package and points TEST_DATABASE_URL at it, the
// same way internal/store's harness does. An explicitly supplied
// TEST_DATABASE_URL wins. The tests create a database of their own on it, so
// they never collide with internal/store's tests on a shared server.
//
// Run:
//
//	go test -tags=integration -count=1 -timeout=600s ./internal/handler/

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

func TestMain(m *testing.M) {
	if os.Getenv("TEST_DATABASE_URL") != "" {
		os.Exit(m.Run())
	}

	port := uint32(18651 + uint32(os.Getpid()%299))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V16).
			Port(port).
			Database("payee_handler_integration").
			Username("postgres").
			Password("postgres").
			RuntimePath(filepath.Join(os.TempDir(), fmt.Sprintf("epg-payee-handler-%d", port))),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("embedded postgres failed to start: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("TEST_DATABASE_URL",
		fmt.Sprintf("postgres://postgres:postgres@localhost:%d/payee_handler_integration?sslmode=disable", port))

	code := m.Run()
	_ = pg.Stop()
	os.Exit(code)
}
