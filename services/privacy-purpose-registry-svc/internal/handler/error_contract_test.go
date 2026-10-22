package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/handler"
	"zoiko.io/privacy-purpose-registry-svc/internal/store"
)

// Error-contract tests for the transport-level responses. The README states
// "every error is {error, error_code}"; handler and pre-flight errors already
// satisfy that (TestErrorContract_*). These tests cover the responses that
// used to bypass it: chi's plain-text 404 and 405, chi's empty-body 500 on a
// panic, and the /readyz failure body. They run against handler.NewRouter,
// the very router cmd/server serves.

// panicStore panics when the purpose list is read — a stand-in for any bug that
// would otherwise reach the Recoverer.
type panicStore struct{ *stubStore }

func (panicStore) ListPurposes(_ context.Context) ([]domain.PurposeVersion, error) {
	panic("boom: simulated store bug (must never reach the caller)")
}

func newServiceRouter(st store.Store) http.Handler {
	return handler.NewRouter(handler.New(st, &stubAuthz{}, zap.NewNop()), zap.NewNop(), nil)
}

// strictEnvelope asserts the body is exactly {"error": "...", "error_code": "..."}.
func strictEnvelope(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json (body %s)", ct, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not JSON: %v: %q", err, w.Body.String())
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "error,error_code" {
		t.Fatalf("envelope keys = %v, want exactly [error error_code]", keys)
	}
	if s, _ := m["error"].(string); s == "" {
		t.Fatalf("error message is empty: %v", m)
	}
	if got, _ := m["error_code"].(string); got != code {
		t.Fatalf("error_code = %q, want %q", got, code)
	}
}

func TestErrorContract_UnknownRoute_Is404WithEnvelope(t *testing.T) {
	r := newServiceRouter(newStubStore())
	for _, path := range []string{
		"/nope",
		"/privacy",
		"/privacy/nope",
		"/privacy/purposes/" + tenantA + "/nope",
		"/privacy/processing-activities/" + tenantA + "/versions/x/unknown-verb",
	} {
		// Authenticated or not, an unknown path is answered identically.
		strictEnvelope(t, call(r, http.MethodGet, path, nil, tenantA, "reader-01"), http.StatusNotFound, domain.ErrCodeNotFound)
		strictEnvelope(t, call(r, http.MethodGet, path, nil, "", ""), http.StatusNotFound, domain.ErrCodeNotFound)
	}
}

func TestErrorContract_UnsupportedMethod_Is405WithEnvelopeAndAllow(t *testing.T) {
	r := newServiceRouter(newStubStore())
	cases := []struct{ method, path, allow string }{
		{http.MethodDelete, "/privacy/purposes", "GET, POST"},
		{http.MethodPut, "/privacy/ropa", "GET"},
		{http.MethodDelete, "/privacy/processing-activities/" + tenantA, "GET"},
		{http.MethodGet, "/privacy/processing-activities/" + tenantA + "/validate", "POST"},
		{http.MethodPost, "/healthz", "GET"},
	}
	for _, c := range cases {
		w := call(r, c.method, c.path, nil, tenantA, "reader-01")
		strictEnvelope(t, w, http.StatusMethodNotAllowed, domain.ErrCodeMethodNotAllowed)
		if got := w.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s %s: Allow = %q, want %q", c.method, c.path, got, c.allow)
		}
	}
}

func TestErrorContract_RecoveredPanic_Is500WithEnvelopeAndNoLeak(t *testing.T) {
	r := newServiceRouter(panicStore{newStubStore()})
	w := call(r, http.MethodGet, purposesPath, nil, tenantA, "reader-01")
	strictEnvelope(t, w, http.StatusInternalServerError, domain.ErrCodeInternal)
	if strings.Contains(w.Body.String(), "boom") || strings.Contains(w.Body.String(), "simulated") {
		t.Fatalf("the panic value leaked to the caller: %s", w.Body.String())
	}
}

func TestErrorContract_ReadinessFailure_Is503WithEnvelope(t *testing.T) {
	r := newServiceRouter(newStubStore()) // nil pool: the database is not initialised
	strictEnvelope(t, call(r, http.MethodGet, "/readyz", nil, "", ""), http.StatusServiceUnavailable, domain.ErrCodeStoreUnavailable)
}

// Liveness is unchanged, and still needs no tenant or principal.
func TestErrorContract_HealthzUnchanged(t *testing.T) {
	r := newServiceRouter(newStubStore())
	w := call(r, http.MethodGet, "/healthz", nil, "", "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("healthz = %d %q", w.Code, w.Body.String())
	}
}

// The router's error handlers must not disturb the pre-flight on real routes:
// no principal is still 401, no tenant still 401, a denied grant still 403 — and
// registered routes still work end to end through the production router.
func TestErrorContract_RegisteredRoutesKeepTheirPreflight(t *testing.T) {
	st, az := newStubStore(), &stubAuthz{}
	r := handler.NewRouter(handler.New(st, az, zap.NewNop()), zap.NewNop(), nil)

	wantErr(t, call(r, http.MethodGet, purposesPath, nil, tenantA, ""), http.StatusUnauthorized, domain.ErrCodeAuthenticationFailed)
	wantErr(t, call(r, http.MethodGet, purposesPath, nil, "", "reader-01"), http.StatusUnauthorized, domain.ErrCodeTenantScopeRequired)
	az.deny = true
	wantErr(t, call(r, http.MethodGet, purposesPath, nil, tenantA, "reader-01"), http.StatusForbidden, domain.ErrCodeAuthorizationDenied)
	az.deny = false
	wantOK(t, call(r, http.MethodGet, purposesPath, nil, tenantA, "reader-01"), http.StatusOK)
	wantErr(t, call(r, http.MethodPost, purposesPath, map[string]string{"statement": "x"}, tenantA, "maker-01", map[string]string{"Idempotency-Key": ""}),
		http.StatusBadRequest, domain.ErrCodeIdempotencyRequired)
}
