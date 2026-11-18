package events_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/events"
	"zoiko.io/eventing/catalogue"
	eventingenvelope "zoiko.io/eventing/envelope"
)

func sample(t string) events.Event {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	return events.Event{
		Type: t, TenantID: "tenant-a", Scope: events.ScopeTenant, ObjectType: events.ObjectTypeAccountingPeriod,
		ObjectID: "0198f6c2-0000-7000-8000-000000000001", ObjectVersion: 7, EffectiveAt: now, RecordedAt: now,
		Actor: "steward-1", CorrelationID: "corr-1", CausationID: "cause-1",
		Data: map[string]any{"legal_entity_id": "le-1", "period_key": "FY2026-P03"},
	}
}

// Every event name this service emits has a canonical type that the envelope
// accepts and the catalogue lists, owned by the period service.
func TestEveryEventHasACataloguedCanonicalType(t *testing.T) {
	for _, legacy := range events.AllEventTypes {
		canonical, ok := events.CanonicalType(legacy)
		require.True(t, ok, legacy)
		require.True(t, eventingenvelope.ValidType(canonical), canonical)
		entry, ok := catalogue.Lookup(canonical)
		require.True(t, ok, "%s is not in the event catalogue", canonical)
		assert.Equal(t, "Period Service", entry.Producer, canonical)
		_, built := events.BuildEnvelope(sample(legacy), "uk")
		assert.NoError(t, built, legacy)
	}
}

func TestBuildEnvelope_CarriesStandardAndLegacyFields(t *testing.T) {
	env, err := events.BuildEnvelope(sample(events.EventPeriodHardClosed), "uk")
	require.NoError(t, err)
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))

	assert.Equal(t, "com.zoikosuite.accounting.period.hard-closed", m["type"])
	assert.Equal(t, "PeriodHardClosed", m["event_type"], "legacy name kept for existing consumers")
	assert.Equal(t, "tenant-a", m["tenantid"])
	assert.Equal(t, "le-1", m["legalentityid"])
	assert.Equal(t, "uk", m["residencyregion"])
	assert.Equal(t, "confidential", m["classification"])
	assert.Equal(t, float64(7), m["aggregateversion"])
	assert.Equal(t, "0198f6c2-0000-7000-8000-000000000001", m["aggregateid"])
	assert.Equal(t, "corr-1", m["correlationid"])
	assert.Equal(t, "cause-1", m["causationid"])
	// the legacy `payload` member and `data` carry the same facts
	assert.Equal(t, m["data"], m["payload"])
	assert.Equal(t, "FY2026-P03", m["data"].(map[string]any)["period_key"])
}

// The data is what the legacy Build puts in its payload, so a consumer that
// reads either envelope sees the same facts.
func TestBuildEnvelope_DataMatchesLegacyPayload(t *testing.T) {
	ev := sample(events.EventPeriodReopened)
	_, legacyBody, err := events.Build(ev)
	require.NoError(t, err)
	var legacy struct {
		Payload map[string]any `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(legacyBody, &legacy))

	env, err := events.BuildEnvelope(ev, "uk")
	require.NoError(t, err)
	raw, _ := json.Marshal(env)
	var cur struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &cur))
	assert.Equal(t, legacy.Payload, cur.Data)
}

func TestBuildEnvelope_Refusals(t *testing.T) {
	_, err := events.BuildEnvelope(sample("Nope"), "uk")
	assert.Error(t, err, "unknown type")
	_, err = events.BuildEnvelope(sample(events.EventPeriodOpened), "")
	assert.Error(t, err, "a region is required (ZS-EVENT-001 section 4)")
	e := sample(events.EventPeriodOpened)
	e.TenantID = ""
	_, err = events.BuildEnvelope(e, "uk")
	assert.Error(t, err)
}
