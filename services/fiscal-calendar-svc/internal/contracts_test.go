package internal_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"zoiko.io/fiscal-calendar-svc/internal/events"
)

func loadYAML(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", name))
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(b, &doc), "%s must be valid YAML", name)
	return doc
}

// The contract files must parse, and the three places that name event types -
// code, AsyncAPI and the outbox CHECK constraint - must agree.
func TestContracts_ParseAndEventNamesAgree(t *testing.T) {
	oa := loadYAML(t, "openapi.yaml")
	assert.Equal(t, "3.0.3", oa["openapi"])
	paths, ok := oa["paths"].(map[string]any)
	require.True(t, ok)
	for _, p := range []string{
		"/v1/fiscal-calendars", "/v1/fiscal-calendars:resolve", "/v1/fiscal-calendars/{id}",
		"/v1/fiscal-calendars/{id}/versions", "/v1/fiscal-calendars/{id}/versions:propose-change",
		"/v1/fiscal-calendar-versions/{id}", "/v1/fiscal-calendar-versions/{id}:approve", "/v1/fiscal-calendar-versions/{id}:activate",
		"/v1/fiscal-calendar-versions/{id}/periods-preview", "/v1/fiscal-calendar-versions/{id}/transition-plan",
		"/v1/calendar-transition-plans/{id}", "/v1/calendar-transition-plans/{id}:approve", "/v1/calendar-transition-plans/{id}:reject",
	} {
		assert.Contains(t, paths, p)
	}

	ay := loadYAML(t, "asyncapi.yaml")
	assert.Contains(t, ay["channels"], "zoiko.fiscal-calendar.events")
	msgs := ay["components"].(map[string]any)["messages"].(map[string]any)

	mig, err := os.ReadFile(filepath.Join("..", "deployments", "migrations", "000004_outbox.up.sql"))
	require.NoError(t, err)
	all := []string{events.EventFiscalCalendarCreated, events.EventFiscalCalendarChangeProposed, events.EventFiscalCalendarVersionActivated, events.EventFiscalCalendarSuperseded}
	for _, ev := range all {
		assert.Contains(t, msgs, ev, "asyncapi.yaml documents %s", ev)
		assert.True(t, strings.Contains(string(mig), "'"+ev+"'"), "outbox CHECK constraint allows %s", ev)
	}
	assert.Len(t, msgs, len(all))
}

// The periods-preview response schema documents exactly the keys REF-05 codes against.
func TestContracts_PeriodsPreviewSchema(t *testing.T) {
	oa := loadYAML(t, "openapi.yaml")
	schemas := oa["components"].(map[string]any)["schemas"].(map[string]any)
	pv := schemas["PeriodsPreview"].(map[string]any)
	props := pv["properties"].(map[string]any)
	for _, k := range []string{"calendar_id", "version_id", "version_no", "legal_entity_id", "fiscal_year", "periods"} {
		assert.Contains(t, props, k)
	}
	assert.Len(t, props, 6)
	period := schemas["Period"].(map[string]any)["properties"].(map[string]any)
	for _, k := range []string{"period_key", "period_no", "start_date", "end_date", "kind"} {
		assert.Contains(t, period, k)
	}
	assert.Len(t, period, 5)
}
