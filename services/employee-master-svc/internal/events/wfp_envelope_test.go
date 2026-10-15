package events_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/employee-master-svc/internal/domain"
	"zoiko.io/employee-master-svc/internal/events"
)

func TestBuildWFPEnvelope(t *testing.T) {
	body, err := events.BuildWFPEnvelope(domain.EventProfileSynced, "corr-1", "cause-1", "p-1", "tenant-a", "le-1", "GB", map[string]any{"profile_id": "x"})
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "worker_financial_profile_synced", got["event_type"])
	assert.Equal(t, "employee-master-svc", got["source_service"])
	assert.Equal(t, "corr-1", got["correlation_id"])
	assert.Equal(t, "cause-1", got["causation_id"])
	assert.Equal(t, "p-1", got["actor_id"])
	assert.Equal(t, "tenant-a", got["tenant_id"])
	assert.Equal(t, "le-1", got["legal_entity_id"])
	assert.Equal(t, "GB", got["jurisdiction"])
	assert.NotEmpty(t, got["event_id"])
	assert.Equal(t, map[string]any{"profile_id": "x"}, got["payload"])

	_, err = events.BuildWFPEnvelope("worker_paid", "c", "", "p", "t", "le", "", nil)
	assert.Error(t, err, "an uncatalogued event cannot be built")
}
