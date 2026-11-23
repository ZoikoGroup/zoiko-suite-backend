package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/compliance-status-svc/internal/authz"
	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/middleware"
	"zoiko.io/compliance-status-svc/internal/store"
)

type recordingAuthz struct {
	err     error
	actions []string
	scopes  []string
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, _, scope, action string) error {
	a.actions = append(a.actions, action)
	a.scopes = append(a.scopes, scope)
	return a.err
}

func hRouter(st store.Store, az AuthZClient) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.TenantContextMiddleware)
	RegisterRoutes(r, New(st, az, zap.NewNop()))
	return r
}

func call(t *testing.T, h http.Handler, method, path, principal, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Tenant-Id", "t1")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func codeOf(w *httptest.ResponseRecorder) string {
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body["code"]
}

func seededStore() *mockStore {
	st := newMockStore()
	st.records["s1"] = &domain.ComplianceHealth{StatusID: "s1", LegalEntityID: "le-1"}
	maker := "maker"
	st.gaps["g1"] = &domain.ComplianceGap{GapID: "g1", LegalEntityID: "le-1", Status: domain.GapOpen, CreatedBy: &maker}
	return st
}

var readPaths = map[string]string{
	"/v1/compliance-status/s1":   actionComplianceStatusRead,
	"/v1/compliance-status/":     actionComplianceStatusRead,
	"/v1/compliance-status/gaps": actionComplianceGapRead,
}

func TestReads_RequirePrincipal(t *testing.T) {
	h := hRouter(seededStore(), &recordingAuthz{})
	for p := range readPaths {
		if w := call(t, h, http.MethodGet, p, "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without principal: expected 401, got %d", p, w.Code)
		}
	}
}

func TestReads_DenyIs403_UnavailableIs503_WithReadAction(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{authz.ErrAuthorizationDenied, http.StatusForbidden}, {authz.ErrAuthzServiceUnavailable, http.StatusServiceUnavailable}} {
		for p, action := range readPaths {
			az := &recordingAuthz{err: tc.err}
			if w := call(t, hRouter(seededStore(), az), http.MethodGet, p, "reader", ""); w.Code != tc.want {
				t.Errorf("GET %s (%v): expected %d, got %d", p, tc.err, tc.want, w.Code)
			}
			if len(az.actions) != 1 || az.actions[0] != action {
				t.Errorf("GET %s: expected one %s check, got %v", p, action, az.actions)
			}
		}
	}
}

func TestReads_Scope(t *testing.T) {
	az := &recordingAuthz{}
	h := hRouter(seededStore(), az)
	for _, p := range []string{"/v1/compliance-status/s1", "/v1/compliance-status/gaps?legal_entity_id=le-2", "/v1/compliance-status/"} {
		if w := call(t, h, http.MethodGet, p, "reader", ""); w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", p, w.Code)
		}
	}
	want := []string{"le-1", "le-2", "t1"}
	for i := range want {
		if az.scopes[i] != want[i] {
			t.Errorf("check %d: scope %s, want %s", i, az.scopes[i], want[i])
		}
	}
}

func TestEvaluate_ActorRule(t *testing.T) {
	st := newMockStore()
	h := hRouter(st, &recordingAuthz{})
	body := `{"legal_entity_id":"le-1","jurisdiction_id":"GB","domain_name":"TAX","total_obligations":1,"fulfilled_obligations":1`
	w := call(t, h, http.MethodPost, "/v1/compliance-status/evaluate", "officer", body+`,"created_by":"forged"}`)
	if w.Code != http.StatusForbidden || codeOf(w) != codeActorMismatch {
		t.Fatalf("differing created_by: expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
	if len(st.records) != 0 {
		t.Fatal("refused evaluate must not be stored")
	}
	w = call(t, h, http.MethodPost, "/v1/compliance-status/evaluate", "officer", body+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("absent created_by: %d %s", w.Code, w.Body.String())
	}
	var c domain.ComplianceHealth
	_ = json.Unmarshal(w.Body.Bytes(), &c)
	if c.CreatedBy != "officer" {
		t.Fatalf("created_by must be the principal, got %q", c.CreatedBy)
	}
}

func TestCreateGap_RecordsPrincipal(t *testing.T) {
	st := newMockStore()
	h := hRouter(st, &recordingAuthz{})
	w := call(t, h, http.MethodPost, "/v1/compliance-status/gaps", "maker",
		`{"legal_entity_id":"le-1","jurisdiction_id":"GB","domain_name":"TAX","gap_type":"X"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create gap: %d %s", w.Code, w.Body.String())
	}
	var g domain.ComplianceGap
	_ = json.Unmarshal(w.Body.Bytes(), &g)
	if g.CreatedBy == nil || *g.CreatedBy != "maker" {
		t.Fatalf("created_by must be the principal, got %v", g.CreatedBy)
	}
}

func TestResolveGap_SegregationOfDuties(t *testing.T) {
	st := seededStore()
	h := hRouter(st, &recordingAuthz{})
	w := call(t, h, http.MethodPost, "/v1/compliance-status/gaps/g1/resolve", "maker", `{}`)
	if w.Code != http.StatusForbidden || codeOf(w) != codeSegregationOfDuties {
		t.Fatalf("creator resolving: expected 403 segregation_of_duties, got %d %s", w.Code, w.Body.String())
	}
	if st.gaps["g1"].Status != domain.GapOpen {
		t.Fatal("refused resolve must not change state")
	}
	if w := call(t, h, http.MethodPost, "/v1/compliance-status/gaps/g1/resolve", "checker", `{}`); w.Code != http.StatusOK {
		t.Fatalf("other principal resolving: %d %s", w.Code, w.Body.String())
	}
}

type sodStore struct{ *mockStore }

func (sodStore) ResolveGap(context.Context, string, *domain.ResolveGapRequest, store.EventMeta) (*domain.ComplianceGap, error) {
	return nil, domain.ErrSegregationOfDuties
}

// The store's lock-time refusal maps to the same 403.
func TestResolveGap_StoreSoDIs403(t *testing.T) {
	h := hRouter(sodStore{seededStore()}, &recordingAuthz{})
	w := call(t, h, http.MethodPost, "/v1/compliance-status/gaps/g1/resolve", "checker", `{}`)
	if w.Code != http.StatusForbidden || codeOf(w) != codeSegregationOfDuties {
		t.Fatalf("expected 403 segregation_of_duties, got %d %s", w.Code, w.Body.String())
	}
}
