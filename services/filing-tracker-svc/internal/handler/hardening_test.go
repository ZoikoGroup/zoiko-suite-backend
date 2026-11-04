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

	"zoiko.io/filing-tracker-svc/internal/authz"
	"zoiko.io/filing-tracker-svc/internal/domain"
	"zoiko.io/filing-tracker-svc/internal/middleware"
	"zoiko.io/filing-tracker-svc/internal/store"
)

// recordingAuthz returns err for every check and records the actions asked.
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

func hardeningRouter(st *mockStore, az AuthzClient) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.TenantContextMiddleware)
	RegisterRoutes(r, New(st, az, zap.NewNop()))
	return r
}

func do(t *testing.T, h http.Handler, method, path, principal, body string) *httptest.ResponseRecorder {
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

func code(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body["code"]
}

func seeded() *mockStore {
	st := newMockStore()
	st.items["f1"] = &domain.FilingRequirement{FilingID: "f1", LegalEntityID: "le-1", Status: domain.StatusScheduled, CreatedBy: "maker"}
	return st
}

const (
	pathList = "/v1/filing-tracker/requirements"
	pathOne  = "/v1/filing-tracker/requirements/f1"
)

func TestReads_RequirePrincipal(t *testing.T) {
	h := hardeningRouter(seeded(), &recordingAuthz{})
	for _, p := range []string{pathList, pathOne} {
		if w := do(t, h, http.MethodGet, p, "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without principal: expected 401, got %d", p, w.Code)
		}
	}
}

func TestReads_DeniedIs403_UnavailableIs503(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{{authz.ErrAuthorizationDenied, http.StatusForbidden}, {authz.ErrAuthzServiceUnavailable, http.StatusServiceUnavailable}} {
		az := &recordingAuthz{err: tc.err}
		h := hardeningRouter(seeded(), az)
		for _, p := range []string{pathList, pathOne} {
			if w := do(t, h, http.MethodGet, p, "reader", ""); w.Code != tc.want {
				t.Errorf("GET %s with authz err %v: expected %d, got %d", p, tc.err, tc.want, w.Code)
			}
		}
		for _, a := range az.actions {
			if a != ActionFilingRequirementRead {
				t.Errorf("expected read action %s, got %s", ActionFilingRequirementRead, a)
			}
		}
	}
}

func TestReads_AllowedAndScoped(t *testing.T) {
	az := &recordingAuthz{}
	h := hardeningRouter(seeded(), az)
	if w := do(t, h, http.MethodGet, pathOne, "reader", ""); w.Code != http.StatusOK {
		t.Fatalf("GET one: expected 200, got %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, pathList+"?legal_entity_id=le-1", "reader", ""); w.Code != http.StatusOK {
		t.Fatalf("GET list: expected 200, got %d", w.Code)
	}
	if w := do(t, h, http.MethodGet, pathList, "reader", ""); w.Code != http.StatusOK {
		t.Fatalf("GET list (tenant-wide): expected 200, got %d", w.Code)
	}
	want := []string{"le-1", "le-1", "t1"}
	for i, s := range want {
		if az.scopes[i] != s {
			t.Errorf("check %d: expected scope %s, got %s", i, s, az.scopes[i])
		}
	}
}

func TestCreate_ActorRule(t *testing.T) {
	body := func(createdBy string) string {
		return `{"legal_entity_id":"le-1","jurisdiction_id":"GB","filing_authority":"HMRC","period_key":"Q1","due_date":"2026-12-31"` + createdBy + `}`
	}
	st := newMockStore()
	h := hardeningRouter(st, &recordingAuthz{})
	w := do(t, h, http.MethodPost, pathList, "maker", body(`,"created_by":"someone-else"`))
	if w.Code != http.StatusForbidden || code(t, w) != codeActorMismatch {
		t.Fatalf("differing created_by: expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
	if len(st.items) != 0 {
		t.Fatal("refused create must not be stored")
	}
	w = do(t, h, http.MethodPost, pathList, "maker", body(""))
	if w.Code != http.StatusCreated {
		t.Fatalf("absent created_by: expected 201, got %d %s", w.Code, w.Body.String())
	}
	var f domain.FilingRequirement
	_ = json.Unmarshal(w.Body.Bytes(), &f)
	if f.CreatedBy != "maker" {
		t.Fatalf("created_by must be the principal, got %q", f.CreatedBy)
	}
}

func TestSubmit_ActorRule(t *testing.T) {
	st := seeded()
	h := hardeningRouter(st, &recordingAuthz{})
	w := do(t, h, http.MethodPost, pathOne+"/submit", "filer", `{"submission_reference":"S1","submitted_by":"forged"}`)
	if w.Code != http.StatusForbidden || code(t, w) != codeActorMismatch {
		t.Fatalf("differing submitted_by: expected 403 actor_mismatch, got %d %s", w.Code, w.Body.String())
	}
	// Absent submitted_by is no longer a 400: it is filled from the principal.
	w = do(t, h, http.MethodPost, pathOne+"/submit", "filer", `{"submission_reference":"S1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("absent submitted_by: expected 200, got %d %s", w.Code, w.Body.String())
	}
	if got := st.items["f1"].SubmittedBy; got == nil || *got != "filer" {
		t.Fatalf("submitted_by must be the principal, got %v", got)
	}
}

func TestConfirm_SegregationOfDuties(t *testing.T) {
	st := seeded()
	h := hardeningRouter(st, &recordingAuthz{})
	if w := do(t, h, http.MethodPost, pathOne+"/submit", "filer", `{"submission_reference":"S1"}`); w.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, http.MethodPost, pathOne+"/confirm", "filer", `{"confirmation_reference":"ACK"}`)
	if w.Code != http.StatusForbidden || code(t, w) != codeSegregationOfDuties {
		t.Fatalf("submitter confirming: expected 403 segregation_of_duties, got %d %s", w.Code, w.Body.String())
	}
	if st.items["f1"].Status != domain.StatusSubmitted {
		t.Fatal("refused confirm must not change state")
	}
	if w := do(t, h, http.MethodPost, pathOne+"/confirm", "reviewer", `{"confirmation_reference":"ACK"}`); w.Code != http.StatusOK {
		t.Fatalf("different principal confirming: expected 200, got %d %s", w.Code, w.Body.String())
	}
}

// The store's lock-time SoD refusal (race between the handler check and the
// row lock) is mapped to the same 403.
type sodStore struct{ *mockStore }

func (sodStore) Confirm(context.Context, string, *domain.ConfirmFilingRequest, store.EventMeta) (*domain.FilingRequirement, error) {
	return nil, domain.ErrSegregationOfDuties
}

func TestConfirm_StoreSoDIs403(t *testing.T) {
	st := seeded()
	st.items["f1"].Status = domain.StatusSubmitted
	r := chi.NewRouter()
	r.Use(middleware.TenantContextMiddleware)
	RegisterRoutes(r, New(sodStore{st}, &recordingAuthz{}, zap.NewNop()))
	w := do(t, r, http.MethodPost, pathOne+"/confirm", "reviewer", `{"confirmation_reference":"ACK"}`)
	if w.Code != http.StatusForbidden || code(t, w) != codeSegregationOfDuties {
		t.Fatalf("expected 403 segregation_of_duties, got %d %s", w.Code, w.Body.String())
	}
}

func TestWrites_DeniedIs403(t *testing.T) {
	h := hardeningRouter(seeded(), &recordingAuthz{err: authz.ErrAuthorizationDenied})
	if w := do(t, h, http.MethodPost, pathOne+"/submit", "filer", `{"submission_reference":"S1"}`); w.Code != http.StatusForbidden {
		t.Fatalf("submit denied: expected 403, got %d", w.Code)
	}
}
