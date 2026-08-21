// These tests drive a REAL http server rather than a stubbed interface.
//
// Every handler test in this service replaces this client with a double, so
// nothing exercised the parsing — and parsing a response shape the dependency
// does not actually send is the failure mode that cost financial-close-svc
// three separate outages-in-waiting (an authz client reading a boolean field
// that never existed, so every check denied; a vault client reading an
// envelope that was never sent, so every close failed). The fixtures below are
// the literal shapes evidence-requirements-svc's own handler emits.
package evidencereq

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/evidence/evaluate" {
			t.Errorf("client called %s, want /v1/evidence/evaluate", r.URL.Path)
		}
		if r.Header.Get("X-Tenant-Id") == "" {
			t.Error("evaluate was sent with no X-Tenant-Id — the catalog is tenant-scoped")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func evaluate(t *testing.T, srv *httptest.Server) error {
	t.Helper()
	c := NewClient(srv.URL)
	return c.EvaluateSufficient(context.Background(), "tenant-a", "le-us", "FINANCIAL",
		"RESOLUTION_PASS", "corr-1", "principal-1", []Artifact{{
			EvidenceType: "SUPPORTING_DOCUMENT", ReferenceID: "doc-1",
		}})
}

func TestEvaluateSufficient_Satisfied_Allows(t *testing.T) {
	srv := newServer(t, http.StatusOK, `{"outcome":"SATISFIED","unmet":[]}`)
	if err := evaluate(t, srv); err != nil {
		t.Fatalf("SATISFIED was refused: %v", err)
	}
}

func TestEvaluateSufficient_NoRequirementsDefined_Allows(t *testing.T) {
	srv := newServer(t, http.StatusOK, `{"outcome":"NO_REQUIREMENTS_DEFINED","unmet":[]}`)
	if err := evaluate(t, srv); err != nil {
		t.Fatalf("NO_REQUIREMENTS_DEFINED was refused: %v", err)
	}
}

func TestEvaluateSufficient_Missing_Refuses(t *testing.T) {
	srv := newServer(t, http.StatusOK,
		`{"outcome":"MISSING","unmet":[{"evidence_type":"BOARD_MINUTES","reason":"no artifact presented"}]}`)
	if err := evaluate(t, srv); !errors.Is(err, ErrEvidenceMissing) {
		t.Fatalf("MISSING returned %v, want ErrEvidenceMissing", err)
	}
}

// The check used to be `if outcome == "MISSING" { refuse }; allow` — so
// anything the client did not recognise passed. That is a fail-OPEN evidence
// gate, and the two cases below are exactly how it would happen in practice:
// a response shape that changed, and a value the catalog started returning.
func TestEvaluateSufficient_UnrecognisedOutcome_FailsClosed(t *testing.T) {
	srv := newServer(t, http.StatusOK, `{"outcome":"PENDING_REVIEW"}`)
	if err := evaluate(t, srv); err == nil {
		t.Fatal("an unrecognised outcome was allowed — the evidence gate failed open")
	}
}

func TestEvaluateSufficient_AbsentOutcomeField_FailsClosed(t *testing.T) {
	// The zero value. This is what a renamed field, or a wrapped envelope,
	// looks like from the client's side — indistinguishable from SATISFIED
	// under a deny-list.
	srv := newServer(t, http.StatusOK, `{"result":"SATISFIED"}`)
	if err := evaluate(t, srv); err == nil {
		t.Fatal("a response with no outcome field was allowed — the evidence gate failed open")
	}
}

func TestEvaluateSufficient_Non200_FailsClosed(t *testing.T) {
	srv := newServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
	if err := evaluate(t, srv); !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("a 500 returned %v, want ErrServiceUnavailable", err)
	}
}

func TestEvaluateSufficient_Unreachable_FailsClosed(t *testing.T) {
	c := NewClient("http://127.0.0.1:1") // nothing listens here
	err := c.EvaluateSufficient(context.Background(), "tenant-a", "le-us", "FINANCIAL",
		"RESOLUTION_PASS", "corr-1", "principal-1", nil)
	if !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("an unreachable service returned %v, want ErrServiceUnavailable", err)
	}
}

// The request body is the other half of the contract: the field names have to
// be the ones evidence-requirements-svc reads, or the evaluation is performed
// against an empty domain and action.
func TestEvaluateSufficient_SendsTheFieldNamesTheCatalogReads(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"SATISFIED"}`))
	}))
	defer srv.Close()

	if err := evaluate(t, srv); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	for _, field := range []string{"legal_entity_id", "domain_code", "action_type", "present_artifacts", "correlation_id"} {
		if _, ok := got[field]; !ok {
			t.Errorf("request body is missing %q — the catalog would evaluate against a zero value", field)
		}
	}
	if got["domain_code"] != "FINANCIAL" {
		t.Errorf("domain_code sent as %v, want FINANCIAL", got["domain_code"])
	}
}
