package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	svcenvelope "zoiko.io/governance-decision-log-svc/internal/envelope"
	"zoiko.io/governance-decision-log-svc/internal/handler"
)

// envelopedRouter is the production middleware order: the problem rewriter in
// front of the generated envelope middleware, in front of the handlers.
func envelopedRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(handler.ProblemEnvelopeRefusals)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), nil))
	handler.RegisterRoutes(r, handler.New(&stubStore{created: true}, testAuthz(), &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))
	return r
}

// Audit I3: envelope refusals were `{"error",...}` application/json while
// every other error is RFC 9457.
func TestEnvelopeRefusal_IsProblemJSONAndKeepsItsMembers(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Principal-Id", testPrincipal)
	w := httptest.NewRecorder()
	envelopedRouter().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d — %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var p struct {
		Type       string            `json:"type"`
		Title      string            `json:"title"`
		Status     int               `json:"status"`
		Detail     string            `json:"detail"`
		Error      string            `json:"error"`
		Violations []json.RawMessage `json:"violations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not JSON: %v — %s", err, w.Body.String())
	}
	if p.Type != "ENVELOPE_INCOMPLETE" || p.Status != 400 || p.Title == "" {
		t.Errorf("not an RFC 9457 problem: %+v", p)
	}
	if p.Error != "envelope_incomplete" || p.Detail == "" || len(p.Violations) == 0 {
		t.Errorf("the original error/detail/violations members must be kept: %+v", p)
	}
}

func TestEnvelopeRefusal_MissingIdentityIs401Problem(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(validBody()))
	w := httptest.NewRecorder()
	envelopedRouter().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || w.Header().Get("Content-Type") != "application/problem+json" ||
		!strings.Contains(w.Body.String(), `"type":"UNAUTHORIZED"`) {
		t.Fatalf("expected a 401 problem, got %d %s — %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

// A read admitted out of contract carries the same header but is an answer,
// not a refusal: it must pass through untouched.
func TestEnvelopeRefusal_AdmittedReadPassesThrough(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/decisions", nil)
	req.Header.Set("X-Tenant-Id", testTenant)
	req.Header.Set("X-Principal-Id", testPrincipal)
	w := httptest.NewRecorder()
	envelopedRouter().ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("expected the read's own 200 [], got %d %s — %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}
