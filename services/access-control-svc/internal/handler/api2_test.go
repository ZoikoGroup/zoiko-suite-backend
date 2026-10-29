package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
)

func acsRouter(authz *stubAuthZ, admin *stubAuthzAdmin) chi.Router {
	protect := []string{"PLATFORM_ADMIN", "TENANT_ADMIN", "ROLE_MANAGE", "USER_PROVISION", "ENTITY_MANAGE", "AUDIT_READ", "SECURITY_POLICY_MANAGE", "BILLING_ADMIN"}
	return newRouter(newStubStore(), &stubPublisher{}, authz, admin, &stubSoD{}, &stubProtectedActions{actions: protect})
}

func assertProblem(t *testing.T, rr *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rr.Code != wantStatus {
		t.Fatalf("expected %d got %d: %s", wantStatus, rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("expected application/problem+json, got %q", ct)
	}
	var body struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("body is not JSON: %s", rr.Body.String())
	}
	if body.Code != wantCode {
		t.Fatalf("expected code %q got %q: %s", wantCode, body.Code, rr.Body.String())
	}
}

func TestErrorResponses_AreProblemDetails(t *testing.T) {
	// 400 malformed body.
	req := httptest.NewRequest(http.MethodPost, "/v1/role-definitions/", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", "admin-1")
	rr := httptest.NewRecorder()
	acsRouter(&stubAuthZ{}, &stubAuthzAdmin{}).ServeHTTP(rr, req)
	assertProblem(t, rr, http.StatusBadRequest, "invalid_json")

	// 401 no principal.
	rr = doReq(acsRouter(&stubAuthZ{}, &stubAuthzAdmin{}), http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "")
	assertProblem(t, rr, http.StatusUnauthorized, "identity_missing")

	// 403 authorization denied.
	rr = doReq(acsRouter(&stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubAuthzAdmin{}), http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
	assertProblem(t, rr, http.StatusForbidden, "forbidden")

	// 404 nonexistent role.
	rr = doReq(acsRouter(&stubAuthZ{}, &stubAuthzAdmin{}), http.MethodGet, "/v1/role-definitions/00000000-0000-0000-0000-000000000000", nil, "admin-1")
	assertProblem(t, rr, http.StatusNotFound, "not_found")

	// 409 duplicate role code (same code, different correlation_id).
	r := acsRouter(&stubAuthZ{}, &stubAuthzAdmin{})
	first := roleBody(uuid.NewString())
	if rr := doReq(r, http.MethodPost, "/v1/role-definitions/", first, "admin-1"); rr.Code != http.StatusCreated {
		t.Fatalf("setup create returned %d: %s", rr.Code, rr.Body.String())
	}
	second := roleBody(uuid.NewString())
	second["role_code"] = first["role_code"]
	rr = doReq(r, http.MethodPost, "/v1/role-definitions/", second, "admin-1")
	assertProblem(t, rr, http.StatusConflict, "role_code_exists")

	// 503 authz admin unavailable.
	rr = doReq(acsRouter(&stubAuthZ{}, &stubAuthzAdmin{createRoleErr: domain.ErrAuthzAdminUnavailable}), http.MethodPost, "/v1/role-definitions/", roleBody(uuid.NewString()), "admin-1")
	assertProblem(t, rr, http.StatusServiceUnavailable, "authz_admin_unavailable")

	// 400 invalid query.
	req = httptest.NewRequest(http.MethodGet, "/v1/role-definitions/?status=bogus", nil)
	req.Header.Set("X-Principal-Id", "admin-1")
	rr = httptest.NewRecorder()
	acsRouter(&stubAuthZ{}, &stubAuthzAdmin{}).ServeHTTP(rr, req)
	assertProblem(t, rr, http.StatusBadRequest, "invalid_status")
}
