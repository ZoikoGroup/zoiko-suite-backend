package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-transfer-svc/internal/authz"
	"zoiko.io/privacy-transfer-svc/internal/domain"
	"zoiko.io/privacy-transfer-svc/internal/handler"
	"zoiko.io/privacy-transfer-svc/internal/middleware"
	"zoiko.io/privacy-transfer-svc/internal/purposeregistry"
)

// recAuthz records every authorization question and can deny one action.
type recAuthz struct {
	denyAction string
	calls      []authzCall
}

type authzCall struct{ Principal, Scope, Action string }

func (a *recAuthz) CheckAllowed(_ context.Context, principal, scope, action string) error {
	a.calls = append(a.calls, authzCall{principal, scope, action})
	if a.denyAction != "" && a.denyAction == action {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func rawRequest(r http.Handler, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	var buf strings.Builder
	if body != nil {
		b, _ := json.Marshal(body)
		buf.Write(b)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(buf.String()))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func withHeaders(r http.Handler, method, path string, body interface{}, extra map[string]string) *httptest.ResponseRecorder {
	h := map[string]string{"X-Principal-Id": "principal-01", "X-Tenant-Id": testTenant}
	if method != http.MethodGet {
		h["X-Correlation-ID"] = "corr-" + uuid.NewString()
		h["Idempotency-Key"] = "idem-" + uuid.NewString()
	}
	for k, v := range extra {
		h[k] = v
	}
	return rawRequest(r, method, path, body, h)
}

// ── Gap 2: read routes are authenticated, tenant-scoped and authorized ──────

func TestReads_RequirePrincipalAndTenant(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	paths := []string{
		"/privacy/processor-relationships",
		"/privacy/processor-relationships/" + rel.RelationshipID,
		"/privacy/processor-relationships/" + rel.RelationshipID + "/subprocessors",
		"/privacy/transfer-mechanisms/" + uuid.NewString(),
		"/privacy/transfer-assessments?relationship_id=" + rel.RelationshipID,
		"/privacy/transfer-decisions/" + uuid.NewString(),
		"/privacy/transfer-assessments/triggers?relationship_id=" + rel.RelationshipID,
		"/v1/privacy/processor-relationships",
	}
	for _, p := range paths {
		w := rawRequest(r, http.MethodGet, p, nil, map[string]string{"X-Tenant-Id": testTenant})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without a principal must be 401, got %d", p, w.Code)
		}
		w = rawRequest(r, http.MethodGet, p, nil, map[string]string{"X-Principal-Id": "p"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without a tenant must be 401, got %d", p, w.Code)
		}
	}
	// The trigger-evaluation POST is authenticated too.
	w := rawRequest(r, http.MethodPost, "/privacy/transfer-assessments/evaluate-triggers", map[string]string{"relationship_id": rel.RelationshipID}, map[string]string{"X-Tenant-Id": testTenant})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("POST evaluate-triggers without a principal must be 401, got %d", w.Code)
	}
}

func TestReads_AuthorizedBeforeLookup_UsingExistingActions(t *testing.T) {
	st := newStubStore()
	az := &recAuthz{}
	r := newTestRouter(st, &stubPublisher{}, az, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	missing := uuid.NewString()

	cases := []struct {
		name, path, action string
	}{
		{"relationship", "/privacy/processor-relationships/" + rel.RelationshipID, handler.PrivacyTransferRelationshipManage},
		{"relationship missing", "/privacy/processor-relationships/" + missing, handler.PrivacyTransferRelationshipManage},
		{"list", "/privacy/processor-relationships", handler.PrivacyTransferRelationshipManage},
		{"subprocessors", "/privacy/processor-relationships/" + rel.RelationshipID + "/subprocessors", handler.PrivacyTransferRelationshipManage},
		{"mechanism", "/privacy/transfer-mechanisms/" + missing, handler.PrivacyTransferMechanismManage},
		{"assessment", "/privacy/transfer-assessments?relationship_id=" + rel.RelationshipID, handler.PrivacyTransferAssessmentRecord},
		{"decision", "/privacy/transfer-decisions/" + missing, handler.PrivacyTransferDecisionEvaluate},
		{"triggers", "/privacy/transfer-assessments/triggers?relationship_id=" + rel.RelationshipID, handler.PrivacyTransferDecisionEvaluate},
	}
	for _, c := range cases {
		az.denyAction = c.action
		az.calls = nil
		w := withHeaders(r, http.MethodGet, c.path, nil, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: unauthorized read must be 403 (also for missing ids), got %d %s", c.name, w.Code, w.Body.String())
		}
		if len(az.calls) != 1 || az.calls[0].Action != c.action || az.calls[0].Scope != testTenant {
			t.Fatalf("%s: wrong authorization question: %+v", c.name, az.calls)
		}
		az.denyAction = ""
		w = withHeaders(r, http.MethodGet, c.path, nil, nil)
		if c.name == "relationship missing" && w.Code != http.StatusNotFound {
			t.Fatalf("authorized read of a missing relationship must be 404, got %d", w.Code)
		}
	}
}

func TestReads_CrossTenantIsInvisible(t *testing.T) {
	r := newTestRouter(newStubStore(), &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)
	other := map[string]string{"X-Tenant-Id": "another-tenant"}
	if w := withHeaders(r, http.MethodGet, "/privacy/processor-relationships/"+rel.RelationshipID, nil, other); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant relationship read must be 404, got %d", w.Code)
	}
	if w := withHeaders(r, http.MethodGet, "/privacy/transfer-mechanisms/"+mech.MechanismID, nil, other); w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant mechanism read must be 404, got %d", w.Code)
	}
	w := withHeaders(r, http.MethodGet, "/privacy/processor-relationships", nil, other)
	var list struct{ Count int }
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Count != 0 {
		t.Fatalf("cross-tenant list must be empty, got %d", list.Count)
	}
}

func TestMutations_AuthorizedBeforeLookup_NoExistenceLeak(t *testing.T) {
	az := &recAuthz{}
	r := newTestRouter(newStubStore(), &stubPublisher{}, az, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	az.denyAction = handler.PrivacyTransferRelationshipManage
	for _, id := range []string{rel.RelationshipID, uuid.NewString()} {
		w := withHeaders(r, http.MethodPost, "/privacy/processor-relationships/"+id+"/status", domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipInactive}, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status update on %s must be 403, got %d", id, w.Code)
		}
		w = withHeaders(r, http.MethodPost, "/privacy/processor-relationships/"+id+"/subprocessors", domain.AttachSubprocessorRequest{ProviderIdentity: "p", Service: "s"}, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("subprocessor attach on %s must be 403, got %d", id, w.Code)
		}
	}
	az.denyAction = handler.PrivacyTransferAssessmentRecord
	for _, id := range []string{rel.RelationshipID, uuid.NewString()} {
		w := withHeaders(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{RelationshipID: id, Outcome: domain.AssessmentApprove}, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("assessment on %s must be 403, got %d", id, w.Code)
		}
	}
}

// ── Gap 3: idempotency ───────────────────────────────────────────────────────

func TestIdempotency_LookupFailureRefusesRequest(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	st.failLookup = true
	w := withHeaders(r, http.MethodPost, "/privacy/transfer-mechanisms", domain.CreateTransferMechanismRequest{MechanismType: "SCC"}, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a failed idempotency lookup must refuse the request, got %d", w.Code)
	}
	if st.execs != 0 || len(st.mechanisms) != 0 {
		t.Fatal("nothing may execute when the idempotency lookup fails")
	}
}

func TestIdempotency_CommitFailureLeavesNothing_RetryCreatesOneAndOneEvent(t *testing.T) {
	st := newStubStore()
	pub := &stubPublisher{}
	r := newTestRouter(st, pub, &stubAuthz{}, newStubPurposeRegistry())
	hdr := map[string]string{"Idempotency-Key": "k-atomic"}
	body := domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s"}

	st.failCommit = true
	if w := withHeaders(r, http.MethodPost, "/privacy/processor-relationships", body, hdr); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
	if len(st.relationships) != 0 || len(st.idempotency) != 0 || pub.calls != 0 {
		t.Fatal("a failed commit must persist no relationship, no idempotency record and no event")
	}
	st.failCommit = false
	if w := withHeaders(r, http.MethodPost, "/privacy/processor-relationships", body, hdr); w.Code != http.StatusCreated {
		t.Fatalf("retry must succeed, got %d", w.Code)
	}
	w := withHeaders(r, http.MethodPost, "/privacy/processor-relationships", body, hdr)
	if w.Header().Get("Idempotency-Replay") != "true" || len(st.relationships) != 1 || pub.calls != 1 {
		t.Fatalf("replay must not re-execute: replay=%q relationships=%d events=%d", w.Header().Get("Idempotency-Replay"), len(st.relationships), pub.calls)
	}
}

func TestIdempotency_AliasHeaderHonoured_BoundToRoute_ScopedByPrincipal(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	body := domain.CreateTransferMechanismRequest{MechanismType: "SCC"}
	alias := map[string]string{"Idempotency-Key": "", "X-Idempotency-Key": "alias-1", "X-Correlation-ID": "c"}
	w1 := withHeaders(r, http.MethodPost, "/privacy/transfer-mechanisms", body, alias)
	w2 := withHeaders(r, http.MethodPost, "/privacy/transfer-mechanisms", body, alias)
	if w1.Code != http.StatusCreated || w2.Header().Get("Idempotency-Replay") != "true" || len(st.mechanisms) != 1 {
		t.Fatalf("X-Idempotency-Key must dedupe: %d replay=%q mechanisms=%d", w1.Code, w2.Header().Get("Idempotency-Replay"), len(st.mechanisms))
	}
	// Same key, different operation: conflict, never a replay of the wrong answer.
	w3 := withHeaders(r, http.MethodPost, "/privacy/processor-relationships",
		domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s"}, alias)
	if w3.Code != http.StatusConflict {
		t.Fatalf("same key on a different operation must be 409, got %d", w3.Code)
	}
	// /v1 alias of the same request is a replay.
	w4 := withHeaders(r, http.MethodPost, "/v1/privacy/transfer-mechanisms", body, alias)
	if w4.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("the /v1 alias of the same request must replay, got %d", w4.Code)
	}
	// Another principal with the same key is a different claim.
	p2 := map[string]string{"Idempotency-Key": "", "X-Idempotency-Key": "alias-1", "X-Principal-Id": "principal-02"}
	w5 := withHeaders(r, http.MethodPost, "/privacy/transfer-mechanisms", body, p2)
	if w5.Code != http.StatusCreated || w5.Header().Get("Idempotency-Replay") == "true" || len(st.mechanisms) != 2 {
		t.Fatalf("principal B must not replay principal A's response: %d mechanisms=%d", w5.Code, len(st.mechanisms))
	}
}

// ── Gap 4: reassessment triggers ─────────────────────────────────────────────

func setupAssessed(t *testing.T, st *stubStore) (http.Handler, *recAuthz, *domain.ProcessorRelationship, *domain.TransferMechanism) {
	t.Helper()
	az := &recAuthz{}
	r := newTestRouter(st, &stubPublisher{}, az, newStubPurposeRegistry())
	w := doRequest(r, http.MethodPost, "/privacy/processor-relationships", domain.CreateProcessorRelationshipRequest{
		ControllerRef: "c", ProcessorRef: "p", Service: "svc", ProcessingInstructions: "instructions",
		DataCategories: []string{"CONTACT_INFO"}, Jurisdictions: []string{"EU"}, SubjectClasses: []string{"CUSTOMER"},
	}, testTenant)
	var rel domain.ProcessorRelationship
	_ = json.Unmarshal(w.Body.Bytes(), &rel)
	mech := createValidMechanism(t, r)
	time.Sleep(5 * time.Millisecond)
	if w := doRequest(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{
		RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove,
	}, testTenant); w.Code != http.StatusCreated {
		t.Fatalf("assessment: %d %s", w.Code, w.Body.String())
	}
	return r, az, &rel, mech
}

func evalReq(rel *domain.ProcessorRelationship, mech *domain.TransferMechanism) domain.EvaluateTransferRequest {
	return domain.EvaluateTransferRequest{RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID, AssessmentRequired: true}
}

func evaluate(t *testing.T, r http.Handler, req domain.EvaluateTransferRequest) (int, domain.TransferDecision) {
	t.Helper()
	w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", req, testTenant)
	var d domain.TransferDecision
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return w.Code, d
}

func TestTriggers_SensitiveCategoryUsesTheTaxonomy(t *testing.T) {
	r, _, rel, mech := setupAssessed(t, newStubStore())
	// SEX_LIFE_ORIENTATION and TRADE_UNION were missed by the old substring match.
	for _, cat := range []string{"SEX_LIFE_ORIENTATION", "TRADE_UNION", "child_data", "HEALTH"} {
		req := evalReq(rel, mech)
		req.DataCategories = []string{cat}
		code, d := evaluate(t, r, req)
		if code != http.StatusOK || d.Result != domain.ResultReviewRequired || !strings.Contains(strings.Join(d.ReasonCodes, ","), "NEW_PURPOSE_OR_SENSITIVE_CATEGORY") {
			t.Fatalf("new sensitive category %s must require reassessment, got %d %s %v", cat, code, d.Result, d.ReasonCodes)
		}
	}
	// A registered category, or a non-sensitive one, does not trigger.
	req := evalReq(rel, mech)
	req.DataCategories = []string{"CONTACT_INFO", "LOCATION"}
	if _, d := evaluate(t, r, req); d.Result != domain.ResultAuthorized {
		t.Fatalf("non-sensitive categories must not trigger, got %s %v", d.Result, d.ReasonCodes)
	}
}

func TestTriggers_SubprocessorAddedAfterAssessmentRequiresReassessment(t *testing.T) {
	r, _, rel, mech := setupAssessed(t, newStubStore())
	time.Sleep(5 * time.Millisecond)
	if w := doRequest(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors",
		domain.AttachSubprocessorRequest{ProviderIdentity: "new-sub", Service: "hosting", NotificationApprovalModel: "PRIOR_APPROVAL"}, testTenant); w.Code != http.StatusCreated {
		t.Fatalf("attach: %d", w.Code)
	}
	code, d := evaluate(t, r, evalReq(rel, mech))
	if code != http.StatusOK || d.Result != domain.ResultReviewRequired || !strings.Contains(strings.Join(d.ReasonCodes, ","), "NEW_SUBPROCESSOR") {
		t.Fatalf("a subprocessor added after the assessment was never assessed: got %s %v", d.Result, d.ReasonCodes)
	}
	// The trigger endpoint agrees with the evaluation.
	w := withHeaders(r, http.MethodGet, "/privacy/transfer-assessments/triggers?relationship_id="+rel.RelationshipID, nil, nil)
	var resp domain.EvaluateTriggersResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.ReassessmentNeeded || len(resp.TriggerEvaluations) != 8 {
		t.Fatalf("trigger endpoint disagrees: %+v", resp)
	}
}

func TestTriggers_DeclaredTriggerIsValidated(t *testing.T) {
	r, _, rel, mech := setupAssessed(t, newStubStore())
	req := evalReq(rel, mech)
	req.ReassessmentTrigger = "ANYTHING_I_LIKE"
	if w := doRequest(r, http.MethodPost, "/privacy/transfer-decisions", req, testTenant); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown declared trigger must be 400, got %d", w.Code)
	}
	w := withHeaders(r, http.MethodGet, "/privacy/transfer-assessments/triggers?relationship_id="+rel.RelationshipID+"&declared_trigger=NOPE", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown declared_trigger must be 400, got %d", w.Code)
	}
	// The three triggers whose facts live in other systems are caller-declared only.
	for _, trig := range []string{domain.TriggerAutomatedDecisioning, domain.TriggerMaterialArchitectureChange, domain.TriggerSecurityPrivacyIncident} {
		req := evalReq(rel, mech)
		req.ReassessmentTrigger = trig
		if _, d := evaluate(t, r, req); d.Result != domain.ResultReviewRequired {
			t.Fatalf("declared trigger %s must require reassessment, got %s", trig, d.Result)
		}
	}
}

func TestTriggers_EndpointAuthorizedAndFailsClosed(t *testing.T) {
	st := newStubStore()
	r, az, rel, _ := setupAssessed(t, st)
	az.denyAction = handler.PrivacyTransferDecisionEvaluate
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		path := "/privacy/transfer-assessments/triggers?relationship_id=" + rel.RelationshipID
		if m == http.MethodPost {
			path = "/privacy/transfer-assessments/evaluate-triggers"
		}
		w := withHeaders(r, m, path, map[string]string{"relationship_id": rel.RelationshipID}, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s triggers must be authorized, got %d", m, w.Code)
		}
	}
}

// ── Gap 7: events ────────────────────────────────────────────────────────────

func TestEvents_OnlyCommittedChangesEmit_WithContext(t *testing.T) {
	st := newStubStore()
	pub := &stubPublisher{}
	r := newTestRouter(st, pub, &stubAuthz{}, newStubPurposeRegistry())
	hdr := map[string]string{"X-Correlation-ID": "corr-ev", "X-Principal-Id": "actor-7"}

	w := withHeaders(r, http.MethodPost, "/privacy/processor-relationships",
		domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s", ProcessingInstructions: "i"}, hdr)
	var rel domain.ProcessorRelationship
	_ = json.Unmarshal(w.Body.Bytes(), &rel)
	if pub.calls != 1 || pub.last.EventType != "privacy.processor.changed" || pub.last.TenantID != testTenant ||
		pub.last.ActorID != "actor-7" || pub.last.CorrelationID != "corr-ev" || pub.last.EntityID != rel.RelationshipID {
		t.Fatalf("processor.changed wrong: %+v", pub.last)
	}
	withHeaders(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/status", domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipInactive}, hdr)
	withHeaders(r, http.MethodPost, "/privacy/processor-relationships/"+rel.RelationshipID+"/subprocessors", domain.AttachSubprocessorRequest{ProviderIdentity: "x", Service: "y"}, hdr)
	if pub.calls != 3 {
		t.Fatalf("expected 3 processor.changed events, got %d", pub.calls)
	}
	// An assessment emits assessment.expiring only when it declares a review trigger.
	withHeaders(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove}, hdr)
	if pub.calls != 3 {
		t.Fatalf("an assessment without a review trigger must not emit, got %d events", pub.calls)
	}
	at := time.Now().Add(24 * time.Hour)
	withHeaders(r, http.MethodPost, "/privacy/transfer-assessments", domain.RecordTransferAssessmentRequest{RelationshipID: rel.RelationshipID, Outcome: domain.AssessmentApprove, ReviewTriggerAt: &at}, hdr)
	if !pub.published("privacy.assessment.expiring") {
		t.Fatal("assessment.expiring was not emitted")
	}
	// A refused operation emits nothing.
	before := pub.calls
	withHeaders(r, http.MethodPost, "/privacy/processor-relationships/"+uuid.NewString()+"/status", domain.UpdateRelationshipStatusRequest{Status: domain.RelationshipInactive}, hdr)
	if pub.calls != before {
		t.Fatal("a refused operation emitted an event")
	}
}

func TestEvents_DecisionEmitsAuthorizationChangedOnceAndNotOnReplay(t *testing.T) {
	st := newStubStore()
	pub := &stubPublisher{}
	r := newTestRouter(st, pub, &stubAuthz{}, newStubPurposeRegistry())
	rel := createRelationship(t, r)
	mech := createValidMechanism(t, r)
	before := pub.calls
	hdr := map[string]string{"Idempotency-Key": "ev-dec", "X-Correlation-ID": "corr-d"}
	req := domain.EvaluateTransferRequest{RelationshipID: rel.RelationshipID, TransferMechanismID: mech.MechanismID}
	w1 := withHeaders(r, http.MethodPost, "/privacy/transfer-decisions", req, hdr)
	w2 := withHeaders(r, http.MethodPost, "/privacy/transfer-decisions", req, hdr)
	if w1.Code != http.StatusOK || w2.Header().Get("Idempotency-Replay") != "true" {
		t.Fatalf("got %d replay=%q", w1.Code, w2.Header().Get("Idempotency-Replay"))
	}
	if pub.calls-before != 1 || pub.last.EventType != "privacy.transfer.authorization_changed" || pub.last.CorrelationID != "corr-d" {
		t.Fatalf("expected exactly one authorization_changed event with the correlation id, got %d %+v", pub.calls-before, pub.last)
	}
	if w1.Body.String() != w2.Body.String() && strings.TrimSpace(w1.Body.String()) != strings.TrimSpace(w2.Body.String()) {
		t.Fatalf("replay must return the original decision body")
	}
}

// ── registry client forwards the caller's identity ──────────────────────────

func TestPurposeRegistryClient_ForwardsTenantAndPrincipal(t *testing.T) {
	var gotTenant, gotPrincipal, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant, gotPrincipal, gotPath = r.Header.Get("X-Tenant-Id"), r.Header.Get("X-Principal-Id"), r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"activity_version_id":"av","activity_id":"a","version_status":"ACTIVE"}`))
	}))
	defer srv.Close()
	c := purposeregistry.NewClient(srv.URL)
	v, err := c.ResolveActivity(context.Background(), "tenant-9", "principal-9", "activity/with space")
	if err != nil || v == nil || v.VersionStatus != "ACTIVE" {
		t.Fatalf("resolve: %v %v", v, err)
	}
	if gotTenant != "tenant-9" || gotPrincipal != "principal-9" {
		t.Fatalf("identity not forwarded: tenant=%q principal=%q", gotTenant, gotPrincipal)
	}
	if !strings.HasSuffix(gotPath, "activity%2Fwith%20space") {
		t.Fatalf("activity id must be path-escaped, got %s", gotPath)
	}
}

func TestCreateRelationship_ForwardsCallerPrincipalToRegistry(t *testing.T) {
	var seen string
	reg := &capturingRegistry{onResolve: func(principal string) { seen = principal }}
	st := newStubStore()
	h := handler.New(st, &stubAuthz{}, reg, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	w := withHeaders(r, http.MethodPost, "/privacy/processor-relationships",
		domain.CreateProcessorRelationshipRequest{ControllerRef: "c", ProcessorRef: "p", Service: "s", PurposeActivityRefs: []string{"act-1"}},
		map[string]string{"X-Principal-Id": "caller-77"})
	if w.Code != http.StatusCreated || seen != "caller-77" {
		t.Fatalf("expected the caller's principal to reach the registry, got %d %q", w.Code, seen)
	}
}

type capturingRegistry struct{ onResolve func(principal string) }

func (c *capturingRegistry) ResolveActivity(_ context.Context, _, principal, _ string) (*purposeregistry.ActivityVersion, error) {
	c.onResolve(principal)
	return &purposeregistry.ActivityVersion{VersionStatus: "ACTIVE"}, nil
}

func TestTenantMismatchBodyRefused(t *testing.T) {
	st := newStubStore()
	r := newTestRouter(st, &stubPublisher{}, &stubAuthz{}, newStubPurposeRegistry())
	w := doRequest(r, http.MethodPost, "/privacy/transfer-mechanisms", domain.CreateTransferMechanismRequest{TenantID: "someone-else", MechanismType: "SCC"}, testTenant)
	if w.Code != http.StatusForbidden || len(st.mechanisms) != 0 {
		t.Fatalf("got %d mechanisms=%d", w.Code, len(st.mechanisms))
	}
}
