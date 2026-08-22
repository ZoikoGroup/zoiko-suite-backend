package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/ai-governance-svc/internal/domain"
)

// TestTenantScopedRoutes_RefuseMissingTenant proves the tenant-identity
// fix at the HTTP layer.
//
// Before this change the tenant came from the caller's own request body or
// ?tenant_id= query string, so these routes did not need X-Tenant-Id at
// all — the entire existing test suite ran without ever sending it and
// passed. That is what made the defect invisible.
func TestTenantScopedRoutes_RefuseMissingTenant(t *testing.T) {
	r := newTestRouter(newTestHandler())

	for _, tc := range []struct {
		name, method, path string
		body               interface{}
	}{
		{"create ai run", http.MethodPost, "/v1/ai-runs", domain.CreateAIRunRequest{
			RunType: "RECOMMEND", ModelID: "claude-5", PromptVersion: "v3", AuditID: "audit-001",
		}},
		{"get ai run", http.MethodGet, "/v1/ai-runs/some-id", nil},
		{"create automation policy", http.MethodPost, "/v1/automation-policies", domain.CreateAutomationPolicyRequest{
			Role: "FINANCE_AGENT", Tool: "LEDGER_POST", ActionType: "POST_JOURNAL",
		}},
		{"resolve automation policy", http.MethodGet,
			"/v1/automation-policies/resolve?role=FINANCE_AGENT&tool=LEDGER_POST&action_type=POST_JOURNAL", nil},
		{"propose automation action", http.MethodPost, "/v1/automation-actions", domain.ProposeAutomationActionRequest{
			ActionType: "POST_JOURNAL", Role: "FINANCE_AGENT", Tool: "LEDGER_POST", IdempotencyKey: "idem-1",
		}},
		{"get automation action", http.MethodGet, "/v1/automation-actions/some-id", nil},
		{"decide automation action", http.MethodPost, "/v1/automation-actions/some-id/decision",
			domain.ApproveAutomationActionRequest{Decision: "APPROVED"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequestAs(tc.method, tc.path, tc.body, ""))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-Id, got %d — %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestPlatformScopedRoutes_WorkWithoutTenant is the other half of the same
// claim, and the reason the 401 lives in the handlers rather than in
// middleware. These three tables carry no tenant_id — doc7 §G2 makes the
// risk taxonomy shared, §G6 the provider registry platform-wide, and §G3
// says policy changes "alter governance truth across tenants". A blanket
// tenant check would have broken them, and inventing a tenant column to
// make the schema look uniform would contradict the doc.
func TestPlatformScopedRoutes_WorkWithoutTenant(t *testing.T) {
	r := newTestRouter(newTestHandler())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/action-risk-classifications",
		domain.SetActionRiskClassificationRequest{
			ActionType: "POST_JOURNAL", RiskCategory: "HIGH", RequiresMakerChecker: true,
		}, ""))
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("platform route must not require X-Tenant-Id, got %d — %s", w.Code, w.Body.String())
	}

	wProv := httptest.NewRecorder()
	r.ServeHTTP(wProv, buildRequestAs(http.MethodPost, "/v1/model-providers",
		domain.RegisterModelProviderRequest{
			ProviderName: "anthropic", ModelName: "claude-opus-5", DataRegion: "eu-west-1",
		}, ""))
	if wProv.Code != http.StatusOK && wProv.Code != http.StatusCreated {
		t.Fatalf("platform route must not require X-Tenant-Id, got %d — %s", wProv.Code, wProv.Body.String())
	}
}

// TestDeclaredTenantMustMatchHeader covers the compatibility path: the
// tenant_id field stays in the API, but a value disagreeing with the
// verified header is refused rather than silently resolved in either
// direction. Trusting the body was the original defect; silently ignoring
// it would leave a caller believing it acted on a tenant it did not.
func TestDeclaredTenantMustMatchHeader(t *testing.T) {
	r := newTestRouter(newTestHandler())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/automation-policies",
		domain.CreateAutomationPolicyRequest{
			TenantID: testTenantB, Role: "FINANCE_AGENT", Tool: "LEDGER_POST", ActionType: "POST_JOURNAL",
		}, testTenantA))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when body tenant_id disagrees with X-Tenant-Id, got %d — %s", w.Code, w.Body.String())
	}

	// Same rule on the query-string form.
	wq := httptest.NewRecorder()
	r.ServeHTTP(wq, buildRequestAs(http.MethodGet,
		"/v1/automation-policies/resolve?tenant_id="+testTenantB+
			"&role=FINANCE_AGENT&tool=LEDGER_POST&action_type=POST_JOURNAL", nil, testTenantA))
	if wq.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when ?tenant_id= disagrees with X-Tenant-Id, got %d — %s", wq.Code, wq.Body.String())
	}
}

// TestCannotCreateAllowlistInAnotherTenant is the defect in its original
// form: tenant B allowlists an autonomous action, then tenant A resolves
// the same action and must still be NOT_ALLOWLISTED. Doc7 §G7 makes the
// allowlist per-tenant precisely so that agentic execution is "a
// controlled execution model, not broad delegated authority" — a caller
// that can name the tenant it is allowlisting in has that authority.
func TestCannotCreateAllowlistInAnotherTenant(t *testing.T) {
	r := newTestRouter(newTestHandler())

	// Tenant B creates an allowlist entry (legitimately, in its own tenant).
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/automation-policies",
		domain.CreateAutomationPolicyRequest{
			Role: "FINANCE_AGENT", Tool: "LEDGER_POST", ActionType: "POST_JOURNAL",
		}, testTenantB))
	if w.Code != http.StatusCreated {
		t.Fatalf("tenant B must be able to allowlist in its own tenant, got %d — %s", w.Code, w.Body.String())
	}

	// Tenant A resolving the same action must fail closed.
	wRes := httptest.NewRecorder()
	r.ServeHTTP(wRes, buildRequestAs(http.MethodGet,
		"/v1/automation-policies/resolve?role=FINANCE_AGENT&tool=LEDGER_POST&action_type=POST_JOURNAL",
		nil, testTenantA))
	if wRes.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wRes.Code, wRes.Body.String())
	}
	var res domain.AutomationPolicyResolution
	if err := json.NewDecoder(wRes.Body).Decode(&res); err != nil {
		t.Fatalf("decode resolution: %v", err)
	}
	if res.Allowed {
		t.Fatal("ISOLATION FAILURE: tenant A resolved ALLOWED against tenant B's autonomy allowlist")
	}
	if res.ReasonCode != "NOT_ALLOWLISTED" {
		t.Fatalf("expected fail-closed NOT_ALLOWLISTED, got %+v", res)
	}
}

// TestCannotApproveAnotherTenantsAutomationAction covers the most
// consequential write: approving an automation action is what authorizes
// an autonomous action to execute (doc7 §G2/§G7). A cross-tenant approval
// grants agentic authority inside someone else's tenant.
func TestCannotApproveAnotherTenantsAutomationAction(t *testing.T) {
	r := newTestRouter(newTestHandler())

	// A HIGH-risk classification so the proposal lands PENDING.
	wClass := httptest.NewRecorder()
	r.ServeHTTP(wClass, buildRequestAs(http.MethodPost, "/v1/action-risk-classifications",
		domain.SetActionRiskClassificationRequest{
			ActionType: "POST_JOURNAL", RiskCategory: "HIGH", RequiresMakerChecker: true,
		}, ""))
	if wClass.Code != http.StatusOK && wClass.Code != http.StatusCreated {
		t.Fatalf("set classification: got %d — %s", wClass.Code, wClass.Body.String())
	}

	// Tenant A allowlists and proposes.
	wPol := httptest.NewRecorder()
	r.ServeHTTP(wPol, buildRequestAs(http.MethodPost, "/v1/automation-policies",
		domain.CreateAutomationPolicyRequest{
			Role: "FINANCE_AGENT", RiskCategory: "HIGH", Tool: "LEDGER_POST", ActionType: "POST_JOURNAL",
		}, testTenantA))
	if wPol.Code != http.StatusCreated {
		t.Fatalf("create allowlist as tenant A: got %d — %s", wPol.Code, wPol.Body.String())
	}

	wProp := httptest.NewRecorder()
	r.ServeHTTP(wProp, buildRequestAs(http.MethodPost, "/v1/automation-actions",
		domain.ProposeAutomationActionRequest{
			ActionType: "POST_JOURNAL", Role: "FINANCE_AGENT", Tool: "LEDGER_POST",
			IdempotencyKey: "idem-a-1", PreconditionsMet: true,
		}, testTenantA))
	if wProp.Code != http.StatusCreated {
		t.Fatalf("propose as tenant A: got %d — %s", wProp.Code, wProp.Body.String())
	}
	var action domain.AutomationAction
	if err := json.NewDecoder(wProp.Body).Decode(&action); err != nil {
		t.Fatalf("decode proposed action: %v", err)
	}
	if action.ApprovalStatus != domain.ApprovalPending {
		t.Fatalf("expected PENDING so there is something to forge an approval on, got %s", action.ApprovalStatus)
	}

	// Tenant B tries to approve it.
	decReq := buildRequestAs(http.MethodPost,
		"/v1/automation-actions/"+action.AutomationActionID+"/decision",
		domain.ApproveAutomationActionRequest{Decision: "APPROVED"}, testTenantB)
	decReq.Header.Set("X-Principal-Id", "principal-b-checker")
	wDec := httptest.NewRecorder()
	r.ServeHTTP(wDec, decReq)
	if wDec.Code != http.StatusNotFound {
		t.Fatalf("ISOLATION FAILURE: tenant B got %d approving tenant A's autonomous action — %s",
			wDec.Code, wDec.Body.String())
	}

	// And it must still be PENDING for tenant A.
	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, buildRequestAs(http.MethodGet,
		"/v1/automation-actions/"+action.AutomationActionID, nil, testTenantA))
	if wGet.Code != http.StatusOK {
		t.Fatalf("tenant A must still read its own action, got %d — %s", wGet.Code, wGet.Body.String())
	}
	if bytes.Contains(wGet.Body.Bytes(), []byte("principal-b-checker")) {
		t.Fatalf("ISOLATION FAILURE: tenant A's action records tenant B's principal as approver: %s",
			wGet.Body.String())
	}
}

// TestCannotReadAnotherTenantsAIRun covers the read side: an AI run
// carries model_id, prompt_version, source_refs, evidence_refs and
// recommended_action — how a governed decision was reached and the
// evidence it rested on.
func TestCannotReadAnotherTenantsAIRun(t *testing.T) {
	r := newTestRouter(newTestHandler())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/ai-runs", domain.CreateAIRunRequest{
		RunType: "RECOMMEND", ModelID: "claude-opus-5", PromptVersion: "v3",
		AuditID: "audit-a-001", SourceRefs: []string{"src-tenant-a-confidential"},
	}, testTenantA))
	if w.Code != http.StatusCreated {
		t.Fatalf("create as tenant A: got %d — %s", w.Code, w.Body.String())
	}
	var run domain.AIRun
	if err := json.NewDecoder(w.Body).Decode(&run); err != nil {
		t.Fatalf("decode ai run: %v", err)
	}

	wB := httptest.NewRecorder()
	r.ServeHTTP(wB, buildRequestAs(http.MethodGet, "/v1/ai-runs/"+run.AIRunID, nil, testTenantB))
	if wB.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's ai run, got %d — %s", wB.Code, wB.Body.String())
	}
	if bytes.Contains(wB.Body.Bytes(), []byte("src-tenant-a-confidential")) {
		t.Fatalf("ISOLATION FAILURE: tenant B saw tenant A's source_refs: %s", wB.Body.String())
	}
}
