package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcenvelope "zoiko.io/evidence-requirements-svc/internal/envelope"
	"zoiko.io/evidence-requirements-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
)

// recordingAuthz records every scope and action it is asked about.
type recordingAuthz struct {
	deny  map[string]error // action → error
	asked []string         // "action@scope"
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, _, scope, action string) error {
	a.asked = append(a.asked, action+"@"+scope)
	return a.deny[action]
}

type stubRegistry struct{ err error }

func (r stubRegistry) CheckEntity(context.Context, string, string) error { return r.err }

// capturingStore keeps the outbox event the handler asked to write.
type capturingStore struct {
	*stubStore
	event *domain.OutboxEvent
}

func (c *capturingStore) RecordEvaluation(ctx context.Context, e *domain.EvidenceEvaluation, ev *domain.OutboxEvent) (bool, error) {
	c.event = ev
	return c.stubStore.RecordEvaluation(ctx, e, ev)
}

func completionRouter(store handler.Store, az handler.AuthZClient, docs handler.DocumentVaultClient, reg handler.EntityChecker, evalMode, readMode handler.GateMode) http.Handler {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeObserve, nil))
	handler.RegisterRoutes(r, handler.New(store, az, docs, zap.NewNop()).WithRegistry(reg).WithGateModes(evalMode, readMode))
	return r
}

func problemOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"), rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	for _, k := range []string{"type", "title", "status", "code"} {
		require.Contains(t, body, k)
	}
	return body
}

func TestEvaluate_IsAuthorizedAtTheEntity(t *testing.T) {
	st := &stubStore{evalCreated: true}
	az := &recordingAuthz{deny: map[string]error{"EVIDENCE_EVALUATE": domain.ErrAuthorizationDenied}}
	h := completionRouter(st, az, &stubDocs{}, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)

	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	body := problemOf(t, rec)
	assert.Equal(t, "authorization_denied", body["code"])
	assert.Equal(t, []string{"EVIDENCE_EVALUATE@" + testEntity}, az.asked)
	assert.Empty(t, st.recordedEvals, "a refused evaluation must not write a governance record")

	// warn mode: logged and admitted.
	h = completionRouter(st, az, &stubDocs{}, stubRegistry{}, handler.GateWarn, handler.GateEnforce)
	rec = do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestEvaluate_LegalEntityMustExistInTheTenant(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{domain.ErrLegalEntityNotFound, http.StatusUnprocessableEntity},
		{domain.ErrRegistryUnavailable, http.StatusServiceUnavailable},
	} {
		st := &stubStore{evalCreated: true}
		h := completionRouter(st, &recordingAuthz{}, &stubDocs{}, stubRegistry{err: tc.err}, handler.GateEnforce, handler.GateEnforce)
		rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal)
		assert.Equal(t, tc.code, rec.Code, rec.Body.String())
		assert.Empty(t, st.recordedEvals)
	}
}

func TestEvaluate_TheSameDocumentInAnotherCaseCountsOnce(t *testing.T) {
	st := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement(domain.EvidenceTypeSupportingDocument, domain.RequirementSpec{MinimumCount: 2}),
	}}
	docs := &stubDocs{}
	h := completionRouter(st, &recordingAuthz{}, docs, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)
	const doc = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	rec := do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeSupportingDocument, ReferenceID: strings.ToUpper(doc)},
		domain.PresentArtifact{EvidenceType: domain.EvidenceTypeSupportingDocument, ReferenceID: " " + doc + " "},
	), testTenant, testPrincipal)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, domain.OutcomeMissing, decodeEvaluate(t, rec).Outcome,
		"one document offered twice must not satisfy minimum_count: 2")
	assert.Equal(t, 1, docs.calls)

	// Non-document references are canonicalised the same way.
	st.effective = []domain.EvidenceRequirement{requirement("SIGNATURE", domain.RequirementSpec{MinimumCount: 2})}
	rec = do(t, h, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(
		domain.PresentArtifact{EvidenceType: "SIGNATURE", ReferenceID: "SIG-1"},
		domain.PresentArtifact{EvidenceType: "SIGNATURE", ReferenceID: "sig-1"},
	), testTenant, testPrincipal)
	assert.Equal(t, domain.OutcomeMissing, decodeEvaluate(t, rec).Outcome)
}

func TestEvaluate_EventCarriesTheGoverningReferences(t *testing.T) {
	inner := &stubStore{evalCreated: true, effective: []domain.EvidenceRequirement{
		requirement("SIGNATURE", domain.RequirementSpec{MinimumCount: 1}),
	}}
	st := &capturingStore{stubStore: inner}
	h := completionRouter(st, &recordingAuthz{}, &stubDocs{}, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)
	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(evaluateBody(domain.PresentArtifact{EvidenceType: "SIGNATURE", ReferenceID: "sig-1"})))
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence/evaluate", &buf)
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("Idempotency-Key", "k-1")
	req.Header.Set("X-Causation-Id", "cause-1")
	req.Header.Set("X-Jurisdiction-Context", "IN-KA")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, st.event)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(st.event.Payload, &payload))
	assert.Equal(t, "cause-1", payload["causation_id"])
	assert.Equal(t, "IN-KA", payload["jurisdiction_context"])
	assert.Equal(t, []any{"req-SIGNATURE"}, payload["requirement_ids"])
}

func TestEvaluate_AVerifiedPeerWorkloadIsTheCaller(t *testing.T) {
	st := &stubStore{evalCreated: true}
	az := &recordingAuthz{}
	h := completionRouter(st, az, &stubDocs{}, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)
	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(evaluateBody()))
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence/evaluate", &buf)
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("Idempotency-Key", "k-2")
	req.Header.Set("X-Identity-Basis", "mtls_peer") // set only by the identity gate
	req.Header.Set("X-Workload-Id", "board-resolutions-svc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, st.recordedEvals, 1)
	assert.Equal(t, "board-resolutions-svc", st.recordedEvals[0].EvaluatedForPrincipalID)

	// A workload header without the gate's peer basis is not an identity.
	req = httptest.NewRequest(http.MethodPost, "/v1/evidence/evaluate", bytes.NewBufferString(`{}`))
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Workload-Id", "anything")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.NotEqual(t, http.StatusOK, rec.Code)
}

func TestCreateRequirement_TenantWideIsAuthorizedAtPlatformScope(t *testing.T) {
	st := &stubStore{created: true}
	az := &recordingAuthz{}
	h := completionRouter(st, az, &stubDocs{}, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)
	body := map[string]any{"tenant_id": testTenant, "domain_code": "FINANCE", "action_type": "JOURNAL_POST",
		"evidence_type": "SIGNATURE", "correlation_id": "c", "requirement_payload": map[string]any{"minimum_count": 1}}
	rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements", body, testTenant, testPrincipal)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"EVIDENCE_REQUIREMENT_CREATE@PLATFORM"}, az.asked,
		"the tenant id is not an entity: a tenant-wide requirement authorizes at PLATFORM")
	assert.True(t, strings.HasPrefix(rec.Header().Get("Location"), "/v1/evidence-requirements/"))
}

func TestCreateRequirement_PayloadMustMatchTheRuleShape(t *testing.T) {
	for name, payload := range map[string]any{
		"wrong type":     map[string]any{"minimum_count": "two"},
		"unknown member": map[string]any{"minimum_cont": 2},
		"negative count": map[string]any{"minimum_count": -1},
		"not an object":  []int{1},
	} {
		t.Run(name, func(t *testing.T) {
			st := &stubStore{created: true}
			h := completionRouter(st, &recordingAuthz{}, &stubDocs{}, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)
			body := map[string]any{"tenant_id": testTenant, "domain_code": "FINANCE", "action_type": "JOURNAL_POST",
				"evidence_type": "SIGNATURE", "correlation_id": "c", "requirement_payload": payload}
			rec := do(t, h, http.MethodPost, "/v1/admin/evidence-requirements", body, testTenant, testPrincipal)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}

func TestReads_NeedAPrincipalAndAGrantAtTheEntity(t *testing.T) {
	eval := &domain.EvidenceEvaluation{EvaluationID: "e-1", TenantID: testTenant, LegalEntityID: testEntity}
	st := &stubStore{evaluation: eval}
	az := &recordingAuthz{deny: map[string]error{"EVIDENCE_EVALUATION_READ": domain.ErrAuthorizationDenied}}
	h := completionRouter(st, az, &stubDocs{}, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)

	rec := do(t, h, http.MethodGet, "/v1/evidence/evaluations/e-1", nil, testTenant, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = do(t, h, http.MethodGet, "/v1/evidence/evaluations/e-1", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, []string{"EVIDENCE_EVALUATION_READ@" + testEntity}, az.asked)

	az.asked = nil
	rec = do(t, h, http.MethodGet, "/v1/evidence-requirements?legal_entity_id="+testEntity, nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = do(t, h, http.MethodGet, "/v1/evidence-requirements", nil, testTenant, testPrincipal)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"EVIDENCE_REQUIREMENT_READ@" + testEntity, "EVIDENCE_REQUIREMENT_READ@PLATFORM"}, az.asked)
}
