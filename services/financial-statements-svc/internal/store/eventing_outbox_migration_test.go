package store_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/eventing/outbox"
)

// The migration must carry outbox.SchemaSQL verbatim.
func TestEventingOutboxMigrationMatchesLibrary(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	migrationDir := filepath.Join(filepath.Dir(filename), "../../deployments/migrations")

	raw, err := os.ReadFile(filepath.Join(migrationDir, "000002_add_eventing_outbox.up.sql"))
	require.NoError(t, err)
	migration := strings.ReplaceAll(string(raw), "\r\n", "\n")
	assert.Contains(t, migration, outbox.SchemaSQL,
		"000002 must contain outbox.SchemaSQL verbatim; copy it again from services/eventing/outbox/schema.go")
}
