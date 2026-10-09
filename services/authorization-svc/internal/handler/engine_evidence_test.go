package handler_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// Second-pass gaps, decided by the documents: one decision engine (GOV-03 #4),
// the §8.2 decision and the GOV-03 / §20 evidence, the entity negative
// control, fail-closed session lookups, and restricted explanation (GOV-03,
// §25).

const evTenant = "11111111-1111-4111-8111-111111111111"
const evEntity = "22222222-2222-4222-8222-222222222222"

func evAuthorize(t *testing.T, s *stubStore, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewBufferString(body))
	req.Header.Set("X-Tenant-Id", evTenant)
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

// §8.2 / §20: the response and the record carry the canonical decision, the
// recorded policy version, reason codes and the evidence fields.
func TestEvidence_AuthorizeCarriesCanonicalDecision(t *testing.T) {
	s := &stubStore{rbacActions: []string{"payment.release"}, rbacBasis: "rbac:role=TREASURY",
		granting: []domain.GrantingAssignment{{AssignmentID: "a-7", RoleID: "r-1", RoleCode: "TREASURY"}}}
	code, got := evAuthorize(t, s, `{"principal_id":"p-1","legal_entity_id":"`+evEntity+`","action_type":"payment.release",
		"resource_type":"payment_instruction","resource_id":"pi-9","resource_version":"18",
		"attributes":{"amount":"250000.00","currency":"GBP"}}`)
	if code != http.StatusOK || got["decision_outcome"] != "GRANTED" || got["decision"] != "PERMIT" {
		t.Fatalf("want 200 GRANTED/PERMIT, got %d %v", code, got)
	}
	if got["policy_set_version"] != "cfg.42" || got["expires_at"] == nil {
		t.Errorf("policy_set_version / expires_at missing: %v", got)
	}
	if codes, _ := got["reason_codes"].([]any); len(codes) == 0 {
		t.Errorf("reason_codes empty: %v", got)
	}
	p := s.recordedParams
	if p.Decision != "PERMIT" || p.ResourceType != "payment_instruction" || p.ResourceID != "pi-9" || p.ResourceVersion != "18" {
		t.Errorf("recorded evidence incomplete: %+v", p)
	}
	if !strings.HasPrefix(p.AttributesDigest, "sha256:") || strings.Contains(p.AttributesDigest, "250000") {
		t.Errorf("attributes must be recorded as a digest, not values: %q", p.AttributesDigest)
	}
	// §20 assignment references: the exact assignment that granted, not the
	// role-code basis string (S9-1: usage is attributed per assignment).
	if len(p.MatchedGrants) != 2 || p.MatchedGrants[0] != "assignment:a-7" || p.MatchedGrants[1] != "role:TREASURY" || p.ExpiresAt == nil {
		t.Errorf("matched_grants / expires_at not recorded: %+v", p)
	}
}

// §11 attribution: a delegated grant records whose authority, by which delegation.
func TestEvidence_DelegatedDecisionAttributed(t *testing.T) {
	s := &stubStore{
		delegatedActions: []string{"payment.approve"}, delegatedBasis: "delegated:from=p-boss",
		delegationSourceDelegator: "p-boss", delegationSourceID: "da-7",
	}
	if code, got := evAuthorize(t, s, `{"principal_id":"p-cover","legal_entity_id":"`+evEntity+`","action_type":"payment.approve"}`); code != 200 || got["decision_outcome"] != "GRANTED" {
		t.Fatalf("want GRANTED, got %d %v", code, got)
	}
	if s.recordedParams.OnBehalfOf != "p-boss" || s.recordedParams.DelegationID != "da-7" {
		t.Fatalf("delegated decision not attributed: on_behalf_of=%q delegation_id=%q", s.recordedParams.OnBehalfOf, s.recordedParams.DelegationID)
	}
}

// Entity standing is a negative control: DISSOLVED / SUSPENDED deny; DORMANT
// permits with an obligation.
func TestEngine_EntityStatusNegativeControl(t *testing.T) {
	for _, st := range []string{"DISSOLVED", "SUSPENDED"} {
		s := &stubStore{rbacActions: []string{"journal.post"}, entityStatus: st}
		_, got := evAuthorize(t, s, `{"principal_id":"p-1","legal_entity_id":"`+evEntity+`","action_type":"journal.post"}`)
		if got["decision_outcome"] != "DENIED" || got["decision_basis"] != "entity_status:"+st {
			t.Errorf("%s entity: want DENIED entity_status:%s, got %v", st, st, got)
		}
	}
	s := &stubStore{rbacActions: []string{"journal.post"}, entityStatus: "DORMANT"}
	_, got := evAuthorize(t, s, `{"principal_id":"p-1","legal_entity_id":"`+evEntity+`","action_type":"journal.post"}`)
	obl, _ := got["obligations"].([]any)
	if got["decision_outcome"] != "GRANTED" || len(obl) == 0 || obl[0] != "ENTITY_DORMANT_REVIEW" {
		t.Errorf("DORMANT entity: want GRANTED with ENTITY_DORMANT_REVIEW, got %v", got)
	}
}

// A session store failure is a 503 on every entry point — the canonical engine
// used to ignore it and simply not grant.
func TestEngine_SessionLookupFailureFailsClosed(t *testing.T) {
	s := &stubStore{privilegedSessionErr: errors.New("db down")}
	code, _ := evAuthorize(t, s, `{"principal_id":"p-1","legal_entity_id":"`+evEntity+`","action_type":"x.y","privileged_session_id":"ps-1"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", code)
	}
}

// GOV-03 #4: available actions equal backend-authorized actions. Both sides of
// a held SoD pair were listed as available; /v1/authorize denies them.
func TestEngine_AvailableActionsEqualAuthorized(t *testing.T) {
	s := &sodRuleStore{
		stubStore: &stubStore{rbacActions: []string{"payment.prepare", "payment.release", "report.view"}},
		pairs:     [][2]string{{"payment.prepare", "payment.release"}},
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/payment_instruction/pi-1/available-actions?legal_entity_id="+evEntity, nil)
	req.Header.Set("X-Principal-Id", "p-1")
	req.Header.Set("X-Tenant-Id", evTenant)
	w := httptest.NewRecorder()
	routerFor(s).ServeHTTP(w, req)
	var resp domain.AvailableActionsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || len(resp.AvailableActions) != 1 || resp.AvailableActions[0] != "report.view" {
		t.Fatalf("want only report.view available, got %d %v (denied %v)", w.Code, resp.AvailableActions, resp.DeniedActions)
	}
	if len(resp.DeniedActions) != 2 {
		t.Errorf("both conflicting actions should be listed as denied: %v", resp.DeniedActions)
	}
}

// The principal and tenant come from the verified headers; asking about
// somebody else needs iam.assignment.read, and /v1/me is always the caller.
func TestReadSurfaces_NoQueryImpersonation(t *testing.T) {
	s := &stubStore{denyAdmin: true, rbacActions: []string{"payment.release"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/payment_instruction/pi-1/available-actions?principal_id=cfo-1&legal_entity_id="+evEntity, nil)
	req.Header.Set("X-Principal-Id", "clerk-1")
	req.Header.Set("X-Tenant-Id", evTenant)
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("available-actions for another principal without iam.assignment.read: want 403, got %d", w.Code)
	}

	for _, path := range []string{"/v1/me/capabilities?principal_id=cfo-1&tenant_id=" + evTenant, "/v1/payment_instruction/pi-1/available-actions?principal_id=cfo-1&tenant_id=" + evTenant} {
		w := httptest.NewRecorder()
		newTestRouter(s).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with identity only in the query: want 401, got %d", path, w.Code)
		}
	}
}

// Explanation access is restricted: without iam.policy.read a caller sees only
// their own decisions, explained by reason code.
func TestExplanation_RestrictedWithoutPolicyRead(t *testing.T) {
	own := domain.AccessDecisionLog{AccessDecisionID: "d-1", PrincipalID: "clerk-1", DecisionOutcome: "DENIED",
		DecisionBasis: "sod:conflict_with=payment.prepare", ReasonCodes: []string{"SOD_STATIC_CONFLICT"}, MatchedGrants: []string{"rbac:role=X"}}
	s := &stubStore{denyAdmin: true, listDecisions: &domain.AccessDecisionPage{Decisions: []domain.AccessDecisionLog{own}}}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Principal-Id", "clerk-1")
		req.Header.Set("X-Tenant-Id", evTenant)
		w := httptest.NewRecorder()
		newTestRouter(s).ServeHTTP(w, req)
		return w
	}

	w := get("/v1/access-decisions")
	if w.Code != http.StatusOK || s.gotListDecisionsParams.PrincipalID != "clerk-1" {
		t.Fatalf("own listing: want 200 scoped to the caller, got %d principal=%q", w.Code, s.gotListDecisionsParams.PrincipalID)
	}
	if strings.Contains(w.Body.String(), "payment.prepare") || strings.Contains(w.Body.String(), "rbac:role") {
		t.Errorf("internal basis leaked to a reader without iam.policy.read: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SOD_STATIC_CONFLICT") {
		t.Errorf("reason codes must remain: %s", w.Body.String())
	}
	if w := get("/v1/access-decisions?principal_id=cfo-1"); w.Code != http.StatusForbidden {
		t.Errorf("another principal's decisions: want 403, got %d", w.Code)
	}

	other := own
	other.PrincipalID = "cfo-1"
	s.findDecision = &other
	if w := get("/v1/access-decisions/d-1"); w.Code != http.StatusNotFound {
		t.Errorf("another principal's decision by id: want 404, got %d", w.Code)
	}
}

func TestExplanation_PolicyReaderSeesEverything(t *testing.T) {
	d := domain.AccessDecisionLog{AccessDecisionID: "d-1", PrincipalID: "cfo-1", DecisionBasis: "sod:conflict_with=payment.prepare"}
	s := &stubStore{findDecision: &d}
	req := httptest.NewRequest(http.MethodGet, "/v1/access-decisions/d-1", nil)
	req.Header.Set("X-Principal-Id", "auditor-1")
	req.Header.Set("X-Tenant-Id", evTenant)
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "payment.prepare") {
		t.Fatalf("holder of iam.policy.read: want the full decision, got %d %s", w.Code, w.Body.String())
	}
}

// routerFor builds the production router over any store.
func routerFor(s handler.AuthorizationStore) chi.Router {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop()))
	return r
}

// The canonical decision API is an evaluation, not a material write: it needs
// no Idempotency-Key, and the idempotency middleware must never replay it.
func TestCanonicalDecisions_NotAMaterialWrite(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/internal/authorization/decisions", nil)
	if handler.MaterialWrite(req) {
		t.Fatal("POST /internal/authorization/decisions classified as a material write")
	}
	if !handler.MaterialWrite(httptest.NewRequest(http.MethodPost, "/v1/admin/sod-exceptions", nil)) {
		t.Fatal("an admin command must stay a material write")
	}
}
