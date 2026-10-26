package health_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/health"
)

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestHandler_Liveness(t *testing.T) {
	log, _ := zap.NewDevelopment()
	h := health.New(nil, log)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.Liveness(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Liveness returned %d, want 200", w.Code)
	}

	body := w.Body.String()
	if !contains(body, `"status"`) || !contains(body, `"ok"`) {
		t.Errorf("Liveness body = %q, want status ok", body)
	}
}

func TestHandler_Readiness_NoPool(t *testing.T) {
	log, _ := zap.NewDevelopment()
	h := health.New(nil, log)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.Readiness(w, req)

	// Without pool, readiness should fail
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("Readiness returned %d, want 503 (no pool)", w.Code)
	}

	body := w.Body.String()
	if !contains(body, `db_pool_not_configured`) {
		t.Errorf("Readiness body = %q, want db_pool_not_configured", body)
	}
}

func TestHandler_ContentType(t *testing.T) {
	log, _ := zap.NewDevelopment()
	h := health.New(nil, log)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.Liveness(w, req)

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", contentType)
	}
}

func TestNew(t *testing.T) {
	log, _ := zap.NewDevelopment()
	h := health.New(nil, log)

	if h == nil {
		t.Errorf("New returned nil")
	}
}
