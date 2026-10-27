package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
)

const (
	definitionID  = "dddddddd-0000-0000-0000-000000000001"
	attestationID = "ffffffff-0000-0000-0000-000000000001"
)

func TestCreateControlTestDefinition_CreatedInCallerTenant(t *testing.T) {
	g := newRig(nil, nil, nil)
	w := g.do(http.MethodPost, "/v1/control-test-definitions",
		`{"control_ref":"CTRL-SOD-01","test_code":"SOD-01-Q3","title":"SoD review","methodology":"sample 25 approvals against segregation matrix"}`)
	expect(t, w, http.StatusCreated, "")
	var got domain.ControlTestDefinition
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.DesignStatus != "DESIGNED" {
		t.Errorf("expected design_status DESIGNED, got %s", got.DesignStatus)
	}
	if g.store.createDefinitionTenant != testTenant {
		t.Fatalf("a definition must be owned by the caller's tenant, got %q", g.store.createDefinitionTenant)
	}
}

func TestCreateControlTestExecution_TesterAndTenantFromHeaders(t *testing.T) {
	g := newRig(&stubStore{definition: &domain.ControlTestDefinition{ControlTestDefinitionID: definitionID}}, nil, nil)
	// tester_principal_id is not even an accepted field — the acting
	// principal comes from X-Principal-Id.
	body := `{"period_start":"2026-01-01T00:00:00Z","period_end":"2026-03-31T00:00:00Z","result":"EFFECTIVE"}`
	expect(t, g.do(http.MethodPost, "/v1/control-test-definitions/"+definitionID+"/executions", body), http.StatusCreated, "")
	if g.store.createExecutionTester != testPrincipal || g.store.createExecutionTenant != testTenant {
		t.Errorf("expected tester %q in tenant %q, got %q in %q", testPrincipal, testTenant, g.store.createExecutionTester, g.store.createExecutionTenant)
	}
}

func TestCreateControlTestExecution_UnknownDefinition404(t *testing.T) {
	g := newRig(&stubStore{definitionErr: domain.ErrControlTestDefinitionNotFound}, nil, nil)
	body := `{"period_start":"2026-01-01T00:00:00Z","period_end":"2026-03-31T00:00:00Z","result":"EFFECTIVE"}`
	w := g.do(http.MethodPost, "/v1/control-test-definitions/"+definitionID+"/executions", body)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// Audit X10: a malformed id is the caller's mistake, answered 400 before it
// can reach a uuid comparison and come back as a 503.
func TestControlTestRoutes_MalformedIDs400(t *testing.T) {
	g := newRig(nil, nil, nil)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/control-test-definitions/ctd-1", ""},
		{http.MethodGet, "/v1/control-test-definitions/ctd-1/executions", ""},
		{http.MethodPost, "/v1/control-test-definitions/ctd-1/executions", `{"period_start":"2026-01-01T00:00:00Z","period_end":"2026-03-31T00:00:00Z","result":"EFFECTIVE"}`},
		{http.MethodGet, "/v1/attestations/att-1", ""},
		{http.MethodPost, "/v1/attestations/att-1/revoke", ""},
	} {
		w := g.do(c.method, c.path, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s: expected 400, got %d: %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if g.store.createExecutionCalls != 0 {
		t.Fatal("a malformed id must not reach the store")
	}
}

// Audit row "Control-test / attestation reads are scoped": every route needs
// a principal, a permission and the caller's tenant.
func TestControlTestReads_RequireIdentityTenantAndPermission(t *testing.T) {
	reads := []struct {
		path   string
		action string
	}{
		{"/v1/control-test-definitions/" + definitionID, authz.ActionControlTestDefinitionRead},
		{"/v1/control-test-definitions/" + definitionID + "/executions", authz.ActionControlTestExecutionRead},
		{"/v1/controls/CTRL-1/effectiveness", authz.ActionControlEffectivenessRead},
		{"/v1/attestations/" + attestationID, authz.ActionAttestationRead},
	}
	for _, rd := range reads {
		g := newRig(&stubStore{definition: &domain.ControlTestDefinition{}, attestation: &domain.Attestation{}}, nil, nil)
		if w := g.do(http.MethodGet, rd.path, "", noPrincipal); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without principal: expected 401, got %d", rd.path, w.Code)
		}
		if w := g.do(http.MethodGet, rd.path, "", noTenant); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without tenant: expected 401, got %d", rd.path, w.Code)
		}
		g = newRig(&stubStore{}, nil, &stubAuthz{err: domain.ErrAuthorizationDenied})
		if w := g.do(http.MethodGet, rd.path, ""); w.Code != http.StatusForbidden {
			t.Errorf("%s denied: expected 403, got %d", rd.path, w.Code)
		}
		if g.az.actionType != rd.action {
			t.Errorf("%s: expected %s, got %s", rd.path, rd.action, g.az.actionType)
		}
	}
}

// doc7 §E3: a control can be TESTED while its OPERATING_EFFECTIVENESS is
// INEFFECTIVE — the two fields never collapse into one status.
func TestGetControlEffectiveness_DesignAndOperatingAreIndependent(t *testing.T) {
	asOf := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	execID := "cte-9"
	g := newRig(&stubStore{effectiveness: &domain.ControlEffectiveness{
		ControlRef: "CTRL-SOD-01", DesignStatus: "TESTED", OperatingEffectiveness: "INEFFECTIVE",
		AsOf: &asOf, LatestExecutionID: &execID,
	}}, nil, nil)
	w := g.do(http.MethodGet, "/v1/controls/CTRL-SOD-01/effectiveness", "")
	expect(t, w, http.StatusOK, "")
	var got domain.ControlEffectiveness
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.DesignStatus != "TESTED" || got.OperatingEffectiveness != "INEFFECTIVE" {
		t.Fatalf("expected TESTED design status with an independently INEFFECTIVE operating result, got %+v", got)
	}
}

func TestGetControlEffectiveness_NoExecutionsRecorded(t *testing.T) {
	g := newRig(nil, nil, nil)
	w := g.do(http.MethodGet, "/v1/controls/CTRL-NEVER-TESTED/effectiveness", "")
	var got domain.ControlEffectiveness
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.DesignStatus != "NOT_TESTED" || got.OperatingEffectiveness != "NO_EXECUTIONS_RECORDED" {
		t.Fatalf("expected NOT_TESTED/NO_EXECUTIONS_RECORDED for an unknown control, got %+v", got)
	}
}

func TestCreateAttestation_SignerAndTenantFromHeaders(t *testing.T) {
	g := newRig(nil, nil, nil)
	body := `{"statement":"SoD controls operated as designed for Q1 2026","statement_version":"v1","subject_ref":"CTRL-SOD-01","period_start":"2026-01-01T00:00:00Z","period_end":"2026-03-31T00:00:00Z","signer_role":"CONTROL_OWNER"}`
	expect(t, g.do(http.MethodPost, "/v1/attestations", body), http.StatusCreated, "")
	if g.store.createAttestationSigner != testPrincipal || g.store.createAttestationTenant != testTenant {
		t.Errorf("expected signer %q in tenant %q, got %q in %q", testPrincipal, testTenant, g.store.createAttestationSigner, g.store.createAttestationTenant)
	}
}

func TestCreateAttestation_InvertedPeriod400(t *testing.T) {
	g := newRig(nil, nil, nil)
	body := `{"statement":"s","statement_version":"v1","subject_ref":"x","period_start":"2026-03-31T00:00:00Z","period_end":"2026-01-01T00:00:00Z","signer_role":"CFO"}`
	if w := g.do(http.MethodPost, "/v1/attestations", body); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an inverted period, got %d", w.Code)
	}
	if g.store.createAttestationCalls != 0 {
		t.Fatal("invalid input must not reach the store")
	}
}

func TestRevokeAttestation_InvalidTransitionConflict(t *testing.T) {
	g := newRig(&stubStore{revokeErr: domain.ErrInvalidAttestationTransition}, nil, nil)
	w := g.do(http.MethodPost, "/v1/attestations/"+attestationID+"/revoke", `{"reason":"superseded"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 revoking an already-terminal attestation, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRevokeAttestation_EmptyBodyAccepted(t *testing.T) {
	g := newRig(&stubStore{revoked: &domain.Attestation{AttestationID: attestationID, AttestationStatus: "REVOKED"}}, nil, nil)
	if w := g.do(http.MethodPost, "/v1/attestations/"+attestationID+"/revoke", ""); w.Code != http.StatusOK {
		t.Fatalf("the reason is optional, so an empty body must be accepted: got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateControlTestDefinition_AuthorizationDenied403(t *testing.T) {
	g := newRig(nil, nil, &stubAuthz{err: domain.ErrAuthorizationDenied})
	w := g.do(http.MethodPost, "/v1/control-test-definitions", `{"control_ref":"CTRL-X","test_code":"X-01","title":"t","methodology":"m"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if g.store.createDefinitionCalls != 0 {
		t.Errorf("expected the store to never be reached when authorization is denied, got %d calls", g.store.createDefinitionCalls)
	}
}
