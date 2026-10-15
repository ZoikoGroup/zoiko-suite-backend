package store_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"zoiko.io/eventing/outbox"
)

// The migration must carry outbox.SchemaSQL verbatim. If the library's schema
// changes and this copy does not, VerifySchema would only catch a missing
// column at startup — this catches any drift at test time. Only the
// constant's surrounding blank lines are ignored; every DDL line and comment
// inside it must match.
func TestEventingOutboxMigrationMatchesLibrary(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	migrationDir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")

	raw, err := os.ReadFile(filepath.Join(migrationDir, "007_add_eventing_outbox.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	migration := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.Contains(migration, strings.TrimSpace(outbox.SchemaSQL)) {
		t.Fatal("007 must contain outbox.SchemaSQL verbatim; copy it again from services/eventing/outbox/schema.go")
	}
}
