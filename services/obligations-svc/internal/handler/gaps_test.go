package handler_test

// Coverage for the gaps closed in the 17 Aug 2026 pass on obligations-svc.
// Kept in its own file so each test sits next to the reason it exists.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/obligations-svc/internal/domain"
)

// This service shipped with NO authorization at all — its config said so, and
// said the Authorization Service "doesn't exist". It does, and fifteen other
// services call it. What the deferral left behind was an open write surface on
// a statutory compliance register.
func TestWrites_AreAuthorized(t *testing.T) {
	cases := []struct {
		name, method, path, action string
		body                       string
	}{
		{"create obligation", http.MethodPost, "/v1/obligations", "OBLIGATION_CREATE", validCreateBody()},
		{"update status", http.MethodPost, "/v1/obligations/o-1/status", "OBLIGATION_STATUS_UPDATE",
			`{"obligation_status":"IN_PROGRESS"}`},
		{"add filing requirement", http.MethodPost, "/v1/obligations/o-1/filing-requirements", "FILING_REQUIREMENT_CREATE",
			`{"filing_type":"VAT_RETURN","filing_authority":"HMRC","submission_channel":"API"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{
				obligation:        &domain.Obligation{ObligationID: "o-1", LegalEntityID: "le-1"},
				obligationCreated: true,
				updated:           &domain.Obligation{ObligationID: "o-1", LegalEntityID: "le-1"},
				transitioned:      true,
				filingReq:         &domain.FilingRequirement{FilingRequirementID: "f-1"},
			}
			az := &stubAuthz{err: domain.ErrAuthorizationDenied}
			r := newTestRouterAuthz(store, &stubPublisher{}, &stubValidator{}, az)

			w := httptest.NewRecorder()
			r.ServeHTTP(w, withIdentity(httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))))

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 when authorization denies, got %d: %s", w.Code, w.Body.String())
			}
			if len(az.calls) != 1 || az.calls[0] != tc.action {
				t.Fatalf("expected one %s check, got %v", tc.action, az.calls)
			}
		})
	}
}

// An unreachable authorization-svc must refuse the write, never permit it.
func TestWrites_AuthorizationUnavailable_FailsClosed(t *testing.T) {
	store := &stubStore{obligation: &domain.Obligation{ObligationID: "o-1"}, obligationCreated: true}
	az := &stubAuthz{err: domain.ErrAuthorizationUnavailable}
	r := newTestRouterAuthz(store, &stubPublisher{}, &stubValidator{}, az)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations",
		bytes.NewBufferString(validCreateBody()))))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 fail-closed, got %d: %s", w.Code, w.Body.String())
	}
}

// Before 000002 this service had no tenant dimension at all, so every read
// returned every tenant's obligations. There was nothing to refuse.
func TestRequests_WithoutTenantScope_Are401(t *testing.T) {
	r, _ := routerWithData()

	cases := []struct{ name, method, path, body string }{
		{"list", http.MethodGet, "/v1/obligations", ""},
		{"get", http.MethodGet, "/v1/obligations/o-1", ""},
		{"create", http.MethodPost, "/v1/obligations", validCreateBody()},
		{"status", http.MethodPost, "/v1/obligations/o-1/status", `{"obligation_status":"CLOSED"}`},
		{"filings list", http.MethodGet, "/v1/obligations/o-1/filing-requirements", ""},
		{"filing create", http.MethodPost, "/v1/obligations/o-1/filing-requirements",
			`{"filing_type":"VAT_RETURN","filing_authority":"HMRC","submission_channel":"API"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			req.Header.Set("X-Principal-Id", testPrincipal) // identity, but no tenant
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 without a tenant, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestRequests_WithoutPrincipal_Are401(t *testing.T) {
	r, _ := routerWithData()
	for _, path := range []string{"/v1/obligations", "/v1/obligations/o-1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Id", testTenant) // tenant, but no identity
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401 without a principal, got %d", path, w.Code)
		}
	}
}

// created_by_principal_id used to be written straight from the request body,
// so the record of who raised a statutory obligation was self-declared.
func TestCreateObligation_CreatedByMustBeTheAuthenticatedPrincipal(t *testing.T) {
	store := &stubStore{obligation: &domain.Obligation{ObligationID: "o-1"}, obligationCreated: true}
	r := newTestRouter(store)

	body := strings.Replace(validCreateBody(),
		`"created_by_principal_id": "admin-1"`,
		`"created_by_principal_id": "somebody-else"`, 1)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations",
		bytes.NewBufferString(body))))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a created_by naming another principal, got %d: %s", w.Code, w.Body.String())
	}
}

// A misspelled field used to be discarded silently, so the caller got
// "missing_field: due_date" for a field they believed they had sent.
func TestCreateObligation_UnknownField_IsRejected(t *testing.T) {
	r := newTestRouter(&stubStore{})
	body := strings.Replace(validCreateBody(), `"due_date"`, `"due_dat"`, 1)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations",
		bytes.NewBufferString(body))))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown field, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_json") {
		t.Errorf("expected the decode to reject it, got %s", w.Body.String())
	}
}

// The register was unbounded: every obligation a tenant had ever recorded, in
// one response, forever.
func TestListObligations_IsPagedAndValidated(t *testing.T) {
	store := &stubStore{list: []*domain.Obligation{{ObligationID: "o-1"}}}
	r := newTestRouter(store)

	for _, q := range []string{"?limit=abc", "?limit=0", "?limit=99999", "?offset=-1"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations"+q, nil)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", q, w.Code)
		}
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// A mistyped obligation id is a uuid that Postgres refuses inside the driver.
// It used to surface as 503 store_unavailable — an outage status for a typo.
func TestGetObligation_MalformedID_IsNotFoundNotAnOutage(t *testing.T) {
	store := &stubStore{findErr: domain.ErrObligationNotFound}
	r := newTestRouter(store)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations/not-a-uuid", nil)))

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// A store refusing an unscoped call must read as 401, not as an outage.
func TestStoreTenantMissing_Surfaces401(t *testing.T) {
	store := &stubStore{listErr: domain.ErrTenantMissing}
	r := newTestRouter(store)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations", nil)))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

// routerWithData is a router whose store answers every route successfully, so
// a test asserting on a precondition failure cannot pass for the wrong reason.
func routerWithData() (interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}, *stubStore) {
	store := &stubStore{
		obligation:        &domain.Obligation{ObligationID: "o-1", LegalEntityID: "le-1"},
		obligationCreated: true,
		findObligation:    &domain.Obligation{ObligationID: "o-1", LegalEntityID: "le-1"},
		list:              []*domain.Obligation{{ObligationID: "o-1"}},
		updated:           &domain.Obligation{ObligationID: "o-1", LegalEntityID: "le-1"},
		transitioned:      true,
		filingReq:         &domain.FilingRequirement{FilingRequirementID: "f-1"},
		filingList:        []*domain.FilingRequirement{{FilingRequirementID: "f-1"}},
	}
	return newTestRouter(store), store
}
