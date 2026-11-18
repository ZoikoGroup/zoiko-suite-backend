package handler_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounting-period-svc/internal/domain"
	"zoiko.io/accounting-period-svc/internal/events"
	"zoiko.io/eventing/integration"
)

func contractsDir() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "contracts", "events")
}

func loadSchema(t *testing.T, canonical string) json.RawMessage {
	t.Helper()
	name := strings.TrimPrefix(canonical, "com.zoikosuite.") + ".schema.json"
	b, err := os.ReadFile(filepath.Join(contractsDir(), name))
	require.NoError(t, err, "every canonical event type needs contracts/events/%s", name)
	return b
}

// Every event the real service emits, across the whole lifecycle plus a refused
// command, satisfies the published payload schema of its type (ZS-EVENT-001
// section 12.1: schema + golden payloads), and the envelope's dataschema names it.
func TestEmittedEventsSatisfyTheirPublishedSchemas(t *testing.T) {
	e := newEnv(t)
	res := e.materialize("v1", nil)
	e.drive(e.periodByKey(res, "FY2026-P01").PeriodID, domain.StateReclosed)
	other := e.periodByKey(res, "FY2026-P02").PeriodID
	e.drive(other, domain.StateHardClosed)
	rec := e.cmd(other, "authorize-reopen", approver, func(b map[string]any) { delete(b, "acc14_workflow_ref") })
	e.expectErr(rec, 422, domain.CodeContextInvalid)

	covered := map[string]bool{}
	for _, entry := range e.store.Outbox() {
		require.NotNil(t, entry.Envelope)
		var wire struct {
			Type       string          `json:"type"`
			DataSchema string          `json:"dataschema"`
			Data       json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal(entry.Payload, &wire))
		schema := loadSchema(t, wire.Type)
		assert.Equal(t, "urn:zoikosuite:schema:"+strings.TrimPrefix(wire.Type, "com.zoikosuite.")+":"+events.SchemaVersion, wire.DataSchema)
		assert.Empty(t, integration.ValidatePayload(schema, wire.Data), "%s: %s", wire.Type, wire.Data)
		covered[wire.Type] = true
	}
	for _, legacy := range events.AllEventTypes {
		canonical, _ := events.CanonicalType(legacy)
		assert.True(t, covered[canonical], "scenario never emitted %s, so its schema is untested", canonical)
	}
}

// A schema file exists for exactly the canonical types, no more and no fewer.
func TestSchemaFilesMatchCanonicalTypes(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(contractsDir(), "*.schema.json"))
	require.NoError(t, err)
	var have, want []string
	for _, f := range files {
		have = append(have, "com.zoikosuite."+strings.TrimSuffix(filepath.Base(f), ".schema.json"))
	}
	for _, l := range events.AllEventTypes {
		c, _ := events.CanonicalType(l)
		want = append(want, c)
	}
	sort.Strings(have)
	sort.Strings(want)
	assert.Equal(t, want, have)
}

// The schemas are real contracts: removing a required fact or changing a type
// is caught (EV-T08).
func TestSchemasRejectBrokenPayloads(t *testing.T) {
	schema := loadSchema(t, "com.zoikosuite.accounting.period.hard-closed")
	good := map[string]any{
		"scope": "TENANT", "tenant_id": "t", "object_type": "accounting_period", "object_id": "o", "object_version": 3,
		"effective_at": "2026-03-01T00:00:00Z", "recorded_at": "2026-03-01T00:00:00Z", "actor": "a", "correlation_id": "c",
		"legal_entity_id": "le", "calendar_id": "cal", "calendar_version_id": "v", "book_scope": "", "module_scope": "",
		"period_key": "FY2026-P01", "fiscal_year": 2026, "period_no": 1, "start_date": "2026-01-01", "end_date": "2026-01-31",
		"kind": "NORMAL", "state": "HARD_CLOSED", "command": "HARD_CLOSE", "from_state": "SOFT_CLOSED", "to_state": "HARD_CLOSED",
		"control_snapshot_ref": "s", "reason": "r",
	}
	raw, _ := json.Marshal(good)
	require.Empty(t, integration.ValidatePayload(schema, raw), "the golden payload must be valid")

	missing := map[string]any{}
	for k, v := range good {
		missing[k] = v
	}
	delete(missing, "period_key")
	raw, _ = json.Marshal(missing)
	assert.NotEmpty(t, integration.ValidatePayload(schema, raw), "a missing required field")

	wrongType := map[string]any{}
	for k, v := range good {
		wrongType[k] = v
	}
	wrongType["object_version"] = "3"
	raw, _ = json.Marshal(wrongType)
	assert.NotEmpty(t, integration.ValidatePayload(schema, raw), "a number sent as a string")
}
