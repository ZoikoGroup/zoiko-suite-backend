package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/config"
)

const testOrigin = "http://localhost:3000"

func testConfig() config.Config {
	return config.Config{
		AllowedOrigins:           []string{testOrigin},
		MaxBodyBytes:             16 * 1024,
		MaxActiveTurnsPerSession: 1,
	}
}

func do(t *testing.T, h http.Handler, method, path, origin, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// CT-003 / SEC-T: a non-allowlisted origin is refused with 403 origin_denied.
func TestCT003_OriginDenied(t *testing.T) {
	h := New(testConfig(), nil, Deps{}).Router()
	w := do(t, h, http.MethodPost, "/public-ai/v1/sessions", "https://evil.example", "application/json", "{}")
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "publicai.origin_denied") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestMissingOriginDenied(t *testing.T) {
	h := New(testConfig(), nil, Deps{}).Router()
	w := do(t, h, http.MethodPost, "/public-ai/v1/sessions", "", "application/json", "{}")
	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d", w.Code)
	}
}

// CT-004: body above the cap is refused with 413 before any handler runs.
func TestCT004_OversizedBody(t *testing.T) {
	h := New(testConfig(), nil, Deps{}).Router()
	w := do(t, h, http.MethodPost, "/public-ai/v1/turns", testOrigin, "application/json", strings.Repeat("a", 17*1024))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", w.Code)
	}
}

// CT-005: non-JSON content type is refused with 415.
func TestCT005_WrongContentType(t *testing.T) {
	h := New(testConfig(), nil, Deps{}).Router()
	w := do(t, h, http.MethodPost, "/public-ai/v1/turns", testOrigin, "text/plain", "{}")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", w.Code)
	}
}

func TestPreflightAllowedOrigin(t *testing.T) {
	h := New(testConfig(), nil, Deps{}).Router()
	w := do(t, h, http.MethodOptions, "/public-ai/v1/turns", testOrigin, "", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("got %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") != testOrigin {
		t.Fatal("missing ACAO")
	}
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "X-Public-Chat-Session") {
		t.Fatal("session header not allowed in preflight")
	}
}

func TestRequestIDIssuedByServer(t *testing.T) {
	h := New(testConfig(), nil, Deps{}).Router()
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	req.Header.Set("X-Request-ID", "attacker-controlled")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	rid := w.Header().Get("X-Request-ID")
	if !strings.HasPrefix(rid, "req_") || rid == "attacker-controlled" {
		t.Fatalf("request id = %q", rid)
	}
}

func TestReadinessReflectsDependencies(t *testing.T) {
	notReady := New(testConfig(), nil, Deps{Ready: func() bool { return false }}).Router()
	if w := do(t, notReady, http.MethodGet, "/health/ready", "", "", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready should be 503, got %d", w.Code)
	}
	ready := New(testConfig(), nil, Deps{Ready: func() bool { return true }}).Router()
	if w := do(t, ready, http.MethodGet, "/health/ready", "", "", ""); w.Code != http.StatusOK {
		t.Fatalf("ready should be 200, got %d", w.Code)
	}
	if w := do(t, notReady, http.MethodGet, "/health/live", "", "", ""); w.Code != http.StatusOK {
		t.Fatalf("live must not depend on readiness, got %d", w.Code)
	}
}

func TestPublicHealthIsGeneric(t *testing.T) {
	h := New(testConfig(), nil, Deps{Ready: func() bool { return true }}).Router()
	w := do(t, h, http.MethodGet, "/public-ai/v1/health/public", testOrigin, "", "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"status":"ok"}` {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

// SSE is disabled in R0 (stream_enabled=false, ADR-005).
func TestTurnEventsDisabledInR0(t *testing.T) {
	h := New(testConfig(), nil, Deps{}).Router()
	w := do(t, h, http.MethodGet, "/public-ai/v1/turns/pub_t_x/events", testOrigin, "", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d", w.Code)
	}
}
