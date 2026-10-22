package handler_test

// Governance audit, 21 Oct 2026 completion pass: read authorization, the
// approver checks (service accounts, rationale on REJECT), the legal-entity
// envelope match, input-format checks, idempotent re-invalidation, the
// escalate-to route and RFC 9457 error bodies.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/handler"

	"github.com/go-chi/chi/v5"

	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

const (
	testTenant = "11111111-1111-4111-8111-111111111111"
	testEntity = "22222222-2222-4222-8222-222222222222"
)

func readInstance() *domain.WorkflowInstance {
	return &domain.WorkflowInstance{WorkflowInstanceID: testWorkflowID, TenantID: testTenant, LegalEntityID: testEntity,
		WorkflowStatus: "PENDING", InitiatedBy: "requester-1", WorkflowType: "PURCHASE_APPROVAL", CurrentStage: 1}
}

func routerWithReadMode(s *stubStore, a *stubAuthz, mode string) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(s, &stubPublisher{}, a, &stubDocuments{}, zap.NewNop()).WithReadAuthz(mode)
	handler.RegisterRoutes(r, h)
	return r
}

func get(r http.Handler, path, principal string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, scopedAs(httptest.NewRequest(http.MethodGet, path, nil), principal))
	return w
}

// Reads were tenant-scoped only: any principal with a tenant header read the
// tenant's whole approval history. A non-participant now needs WORKFLOW_READ
// on the instance's legal entity.
func TestRead_NonParticipantNeedsWorkflowRead(t *testing.T) {
	a := &stubAuthz{err: domain.ErrAuthorizationDenied}
	s := &stubStore{findInstance: readInstance()}
	for _, path := range []string{"/v1/workflows/" + testWorkflowID, "/v1/workflows/" + testWorkflowID + "/next-approver",
		"/v1/workflows/" + testWorkflowID + "/evidence"} {
		a.asked, a.entities = nil, nil
		w := get(routerWithReadMode(s, a, ""), path, "stranger-1")
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403, got %d: %s", path, w.Code, w.Body.String())
		}
		if len(a.asked) != 1 || a.asked[0] != "WORKFLOW_READ" || a.entities[0] != testEntity {
			t.Fatalf("%s: asked %v at %v, want WORKFLOW_READ at the instance's entity", path, a.asked, a.entities)
		}
	}
}

func TestRead_ListsAskAtPlatformScope(t *testing.T) {
	a := &stubAuthz{err: domain.ErrAuthorizationDenied}
	for _, path := range []string{"/v1/workflows/pending", "/v1/workflow-definitions/"} {
		a.asked, a.entities = nil, nil
		w := get(routerWithReadMode(&stubStore{}, a, ""), path, "stranger-1")
		if w.Code != http.StatusForbidden || len(a.entities) != 1 || a.entities[0] != "PLATFORM" {
			t.Fatalf("%s: code %d, entities %v", path, w.Code, a.entities)
		}
	}
}

// The initiator and the current stage's approver read their own workflow
// without the grant, and without a call to authorization-svc.
func TestRead_ParticipantsReadWithoutTheGrant(t *testing.T) {
	for _, tc := range []struct {
		principal string
		onStage   bool
	}{{"requester-1", false}, {"approver-1", true}} {
		a := &stubAuthz{err: domain.ErrAuthorizationDenied}
		s := &stubStore{findInstance: readInstance(), currentStageApprover: tc.onStage}
		w := get(routerWithReadMode(s, a, ""), "/v1/workflows/"+testWorkflowID, tc.principal)
		if w.Code != http.StatusOK || len(a.asked) != 0 {
			t.Fatalf("%s: code %d, asked %v", tc.principal, w.Code, a.asked)
		}
	}
}

// warn serves the read (rollout while role templates are granted); off asks
// nothing.
func TestRead_WarnAndOffModes(t *testing.T) {
	a := &stubAuthz{err: domain.ErrAuthorizationDenied}
	if w := get(routerWithReadMode(&stubStore{findInstance: readInstance()}, a, "warn"), "/v1/workflows/"+testWorkflowID, "stranger-1"); w.Code != http.StatusOK {
		t.Fatalf("warn: expected 200, got %d", w.Code)
	}
	a.asked = nil
	if w := get(routerWithReadMode(&stubStore{findInstance: readInstance()}, a, "off"), "/v1/workflows/"+testWorkflowID, "stranger-1"); w.Code != http.StatusOK || len(a.asked) != 0 {
		t.Fatalf("off: code %d asked %v", w.Code, a.asked)
	}
}

func TestRead_AuthzUnavailableFailsClosed(t *testing.T) {
	a := &stubAuthz{err: domain.ErrAuthorizationServiceUnavailable}
	if w := get(routerWithReadMode(&stubStore{findInstance: readInstance()}, a, ""), "/v1/workflows/"+testWorkflowID, "stranger-1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func post(r http.Handler, path, principal, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := scopedAs(httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body)), principal)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Row 38: a rejection needs its reason, on both decision routes, and the
// refusal comes before authorization-svc or the store is asked anything.
func TestDecision_RejectNeedsARationale(t *testing.T) {
	for _, path := range []string{"/actions", "/quorum-votes"} {
		a := &stubAuthz{}
		s := &stubStore{findInstance: readInstance()}
		w := post(newTestRouterFull(s, &stubPublisher{}, a), "/v1/workflows/"+testWorkflowID+path, "approver-1", `{"action":"REJECT"}`, nil)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "rationale_required") {
			t.Fatalf("%s: expected 400 rationale_required, got %d: %s", path, w.Code, w.Body.String())
		}
		if len(a.entities) != 0 {
			t.Fatalf("%s: authorization-svc was asked before the input was refused", path)
		}
		w = post(newTestRouterFull(s, &stubPublisher{}, a), "/v1/workflows/"+testWorkflowID+path, "approver-1", `{"action":"REJECT","rationale":"   "}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: a blank rationale was accepted (%d)", path, w.Code)
		}
	}
}

// R-INV-08 (Q-5): a service or system principal cannot decide an approval.
func TestDecision_ServicePrincipalCannotDecide(t *testing.T) {
	for _, principal := range []string{"svc-payments", "system:workflow-svc", "SVC-Batch"} {
		a := &stubAuthz{}
		w := post(newTestRouterFull(&stubStore{findInstance: readInstance()}, &stubPublisher{}, a),
			"/v1/workflows/"+testWorkflowID+"/actions", principal, `{"action":"APPROVE"}`, nil)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "approver_not_human") {
			t.Fatalf("%s: expected 403 approver_not_human, got %d: %s", principal, w.Code, w.Body.String())
		}
	}
}

// Row 53/54: the body's legal entity must be a UUID and agree with the
// envelope's X-Legal-Entity-Id.
func TestCreate_LegalEntityChecks(t *testing.T) {
	r := newTestRouter(&stubStore{})
	body := validCreateBody()
	if w := post(r, "/v1/workflows", "requester-1", body, map[string]string{"X-Legal-Entity-Id": "33333333-3333-4333-8333-333333333333"}); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "legal_entity_mismatch") {
		t.Fatalf("mismatch: expected 400 legal_entity_mismatch, got %d: %s", w.Code, w.Body.String())
	}
	bad := strings.Replace(body, testEntity, "not-a-uuid", 1)
	if w := post(r, "/v1/workflows", "requester-1", bad, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("non-UUID entity: expected 400, got %d", w.Code)
	}
}

// Row 54: a non-UUID tenant is the caller's error, not a 503 store outage.
func TestTenantHeader_MustBeAUUID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/"+testWorkflowID, nil)
	req.Header.Set("X-Tenant-Id", "tenant-one")
	req.Header.Set("X-Principal-Id", "requester-1")
	w := httptest.NewRecorder()
	newTestRouter(&stubStore{findInstance: readInstance()}).ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestQuorumVote_PathIDChecked(t *testing.T) {
	if w := post(newTestRouter(&stubStore{}), "/v1/workflows/not-a-uuid/quorum-votes", "approver-1", `{"action":"APPROVE"}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// N-1: re-invalidating is the idempotent no-op it claims to be, not 409.
func TestInvalidate_AlreadyInvalidatedIsIdempotent(t *testing.T) {
	inst := readInstance()
	inst.WorkflowStatus = domain.WorkflowStatusInvalidated
	w := post(newTestRouter(&stubStore{findInstance: inst}), "/v1/workflows/"+testWorkflowID+"/invalidate", "svc-payables",
		`{"reason_code":"CONTROL_FAILURE"}`, map[string]string{"X-Expected-Version": "1"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// Row 28: escalate-to had a handler and no route.
func TestEscalateTo_IsRouted(t *testing.T) {
	inst := readInstance()
	s := &stubStore{findInstance: inst, escalateToInstance: inst, escalateToTransitioned: true}
	w := post(newTestRouter(s), "/v1/workflows/"+testWorkflowID+"/escalate-to", "admin-1",
		`{"escalation_target":"head-of-finance","reason_code":"SLA_BREACH"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// X9 / row 55: error bodies are RFC 9457 problem documents that keep the
// legacy "error" member.
func TestErrors_AreProblemJSONWithLegacyCode(t *testing.T) {
	a := &stubAuthz{err: domain.ErrAuthorizationDenied}
	w := get(routerWithReadMode(&stubStore{findInstance: readInstance()}, a, ""), "/v1/workflows/"+testWorkflowID, "stranger-1")
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "authorization_denied" || body["status"] != float64(403) || body["error_class"] != "AUTHORIZATION_DENIED" {
		t.Fatalf("body = %v", body)
	}
}

type stubRegistry struct{ err error }

func (s stubRegistry) CheckEntity(context.Context, string, string) error { return s.err }

func routerWithRegistry(s *stubStore, a *stubAuthz, reg stubRegistry) chi.Router {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, a, &stubDocuments{}, zap.NewNop()).WithRegistryChecker(reg))
	return r
}

// Row 6 / 53: the legal entity must exist in the caller's tenant; a registry
// that cannot answer fails closed.
func TestCreate_RegistryDecidesTheEntity(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{nil, http.StatusCreated}, {domain.ErrLegalEntityNotFound, http.StatusNotFound}, {domain.ErrRegistryUnavailable, http.StatusServiceUnavailable}} {
		s := &stubStore{instance: readInstance()}
		w := post(routerWithRegistry(s, &stubAuthz{}, stubRegistry{err: tc.err}), "/v1/workflows", "requester-1", validCreateBody(), nil)
		if w.Code != tc.want {
			t.Fatalf("registry %v: expected %d, got %d: %s", tc.err, tc.want, w.Code, w.Body.String())
		}
	}
}

// Row 13: a workflow started from a definition follows the definition's
// route, deadline, expiry and escalation target (it used to validate the
// definition and then ignore its stages).
func TestCreate_DefinitionSuppliesTheRoute(t *testing.T) {
	expiry, target := 48, "head-of-finance"
	def := &domain.WorkflowDefinition{WorkflowDefinitionID: "33333333-3333-4333-8333-333333333333", WorkflowType: "PURCHASE_APPROVAL",
		Version: 2, IsActive: true, ExpiryHours: &expiry, EscalationTarget: &target,
		StagesJSON: []byte(`[{"order":1,"stage_type":"SINGLE","approver_principal_id":"approver-1","due_at_hours":24},{"order":2,"stage_type":"ROLE","approver_role":"FINANCE_CONTROLLER"}]`)}
	s := &stubStore{instance: readInstance(), workflowDefinition: def}
	a := &stubAuthz{}
	body := `{"tenant_id":"` + testTenant + `","legal_entity_id":"` + testEntity + `","workflow_type":"PURCHASE_APPROVAL","workflow_definition_id":"` + def.WorkflowDefinitionID + `"}`
	w := post(newTestRouterFull(s, &stubPublisher{}, a), "/v1/workflows", "requester-1", body, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	p := s.createdParams
	if len(p.Stages) != 2 || p.Stages[0].ApproverPrincipalID != "approver-1" || p.Stages[0].DueAtHours != 24 ||
		p.Stages[1].StageType != domain.StageTypeRole || p.Stages[1].ApproverRole != "FINANCE_CONTROLLER" {
		t.Fatalf("stages = %+v, want the definition's route", p.Stages)
	}
	if p.WorkflowDefinitionVersion != 2 || p.EscalationTarget == nil || *p.EscalationTarget != target || p.DueAt == nil {
		t.Fatalf("pinning: version %d target %v due %v", p.WorkflowDefinitionVersion, p.EscalationTarget, p.DueAt)
	}
	// Only the named approver is checked at create; a ROLE stage names no one.
	if len(a.entities) != 1 {
		t.Fatalf("approver checks = %d, want 1", len(a.entities))
	}
}

// A ROLE stage is decided by a role holder, as such ("role"), and the
// stage's role travels to authorization-svc as an attribute.
func TestDecision_RoleStageHolderDecides(t *testing.T) {
	role := "FINANCE_CONTROLLER"
	s := &stubStore{findInstance: readInstance(),
		currentStage:   &domain.WorkflowStage{StageOrder: 2, StageType: domain.StageTypeRole, ApproverRole: &role},
		submitInstance: readInstance(), submitStage: &domain.WorkflowStage{StageOrder: 2}, submitTransitioned: true}
	w := post(newTestRouter(s), "/v1/workflows/"+testWorkflowID+"/actions", "controller-7", `{"action":"APPROVE"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.submitParams.ActingAs != "role" || s.submitParams.Authority.AccessDecisionID != "ad-1" {
		t.Fatalf("submit = %+v, want acting as role with the access decision", s.submitParams)
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["decision_id"] == nil || resp["decision_id"] == "" {
		t.Fatalf("no decision_id in %v", resp)
	}
}

// The escalation target of an ESCALATED instance decides as such; the
// delegation check is not asked.
func TestDecision_EscalationTargetDecides(t *testing.T) {
	inst := readInstance()
	inst.WorkflowStatus = domain.WorkflowStatusEscalated
	target := "head-of-finance"
	inst.EscalationTarget = &target
	s := &stubStore{findInstance: inst, currentStage: &domain.WorkflowStage{StageOrder: 1, ApproverPrincipalID: "approver-1"},
		submitInstance: inst, submitStage: &domain.WorkflowStage{StageOrder: 1}, submitTransitioned: true}
	w := post(newTestRouter(s), "/v1/workflows/"+testWorkflowID+"/actions", target, `{"action":"REJECT","rationale":"no budget"}`, nil)
	if w.Code != http.StatusOK || s.submitParams.ActingAs != "escalation" || s.submitParams.AssignedApproverID != "" {
		t.Fatalf("code %d submit %+v", w.Code, s.submitParams)
	}
}

// A delegate of the stage's approver decides with the delegation recorded.
func TestDecision_DelegateRecordsTheDelegation(t *testing.T) {
	s := &stubStore{findInstance: readInstance(), currentStage: &domain.WorkflowStage{StageOrder: 1, ApproverPrincipalID: "approver-1"},
		submitInstance: readInstance(), submitStage: &domain.WorkflowStage{StageOrder: 1}, submitTransitioned: true}
	w := post(newTestRouter(s), "/v1/workflows/"+testWorkflowID+"/actions", "cover-1", `{"action":"APPROVE"}`, nil)
	if w.Code != http.StatusOK || s.submitParams.AssignedApproverID != "approver-1" || s.submitParams.Authority.DelegationID != "del-1" {
		t.Fatalf("code %d submit %+v", w.Code, s.submitParams)
	}
}

// Commands carry the Idempotency-Key and a request hash to the store; the
// same body hashes the same, a different body differently.
func TestDecision_IdempotencyKeyAndHashReachTheStore(t *testing.T) {
	hashOf := func(body string) domain.CommandMeta {
		s := &stubStore{findInstance: readInstance(), currentStage: &domain.WorkflowStage{StageOrder: 1, ApproverPrincipalID: "approver-1"},
			submitInstance: readInstance(), submitStage: &domain.WorkflowStage{StageOrder: 1}}
		post(newTestRouter(s), "/v1/workflows/"+testWorkflowID+"/actions", "approver-1", body, map[string]string{"Idempotency-Key": "k-1"})
		return s.submitParams.Meta
	}
	a, b, c := hashOf(`{"action":"APPROVE"}`), hashOf(`{"action":"APPROVE"}`), hashOf(`{"action":"REJECT","rationale":"x"}`)
	if a.IdempotencyKey != "k-1" || a.RequestHash == "" || a.RequestHash != b.RequestHash || a.RequestHash == c.RequestHash {
		t.Fatalf("meta a=%+v b=%+v c=%+v", a, b, c)
	}
	if a.DecisionID == "" || a.DecisionID == b.DecisionID {
		t.Fatal("each command needs its own ledger decision id")
	}
}
