package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/fiscal-calendar-svc/internal/events"
)

func TestBuild_CarriesRequiredFields(t *testing.T) {
	eff := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	rec := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	key, body, err := events.Build(events.Event{
		Type: events.EventFiscalCalendarVersionActivated, TenantID: "tenant-a", Scope: events.ScopeTenant,
		ObjectType: events.ObjectTypeFiscalCalendarVersion, ObjectID: "obj-1", ObjectVersion: 7,
		EffectiveAt: eff, RecordedAt: rec, Actor: "ctrl-1", CorrelationID: "corr-1", CausationID: "cause-1",
		Data: map[string]any{"calendar_id": "cal-1", "object_id": "must-not-override"},
	})
	require.NoError(t, err)
	assert.Equal(t, "obj-1", key)

	var env map[string]any
	require.NoError(t, json.Unmarshal(body, &env))
	assert.Equal(t, "FiscalCalendarVersionActivated", env["event_type"])
	assert.Equal(t, "tenant-a", env["tenant_id"])
	assert.Equal(t, "fiscal-calendar-svc", env["source_service"])
	assert.Equal(t, "corr-1", env["correlation_id"])
	assert.Equal(t, "cause-1", env["causation_id"])
	p := env["payload"].(map[string]any)
	assert.Equal(t, "obj-1", p["object_id"], "event data can never override envelope facts")
	assert.Equal(t, float64(7), p["object_version"])
	assert.Equal(t, "2026-03-01T00:00:00Z", p["effective_at"])
	assert.Equal(t, "2026-03-01T10:00:00Z", p["recorded_at"])
	assert.Equal(t, "ctrl-1", p["actor"])
	assert.Equal(t, "TENANT", p["scope"])
	assert.Equal(t, "cal-1", p["calendar_id"])
}

func TestBuild_RejectsUnknownTypeAndMissingIdentity(t *testing.T) {
	_, _, err := events.Build(events.Event{Type: "Nope", TenantID: "t", ObjectID: "o"})
	assert.Error(t, err)
	_, _, err = events.Build(events.Event{Type: events.EventFiscalCalendarCreated, ObjectID: "o"})
	assert.Error(t, err)
	_, _, err = events.Build(events.Event{Type: events.EventFiscalCalendarCreated, TenantID: "t"})
	assert.Error(t, err)
}
