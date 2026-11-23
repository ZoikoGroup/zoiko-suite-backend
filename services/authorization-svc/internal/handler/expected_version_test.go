package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/authorization-svc/internal/domain"
)

// API-3 (ZS-API-001; decision P-09): a mutation of an existing, versioned
// aggregate names the version it was made against, as If-Match (or, for
// callers written before API-3, the body's expected_version). Missing → 428,
// stale → 412, and the version reaches the store's guarded write.

func retireRole(s *stubStore, ifMatch, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/roles/r-1/retire", strings.NewReader(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", ownRoleTenant)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	newTestRouter(s).ServeHTTP(w, req)
	return w
}

func TestExpectedVersion_IfMatchReachesTheStore(t *testing.T) {
	s := ownRoleStore()
	if w := retireRole(s, `"v7"`, ``); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotExpectedVersion != 7 {
		t.Errorf("store got expected version %d, want 7", s.gotExpectedVersion)
	}
}

// The response carries the new version as an ETag, so the next change can
// be made against it without re-reading.
func TestExpectedVersion_ResponseCarriesTheNewETag(t *testing.T) {
	s := ownRoleStore()
	s.setActiveRole.Version = 8
	w := retireRole(s, `"v7"`, ``)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("ETag"); got != `"v8"` {
		t.Errorf("ETag = %q, want \"v8\"", got)
	}
}

func TestExpectedVersion_BodyFormStillAccepted(t *testing.T) {
	s := ownRoleStore()
	if w := postAsAdmin(s, "/v1/admin/roles/r-1/retire", `{"expected_version":7}`); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotExpectedVersion != 7 {
		t.Errorf("store got expected version %d, want 7", s.gotExpectedVersion)
	}
}

func TestExpectedVersion_MissingIs428(t *testing.T) {
	for _, body := range []string{``, `{}`} {
		s := ownRoleStore()
		w := retireRole(s, "", body)
		if w.Code != http.StatusPreconditionRequired {
			t.Fatalf("body %q: want 428, got %d: %s", body, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "precondition_required") {
			t.Errorf("body %q: want code precondition_required, got %s", body, w.Body.String())
		}
		if s.setActiveCalls != 0 {
			t.Errorf("body %q: the store was written without a version", body)
		}
	}
}

func TestExpectedVersion_StaleIs412(t *testing.T) {
	s := ownRoleStore()
	s.setActiveErr = domain.ErrVersionConflict
	w := retireRole(s, `"v1"`, ``)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: want 412, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stale_version") {
		t.Errorf("want code stale_version, got %s", w.Body.String())
	}
}

func TestExpectedVersion_WildcardIsUnconditional(t *testing.T) {
	s := ownRoleStore()
	if w := retireRole(s, `*`, ``); w.Code != http.StatusOK {
		t.Fatalf("If-Match *: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.gotExpectedVersion != 0 {
		t.Errorf("If-Match *: store got expected version %d, want 0 (unconditional)", s.gotExpectedVersion)
	}
}

func TestExpectedVersion_InvalidIs400(t *testing.T) {
	cases := []struct{ name, ifMatch, body string }{
		{"negative body version", "", `{"expected_version":-1}`},
		{"non-numeric body version", "", `{"expected_version":"x"}`},
		{"malformed If-Match", `"vx"`, ``},
		{"If-Match and body disagree", `"v3"`, `{"expected_version":4}`},
	}
	for _, tc := range cases {
		s := ownRoleStore()
		if w := retireRole(s, tc.ifMatch, tc.body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", tc.name, w.Code, w.Body.String())
		}
		if s.setActiveCalls != 0 {
			t.Errorf("%s: the store was written", tc.name)
		}
	}
}

// The other versioned aggregates answer the same way.
func TestExpectedVersion_EveryVersionedRouteRequiresIt(t *testing.T) {
	for _, path := range []string{
		"/v1/admin/roles/r-1/reactivate",
		"/v1/admin/permission-bundles/b-1/retire",
		"/v1/admin/sod-rules/s-1/retire",
		"/v1/admin/abac-rules/x-1/retire",
		"/v1/admin/authority-limits/al-1/retire",
		"/v1/admin/authority-limits/al-1/change",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"reason_code":"x"}`))
		req.Header.Set("X-Principal-Id", "admin-1")
		req.Header.Set("X-Tenant-Id", ownRoleTenant)
		w := httptest.NewRecorder()
		newTestRouter(ownRoleStore()).ServeHTTP(w, req)
		if w.Code != http.StatusPreconditionRequired {
			t.Errorf("%s without a version: want 428, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}
