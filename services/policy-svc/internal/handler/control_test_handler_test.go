package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zoiko.io/policy-svc/internal/domain"
)

func TestCreateControlTestDefinition_Created(t *testing.T) {
	store := &stubStore{}
	r := newControlTestRouter(store, &stubAuthz{})

	body := `{"control_ref":"CTRL-SOD-01","test_code":"SOD-01-Q3","title":"SoD review","methodology":"sample 25 approvals against segregation matrix"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/control-test-definitions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var got domain.ControlTestDefinition
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.DesignStatus != "DESIGNED" {
		t.Errorf("expected design_status DESIGNED, got %s", got.DesignStatus)
	}
}

func TestCreateControlTestExecution_TesterComesFromHeaderNotBody(t *testing.T) {
	store := &stubStore{
		definition: &domain.ControlTestDefinition{ControlTestDefinitionID: "ctd-1", ControlRef: "CTRL-SOD-01"},
	}
	r := newControlTestRouter(store, &stubAuthz{})

	// tester_principal_id is not even an accepted field — the acting
	// principal must come from X-Principal-Id, same doctrine as
	// activated_by_principal_id elsewhere in this service.
	body := `{"period_start":"2026-01-01T00:00:00Z","period_end":"2026-03-31T00:00:00Z","result":"EFFECTIVE"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/control-test-definitions/ctd-1/executions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if store.createExecutionTester != testPrincipal {
		t.Errorf("expected tester_principal_id to be the header principal %q, got %q", testPrincipal, store.createExecutionTester)
	}
}

func TestCreateControlTestExecution_UnknownDefinition404(t *testing.T) {
	store := &stubStore{definitionErr: domain.ErrControlTestDefinitionNotFound}
	r := newControlTestRouter(store, &stubAuthz{})

	body := `{"period_start":"2026-01-01T00:00:00Z","period_end":"2026-03-31T00:00:00Z","result":"EFFECTIVE"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/control-test-definitions/does-not-exist/executions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGetControlEffectiveness_DesignAndOperatingAreIndependent is the doc7
// §E3 payoff: a control can be TESTED (a methodology exists) while its
// OPERATING_EFFECTIVENESS still shows INEFFECTIVE — the two fields must
// never collapse into one status.
func TestGetControlEffectiveness_DesignAndOperatingAreIndependent(t *testing.T) {
	asOf := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	execID := "cte-9"
	store := &stubStore{
		effectiveness: &domain.ControlEffectiveness{
			ControlRef:             "CTRL-SOD-01",
			DesignStatus:           "TESTED",
			OperatingEffectiveness: "INEFFECTIVE",
			AsOf:                   &asOf,
			LatestExecutionID:      &execID,
		},
	}
	r := newControlTestRouter(store, &stubAuthz{})

	req := httptest.NewRequest(http.MethodGet, "/v1/controls/CTRL-SOD-01/effectiveness", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got domain.ControlEffectiveness
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.DesignStatus != "TESTED" || got.OperatingEffectiveness != "INEFFECTIVE" {
		t.Fatalf("expected TESTED design status with an independently INEFFECTIVE operating result, got %+v", got)
	}
}

func TestGetControlEffectiveness_NoExecutionsRecorded(t *testing.T) {
	store := &stubStore{}
	r := newControlTestRouter(store, &stubAuthz{})

	req := httptest.NewRequest(http.MethodGet, "/v1/controls/CTRL-NEVER-TESTED/effectiveness", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var got domain.ControlEffectiveness
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.DesignStatus != "NOT_TESTED" || got.OperatingEffectiveness != "NO_EXECUTIONS_RECORDED" {
		t.Fatalf("expected NOT_TESTED/NO_EXECUTIONS_RECORDED for an unknown control, got %+v", got)
	}
}

func TestCreateAttestation_SignerComesFromHeaderNotBody(t *testing.T) {
	store := &stubStore{}
	r := newControlTestRouter(store, &stubAuthz{})

	body := `{"statement":"SoD controls operated as designed for Q1 2026","statement_version":"v1","subject_ref":"CTRL-SOD-01","period_start":"2026-01-01T00:00:00Z","period_end":"2026-03-31T00:00:00Z","signer_role":"CONTROL_OWNER"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/attestations", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if store.createAttestationSigner != testPrincipal {
		t.Errorf("expected signer_principal_id to be the header principal %q, got %q", testPrincipal, store.createAttestationSigner)
	}
}

func TestRevokeAttestation_InvalidTransitionConflict(t *testing.T) {
	store := &stubStore{revokeErr: domain.ErrInvalidAttestationTransition}
	r := newControlTestRouter(store, &stubAuthz{})

	req := authed(httptest.NewRequest(http.MethodPost, "/v1/attestations/att-1/revoke", bytes.NewBufferString(`{"reason":"superseded"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 revoking an already-terminal attestation, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateControlTestDefinition_AuthorizationDenied403(t *testing.T) {
	store := &stubStore{}
	r := newControlTestRouter(store, &stubAuthz{err: domain.ErrAuthorizationDenied})

	body := `{"control_ref":"CTRL-X","test_code":"X-01","title":"t","methodology":"m"}`
	req := authed(httptest.NewRequest(http.MethodPost, "/v1/control-test-definitions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if store.createDefinitionCalls != 0 {
		t.Errorf("expected the store to never be reached when authorization is denied, got %d calls", store.createDefinitionCalls)
	}
}
