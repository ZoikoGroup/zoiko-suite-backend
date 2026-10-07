package events_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"

	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

// ORG §9.2 gate 1, event half: every field asyncapi.yaml's Envelope marks
// `required` is present on a record the code actually builds, and every field
// the code emits is declared — so a consumer generated from the contract
// neither misses a field nor chokes on an undeclared one.
func TestAsyncAPI_EnvelopeSchemaMatchesWhatTheCodeEmits(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "../../asyncapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				Envelope struct {
					Required   []string       `yaml:"required"`
					Properties map[string]any `yaml:"properties"`
				} `yaml:"Envelope"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	schema := doc.Components.Schemas.Envelope

	rec, err := events.BuildRecord(events.RecordSpec{
		EventType: "entity.profile.amended", TenantID: "t", LegalEntityID: "e", Jurisdiction: "j",
		ActorID: "a", CorrelationID: "c", ObjectID: "e", ObjectVersion: 2, EvidenceRef: "ev",
		Payload: map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatal(err)
	}
	emitted := map[string]any{}
	if err := json.Unmarshal(rec.Payload, &emitted); err != nil {
		t.Fatal(err)
	}
	for _, f := range schema.Required {
		if _, ok := emitted[f]; !ok {
			t.Errorf("asyncapi requires %q but the code does not emit it", f)
		}
	}
	for f := range emitted {
		if _, ok := schema.Properties[f]; !ok {
			t.Errorf("the code emits %q but asyncapi's Envelope does not declare it", f)
		}
	}
}
