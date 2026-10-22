package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/authz"
	"zoiko.io/evidence-manifest-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

// recordingAuthz remembers every authorization question asked and denies the
// principals listed in deny.
type recordingAuthz struct {
	deny  map[string]bool
	calls []authzCall
}

type authzCall struct{ principal, entity, action string }

func (a *recordingAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, actionType string) error {
	a.calls = append(a.calls, authzCall{principalID, legalEntityID, actionType})
	if a.deny[principalID] {
		return authz.ErrAuthorizationDenied
	}
	return nil
}

func principalRouter(t *testing.T, az handler.AuthzChecker) (chi.Router, *stubStore) {
	t.Helper()
	return principalRouterWith(t, newStubStore(), az)
}

func principalRouterWith(t *testing.T, st *stubStore, az handler.AuthzChecker) (chi.Router, *stubStore) {
	t.Helper()
	gov, acc, wf, pub := scopeTestSources()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(st, gov, acc, wf, &stubWorkflowHistory{}, pub, az, zap.NewNop()))
	return r, st
}

func postGenerate(r http.Handler, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "t1")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// ── Gap 1: the verified principal is the only principal ──────────────────────

func TestPrincipal_NormalPrincipalWorks_AndIsTheAuthzSubjectAndRequestedBy(t *testing.T) {
	az := &recordingAuthz{}
	r, st := principalRouter(t, az)

	w := postGenerate(r, generateBody(t, "t1"), map[string]string{"X-Principal-Id": "principal-real"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	require.Len(t, az.calls, 1)
	assert.Equal(t, "principal-real", az.calls[0].principal)
	assert.Equal(t, "e1", az.calls[0].entity)
	assert.Equal(t, handler.EvidenceManifestGenerate, az.calls[0].action)
	assert.Equal(t, "principal-real", st.manifests["manifest-1"].RequestedBy)
}

func TestPrincipal_ConflictingActorHeader_CannotImpersonate(t *testing.T) {
	az := &recordingAuthz{deny: map[string]bool{"principal-real": true}}
	r, st := principalRouter(t, az)

	// principal-real is NOT allowed; the attacker names an allowed actor.
	w := postGenerate(r, generateBody(t, "t1"), map[string]string{
		"X-Principal-Id":       "principal-real",
		"X-Actor-Principal-ID": "principal-admin",
	})
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Len(t, az.calls, 1)
	assert.Equal(t, "principal-real", az.calls[0].principal, "authz must be asked about the verified principal")
	assert.Empty(t, st.manifests, "nothing may be created for a denied principal")
}

func TestPrincipal_ActorHeaderAlone_IsNotAPrincipal(t *testing.T) {
	az := &recordingAuthz{}
	r, st := principalRouter(t, az)

	w := postGenerate(r, generateBody(t, "t1"), map[string]string{"X-Actor-Principal-ID": "principal-admin"})
	assert.Equal(t, http.StatusUnauthorized, w.Code, "missing verified principal must fail closed")
	assert.Empty(t, az.calls)
	assert.Empty(t, st.manifests)
}

func TestPrincipal_ActorHeaderDoesNotChangeRequestedBy(t *testing.T) {
	r, st := principalRouter(t, &recordingAuthz{})
	w := postGenerate(r, generateBody(t, "t1"), map[string]string{
		"X-Principal-Id":       "principal-real",
		"X-Actor-Principal-ID": "principal-other",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	assert.Equal(t, "principal-real", st.manifests["manifest-1"].RequestedBy)
}

func TestPrincipal_BodyRequestedByCannotNameSomeoneElse(t *testing.T) {
	az := &recordingAuthz{}
	r, st := principalRouter(t, az)

	var m map[string]any
	require.NoError(t, json.Unmarshal(generateBody(t, "t1"), &m))
	m["requested_by"] = "someone-else"
	body, _ := json.Marshal(m)

	w := postGenerate(r, body, map[string]string{"X-Principal-Id": "principal-real"})
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.Empty(t, st.manifests)

	// An agreeing value is harmless.
	m["requested_by"] = "principal-real"
	body, _ = json.Marshal(m)
	w = postGenerate(r, body, map[string]string{"X-Principal-Id": "principal-real"})
	assert.Equal(t, http.StatusCreated, w.Code, w.Body.String())
}

// Every route that reads a principal must ignore the actor header — a denied
// verified principal stays denied however the header is set.
func TestPrincipal_ActorHeaderIgnoredOnEveryRoute(t *testing.T) {
	az := &recordingAuthz{deny: map[string]bool{"principal-real": true}}
	r, _ := principalRouter(t, az)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/evidence-manifests?legal_entity_id=e1"},
		{http.MethodGet, "/v1/evidence-manifests/some-id"},
		{http.MethodGet, "/v1/evidence-manifests/some-id/records"},
		{http.MethodPost, "/v1/evidence-manifests/some-id/verify"},
		{http.MethodGet, "/v1/evidence-manifests/some-id/download"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("X-Tenant-Id", "t1")
			req.Header.Set("X-Principal-Id", "principal-real")
			req.Header.Set("X-Actor-Principal-ID", "principal-admin")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.NotEqual(t, http.StatusOK, w.Code)
			for _, c := range az.calls {
				assert.NotEqual(t, "principal-admin", c.principal)
			}
		})
	}
}

// ── Gap 2: tenant → principal → authorization → validation ───────────────────

func TestGenerateOrder_MissingPrincipal_Is401_EvenForMalformedBody(t *testing.T) {
	az := &recordingAuthz{}
	r, _ := principalRouter(t, az)
	w := postGenerate(r, []byte(`{not json`), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, az.calls)
}

func TestGenerateOrder_BlankPrincipal_Is401(t *testing.T) {
	r, _ := principalRouter(t, &recordingAuthz{})
	w := postGenerate(r, generateBody(t, "t1"), map[string]string{"X-Principal-Id": ""})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestGenerateOrder_UnauthorizedPrincipal_Is403(t *testing.T) {
	az := &recordingAuthz{deny: map[string]bool{"principal-denied": true}}
	r, st := principalRouter(t, az)
	w := postGenerate(r, generateBody(t, "t1"), map[string]string{"X-Principal-Id": "principal-denied"})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Empty(t, st.manifests)
}

// The point of the reorder: an unauthorized caller sending a request that
// would fail validation gets 403, not a 400 describing what is wrong with it.
func TestGenerateOrder_MalformedRequestFromUnauthorizedCaller_Is403Not400(t *testing.T) {
	az := &recordingAuthz{deny: map[string]bool{"principal-denied": true}}
	r, _ := principalRouter(t, az)

	for name, body := range map[string]string{
		"invalid scenario":  `{"tenant_id":"t1","legal_entity_id":"e1","scenario_type":"NOPE"}`,
		"no records":        `{"tenant_id":"t1","legal_entity_id":"e1","scenario_type":"AUDIT"}`,
		"tenant mismatch":   `{"tenant_id":"other","legal_entity_id":"e1","scenario_type":"AUDIT","governance_decision_ids":["g"]}`,
		"wrong field types": `{"legal_entity_id":"e1","governance_decision_ids":"not-a-list"}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := postGenerate(r, []byte(body), map[string]string{"X-Principal-Id": "principal-denied"})
			assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assert.NotContains(t, w.Body.String(), "invalid_scenario_type")
			assert.NotContains(t, w.Body.String(), "no_records_requested")
			assert.NotContains(t, w.Body.String(), "tenant_mismatch")
		})
	}
}

func TestGenerateOrder_MalformedRequestFromAuthorizedCaller_Is400(t *testing.T) {
	r, st := principalRouter(t, &recordingAuthz{})
	for name, tc := range map[string]struct{ body, code string }{
		"invalid scenario": {`{"tenant_id":"t1","legal_entity_id":"e1","scenario_type":"NOPE","governance_decision_ids":["g"]}`, "invalid_scenario_type"},
		"no records":       {`{"tenant_id":"t1","legal_entity_id":"e1","scenario_type":"AUDIT"}`, "no_records_requested"},
		"missing entity":   {`{"tenant_id":"t1","scenario_type":"AUDIT","governance_decision_ids":["g"]}`, "missing_field"},
		"not json":         {`{nope`, "invalid_json"},
	} {
		t.Run(name, func(t *testing.T) {
			w := postGenerate(r, []byte(tc.body), map[string]string{"X-Principal-Id": "principal-ok"})
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tc.code)
		})
	}
	assert.Empty(t, st.manifests)
}

func TestGenerateOrder_TenantMismatch_AfterAuthorization_Is403(t *testing.T) {
	az := &recordingAuthz{}
	r, st := principalRouter(t, az)
	body := []byte(`{"tenant_id":"other","legal_entity_id":"e1","scenario_type":"AUDIT","governance_decision_ids":["g"]}`)
	w := postGenerate(r, body, map[string]string{"X-Principal-Id": "principal-ok"})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "tenant_mismatch")
	assert.Len(t, az.calls, 1, "authorization precedes body validation")
	assert.Empty(t, st.manifests)
}

func TestGenerateOrder_MissingTenant_Is401BeforeAnything(t *testing.T) {
	az := &recordingAuthz{}
	r, _ := principalRouter(t, az)
	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(generateBody(t, "t1")))
	req.Header.Set("X-Principal-Id", "principal-ok")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, az.calls)
}
