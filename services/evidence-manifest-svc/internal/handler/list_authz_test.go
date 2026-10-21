package handler_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

// GET /v1/evidence-manifests with no legal_entity_id used to list every
// manifest header in the tenant with no authorization at all.

func listManifests(t *testing.T, st *stubStore, az handler.AuthzChecker, tenant, query string) *httptest.ResponseRecorder {
	t.Helper()
	gov, acc, wf, pub := scopeTestSources()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(st, gov, acc, wf, &stubWorkflowHistory{}, pub, az, zap.NewNop()))
	req := httptest.NewRequest(http.MethodGet, "/v1/evidence-manifests"+query, nil)
	req.Header.Set("X-Tenant-Id", tenant)
	req.Header.Set("X-Principal-Id", "principal-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func manifestsFixture() *stubStore {
	st := newStubStore()
	st.manifests = map[string]*domain.EvidenceManifest{
		"m1": {ManifestID: "m1", TenantID: "t1", LegalEntityID: "e1"},
		"m2": {ManifestID: "m2", TenantID: "t1", LegalEntityID: "e2"},
		"m3": {ManifestID: "m3", TenantID: "t1", LegalEntityID: "e1"},
		"x1": {ManifestID: "x1", TenantID: "t2", LegalEntityID: "e9"},
	}
	return st
}

func manifestIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var got []domain.EvidenceManifest
	require.NoError(t, json.Unmarshal(body, &got))
	out := []string{}
	for _, m := range got {
		out = append(out, m.ManifestID)
	}
	return out
}

func TestListManifests_NoEntity_Unauthorized_IsRefused(t *testing.T) {
	az := &entityAuthz{deny: map[string]bool{"e1": true, "e2": true}}
	w := listManifests(t, manifestsFixture(), az, "t1", "")
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), "m1", "nothing about the manifests is disclosed")
	assert.ElementsMatch(t, []string{"e1", "e2"}, askedEntities(az), "each entity was actually asked")
	for _, c := range az.asked {
		assert.Equal(t, handler.EvidenceManifestRead, c.action, "the existing permission is reused")
	}
}

func TestListManifests_NoEntity_AuthorizationOutage_FailsClosed(t *testing.T) {
	w := listManifests(t, manifestsFixture(), &entityAuthz{failFor: "e2"}, "t1", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotContains(t, w.Body.String(), "m1")
	_ = errors.New
}

func TestListManifests_NoEntity_OnlyReadableEntitiesAreListed(t *testing.T) {
	w := listManifests(t, manifestsFixture(), &entityAuthz{deny: map[string]bool{"e2": true}}, "t1", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.ElementsMatch(t, []string{"m1", "m3"}, manifestIDs(t, w.Body.Bytes()), "e2's manifest is omitted, not named")
}

func TestListManifests_NoEntity_AllAuthorized_ListsTheTenantOnly(t *testing.T) {
	w := listManifests(t, manifestsFixture(), &entityAuthz{}, "t1", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.ElementsMatch(t, []string{"m1", "m2", "m3"}, manifestIDs(t, w.Body.Bytes()), "tenant t2's manifest is never listed")

	w = listManifests(t, manifestsFixture(), &entityAuthz{}, "t2", "")
	assert.Equal(t, []string{"x1"}, manifestIDs(t, w.Body.Bytes()))
}

func TestListManifests_NoEntity_EmptyTenant_ReturnsEmptyList(t *testing.T) {
	w := listManifests(t, manifestsFixture(), &entityAuthz{deny: map[string]bool{"e1": true}}, "t-empty", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "[]", trimmed(w.Body.String()))
}

func TestListManifests_EntityFilter_StillAuthorizesAndFilters(t *testing.T) {
	az := &entityAuthz{}
	w := listManifests(t, manifestsFixture(), az, "t1", "?legal_entity_id=e1")
	require.Equal(t, http.StatusOK, w.Code)
	assert.ElementsMatch(t, []string{"m1", "m3"}, manifestIDs(t, w.Body.Bytes()))
	assert.Equal(t, []string{"e1"}, askedEntities(az))

	w = listManifests(t, manifestsFixture(), &entityAuthz{deny: map[string]bool{"e1": true}}, "t1", "?legal_entity_id=e1")
	assert.Equal(t, http.StatusForbidden, w.Code)
	w = listManifests(t, manifestsFixture(), &entityAuthz{failFor: "e1"}, "t1", "?legal_entity_id=e1")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func askedEntities(az *entityAuthz) []string {
	var out []string
	for _, c := range az.asked {
		out = append(out, c.entity)
	}
	return out
}

func trimmed(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
