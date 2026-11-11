package events_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"

	"zoiko.io/governance-decision-log-svc/internal/domain"
	"zoiko.io/governance-decision-log-svc/internal/events"
)

// The JSON Schemas in schemas/ are what deployments/scripts/
// register-governance-decision-log-schemas.py registers in schema-registry-svc
// under all four names (rule R9). This test holds them to what the producer
// actually emits, so the registered contract cannot drift from the wire.

func loadSchema(t *testing.T, name string) *openapi3.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", name))
	require.NoError(t, err)
	var s openapi3.Schema
	require.NoError(t, json.Unmarshal(raw, &s))
	return &s
}

func asValue(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(b, &v))
	return v
}

func TestR9_EmittedEnvelopesMatchTheRegisteredSchemas(t *testing.T) {
	recorded := loadSchema(t, "governance.decision.recorded.v1.json")
	replayed := loadSchema(t, "governance.decision.replayed.v1.json")

	cause, jur, title, prio := "dec-0", "GB", "Approve renewal", "HIGH"
	full := domain.GovernanceDecision{DecisionID: "dec-1", TenantID: "t-1", LegalEntityID: "le-1", ActorID: "a-1",
		ActionType: "CONTRACT_APPROVE", Outcome: "GRANTED", RuleBasis: "PC:v1", CorrelationID: "c-1",
		CausationID: &cause, JurisdictionContext: &jur, Title: &title, Priority: &prio, Tags: []string{"x"},
		DecidedAt: time.Now(), Domain: "LEGAL_CONTRACTS", ChainSeq: 7,
		CurrHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", DisplayRef: "GD-2026-0007"}
	minimal := domain.GovernanceDecision{DecisionID: "dec-2", TenantID: "t-1", LegalEntityID: "le-1", ActorID: "a-1",
		ActionType: "X", Outcome: "DENIED", RuleBasis: "r", CorrelationID: "c-2", DecidedAt: time.Now(), Domain: "OTHER",
		ChainSeq: 1, CurrHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", DisplayRef: "GD-2026-0001"}

	for name, d := range map[string]domain.GovernanceDecision{"full": full, "minimal (nulls)": minimal} {
		wire, err := events.DecisionRecorded(d)
		require.NoError(t, err)
		queued, err := events.WithCanonicalAlias(wire)
		require.NoError(t, err)
		require.Len(t, queued, 2)
		for _, q := range queued {
			require.NoError(t, recorded.VisitJSON(asValue(t, q.Envelope)), "%s %s", name, q.Type)
		}
	}

	wire, err := events.DecisionReplayed(full, domain.ReplayManifest{ReplayManifestID: "m-1", DecisionID: "dec-1", TenantID: "t-1",
		PolicyVersionID: "pv-1", OriginalOutcome: "GRANTED", ReplayedOutcome: "GRANTED", OutcomesMatch: true,
		ReplayedByPrincipalID: "auditor", ReplayedAt: time.Now()}, "c-r")
	require.NoError(t, err)
	queued, err := events.WithCanonicalAlias(wire)
	require.NoError(t, err)
	for _, q := range queued {
		require.NoError(t, replayed.VisitJSON(asValue(t, q.Envelope)), q.Type)
	}

	// The schemas are not vacuous: an envelope without its payload hash, or
	// with a foreign event type, is refused.
	env := asValue(t, queued[0].Envelope).(map[string]any)
	delete(env, "payload_hash")
	require.Error(t, replayed.VisitJSON(env))
	env = asValue(t, queued[0].Envelope).(map[string]any)
	env["event_type"] = "governance.decision.recorded"
	require.Error(t, replayed.VisitJSON(env))
}

// schema-registry-svc's compatibility checker reads "type", "properties",
// "required" and "items" as plain strings and lists; a property whose type is
// a list ("string","null") would make the registration answer 400.
func TestR9_SchemasAreRegistrableShapes(t *testing.T) {
	type prop struct {
		Type       json.RawMessage `json:"type"`
		Properties map[string]prop `json:"properties"`
	}
	var walk func(t *testing.T, path string, p prop)
	walk = func(t *testing.T, path string, p prop) {
		if len(p.Type) > 0 {
			var s string
			require.NoError(t, json.Unmarshal(p.Type, &s), "%s: type must be a single string", path)
		}
		for k, c := range p.Properties {
			walk(t, path+"."+k, c)
		}
	}
	for _, f := range []string{"governance.decision.recorded.v1.json", "governance.decision.replayed.v1.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", f))
		require.NoError(t, err)
		var root prop
		require.NoError(t, json.Unmarshal(raw, &root))
		walk(t, f, root)
	}
}
