package envelope

// Guards the generated ServicePolicy for this service specifically.
//
// rollout.sh regenerates contract.go from a list, and this service was on the
// entity-scoped list because internal/domain carries legal_entity_id. It
// carries it as OUTPUT: this is the service that issues legal entity IDs. Under
// LegalEntityID: RequiredOnWrite the registry could not be bootstrapped at all
// — POST /v1/tenants creates the tenant an entity would hang from, and
// POST /v1/entities mints the very identifier the header would have to contain.
//
// These tests fail if anyone puts it back on that list.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func envWithoutEntity() Envelope {
	return Envelope{
		TenantID:       "tenant-01",
		ActorSubjectID: "user-01",
		RequestID:      "req-01",
		CorrelationID:  "corr-01",
		SourceChannel:  ChannelWeb,
		IdempotencyKey: "idem-01",
	}
}

func fieldsOf(err *ValidationError) map[string]bool {
	out := map[string]bool{}
	if err == nil {
		return out
	}
	for _, v := range err.Violations {
		out[v.Field] = true
	}
	return out
}

// The registry's own bootstrap routes must be reachable without naming an
// entity, because neither call can name one that does not exist yet.
func TestBootstrapWritesDoNotRequireLegalEntity(t *testing.T) {
	for _, path := range []string{
		"/v1/tenants",            // creates the tenant itself
		"/v1/entities",           // mints the legal entity ID
		"/v1/residency-policies", // tenant-scoped
		"/v1/workspaces",         // tenant-scoped
		"/v1/entity-hierarchies", // names its entities in the body, not the scope
	} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, nil)

			err := ServicePolicy().Validate(envWithoutEntity(), r)
			if fieldsOf(err)["legal_entity_id"] {
				t.Fatalf("POST %s refused for a missing legal_entity_id; this service issues "+
					"entity IDs and cannot require one as input", path)
			}
			if err != nil {
				t.Fatalf("unexpected violations: %v", Fields(err))
			}
		})
	}
}

// The rest of the contract is untouched — this is a targeted exemption, not a
// disabled policy.
func TestUnconditionalFieldsStillEnforced(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/tenants", nil)

	got := fieldsOf(ServicePolicy().Validate(Envelope{}, r))
	for _, want := range []string{
		"tenant_id", "actor_subject_id", "request_id", "correlation_id",
		"source_channel", "idempotency_key",
	} {
		if !got[want] {
			t.Errorf("%s is unconditionally mandatory but was not reported", want)
		}
	}
}
