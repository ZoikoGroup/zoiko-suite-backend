package internal_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"zoiko.io/accounting-period-svc/internal/events"
)

func loadYAML(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", name))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(b, &doc), "%s must be valid YAML", name)
	return doc
}

// The contract files must parse, and the places that name event types - code,
// AsyncAPI and the outbox CHECK constraint - must agree.
func TestContracts_ParseAndEventNamesAgree(t *testing.T) {
	oa := loadYAML(t, "openapi.yaml")
	assert.Equal(t, "3.0.3", oa["openapi"])
	paths, ok := oa["paths"].(map[string]any)
	require.True(t, ok)
	for _, p := range []string{
		"/v1/accounting-periods:materialize", "/v1/accounting-periods/{idCommand}", "/v1/accounting-periods",
		"/v1/accounting-periods/{id}", "/v1/accounting-periods/{id}/state-history", "/v1/accounting-periods:resolve",
		"/v1/accounting-periods:status-by-key", "/v1/calendar-usage",
	} {
		assert.Contains(t, paths, p)
	}
	assert.Len(t, paths, 8)

	ay := loadYAML(t, "asyncapi.yaml")
	assert.Contains(t, ay["channels"], "zoiko.accounting-period.events")
	msgs := ay["components"].(map[string]any)["messages"].(map[string]any)

	mig, err := os.ReadFile(filepath.Join("..", "deployments", "migrations", "000004_outbox.up.sql"))
	require.NoError(t, err)
	for _, ev := range events.AllEventTypes {
		assert.Contains(t, msgs, ev, "asyncapi.yaml documents %s", ev)
		assert.True(t, strings.Contains(string(mig), "'"+ev+"'"), "outbox CHECK constraint allows %s", ev)
	}
	assert.Len(t, msgs, len(events.AllEventTypes))
}
