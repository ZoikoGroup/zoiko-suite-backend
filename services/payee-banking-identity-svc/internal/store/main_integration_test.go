//go:build integration

package store_test

// Integration harness: starts an embedded Postgres for the whole package and
// points TEST_DATABASE_URL at it. An explicitly supplied TEST_DATABASE_URL wins.
//
// Run:
//
//	go test -tags=integration -count=1 -timeout=600s ./internal/store/

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

	port := uint32(18101 + uint32(os.Getpid()%499))
	dbName := "payee_banking_integration_test"

	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V16).
			Port(port).
			Database(dbName).
			Username("postgres").
			Password("postgres").
			RuntimePath(filepath.Join(os.TempDir(), fmt.Sprintf("epg-payee-banking-%d", port))),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("embedded postgres failed to start: %v\n", err)
		os.Exit(1)
	}

	os.Setenv("TEST_DATABASE_URL",
		fmt.Sprintf("postgres://postgres:postgres@localhost:%d/%s?sslmode=disable", port, dbName))

	code := m.Run()
	_ = pg.Stop()
	os.Exit(code)
}
