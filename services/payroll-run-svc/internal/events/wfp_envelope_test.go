package events_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
)

func TestBuildWFPEnvelope_CarriesPlatformEnvelopeFields(t *testing.T) {
	body, err := events.BuildWFPEnvelope(domain.EventPayrollResultReceived, "corr-1", "cause-1", "p-1", "tenant-a", "le-1", "GB",
		map[string]any{"run_ref_id": "r-1"})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "payroll_result_received", got["event_type"])
	assert.Equal(t, "payroll-run-svc", got["source_service"])
	assert.Equal(t, "1.0", got["event_version"])
	assert.Equal(t, "1.0", got["schema_version"])
	assert.Equal(t, "corr-1", got["correlation_id"])
	assert.Equal(t, "cause-1", got["causation_id"])
	assert.Equal(t, "p-1", got["actor_id"])
	assert.Equal(t, "tenant-a", got["tenant_id"])
	assert.Equal(t, "le-1", got["legal_entity_id"])
	assert.Equal(t, "GB", got["jurisdiction"])
	assert.NotEmpty(t, got["emitted_at"])
	assert.Equal(t, map[string]any{"run_ref_id": "r-1"}, got["payload"])
}

func TestBuildWFPEnvelope_RefusesUncataloguedEvent(t *testing.T) {
	_, err := events.BuildWFPEnvelope("payroll_paid", "c", "", "p", "t", "le", "", nil)
	assert.Error(t, err, "an event outside the canonical catalogue cannot be built")
}
