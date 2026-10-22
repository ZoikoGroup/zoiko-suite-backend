package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/authz"
	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

// entityAuthz grants or denies per legal entity and records what was asked.
type entityAuthz struct {
	deny    map[string]bool
	failFor string
	asked   []authzCall
}

func (a *entityAuthz) CheckAllowed(_ context.Context, principalID, legalEntityID, action string) error {
	a.asked = append(a.asked, authzCall{principalID, legalEntityID, action})
	if legalEntityID == a.failFor && a.failFor != "" {
		return errors.New("authorization-svc unreachable")
	}
	if a.deny[legalEntityID] {
		return authz.ErrAuthorizationDenied
	}
	return nil
}

func listPops(t *testing.T, st *stubStore, az handler.AuthzChecker) *httptest.ResponseRecorder {
	t.Helper()
	gov, acc, wf, pub := scopeTestSources()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(st, gov, acc, wf, &stubWorkflowHistory{}, pub, az, zap.NewNop()))
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/engagements/eng-1/populations", nil)
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func popsFor(entities ...string) []*domain.AuditPopulation {
	var out []*domain.AuditPopulation
	for i, e := range entities {
		out = append(out, &domain.AuditPopulation{PopulationID: "pop-" + string(rune('a'+i)), LegalEntityID: e})
	}
	return out
}

func ids(t *testing.T, body []byte) []string {
	t.Helper()
	var got []domain.AuditPopulation
	require.NoError(t, json.Unmarshal(body, &got))
	var out []string
	for _, p := range got {
		out = append(out, p.PopulationID)
	}
	return out
}

func TestListAuditPopulations_AllAuthorized_ReturnsAll(t *testing.T) {
	st := newStubStore()
	st.populations = popsFor("e1", "e2", "e1")
	az := &entityAuthz{}
	w := listPops(t, st, az)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"pop-a", "pop-b", "pop-c"}, ids(t, w.Body.Bytes()))
	for _, c := range az.asked {
		assert.Equal(t, handler.AuditPopulationRead, c.action)
		assert.Equal(t, "principal-1", c.principal)
	}
	assert.Len(t, az.asked, 2, "one check per distinct legal entity")
}

// The reported bug: only pops[0] was checked, so a caller authorized for the
// first entity received every entity's populations.
func TestListAuditPopulations_SomeAuthorized_ReturnsOnlyThose_NoLeakage(t *testing.T) {
	st := newStubStore()
	st.populations = popsFor("e1", "e2", "e3", "e2")
	w := listPops(t, st, &entityAuthz{deny: map[string]bool{"e2": true}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"pop-a", "pop-c"}, ids(t, w.Body.Bytes()))
	assert.NotContains(t, w.Body.String(), "e2")
	assert.NotContains(t, w.Body.String(), "pop-b")
	assert.NotContains(t, w.Body.String(), "pop-d")
}

func TestListAuditPopulations_FirstDeniedLaterAllowed_StillFiltersPerEntity(t *testing.T) {
	st := newStubStore()
	st.populations = popsFor("e1", "e2")
	w := listPops(t, st, &entityAuthz{deny: map[string]bool{"e1": true}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"pop-b"}, ids(t, w.Body.Bytes()))
}

func TestListAuditPopulations_NoneAuthorized_Is403_NothingLeaked(t *testing.T) {
	st := newStubStore()
	st.populations = popsFor("e1", "e2")
	w := listPops(t, st, &entityAuthz{deny: map[string]bool{"e1": true, "e2": true}})
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.NotContains(t, w.Body.String(), "pop-")
	assert.NotContains(t, w.Body.String(), "e1")
}

func TestListAuditPopulations_Empty_ReturnsEmptyList_NoAuthzCallNeeded(t *testing.T) {
	st := newStubStore()
	az := &entityAuthz{deny: map[string]bool{"e1": true}}
	w := listPops(t, st, az)
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `[]`, w.Body.String())
	assert.Empty(t, az.asked)
}

func TestListAuditPopulations_AuthzUnavailable_FailsClosed(t *testing.T) {
	st := newStubStore()
	st.populations = popsFor("e1", "e2")
	w := listPops(t, st, &entityAuthz{failFor: "e2"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.NotContains(t, w.Body.String(), "pop-")
}

func TestListAuditPopulations_MissingPrincipal_Is401(t *testing.T) {
	st := newStubStore()
	st.populations = popsFor("e1")
	gov, acc, wf, pub := scopeTestSources()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, handler.New(st, gov, acc, wf, &stubWorkflowHistory{}, pub, &entityAuthz{}, zap.NewNop()))
	req := httptest.NewRequest(http.MethodGet, "/v1/audit/engagements/eng-1/populations", nil)
	req.Header.Set("X-Tenant-Id", "t1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}
