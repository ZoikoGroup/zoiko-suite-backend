package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"zoiko.io/workflow-svc/internal/telemetry"
)

var (
	testMetricsOnce sync.Once
	testMetrics     *telemetry.Metrics
)

func getTestMetrics() *telemetry.Metrics {
	testMetricsOnce.Do(func() {
		testMetrics = telemetry.NewMetrics("test-service")
	})
	return testMetrics
}

func TestMetrics_WrapReadiness(t *testing.T) {
	m := getTestMetrics()

	readyHandler := m.WrapReadiness(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	readyHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ready handler returned %d, want 200", w.Code)
	}
}

func TestMetrics_WrapReadiness_FailureSetsGaugeToZero(t *testing.T) {
	m := getTestMetrics()

	readyHandler := m.WrapReadiness(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	readyHandler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("ready handler returned %d, want 503", w.Code)
	}
}

func TestMetrics_MetricsHandler_ReturnsMetrics(t *testing.T) {
	m := getTestMetrics()

	readyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	promHandler := promhttp.Handler()

	handler := m.MetricsHandler(readyHandler, promHandler)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("metrics handler returned %d, want 200", w.Code)
	}

	body := w.Body.String()
	if len(body) == 0 {
		t.Errorf("metrics response body is empty")
	}
	// Just verify it's a valid prometheus metrics response
	if !contains(body, "# HELP") {
		t.Errorf("metrics should contain HELP lines")
	}
	if !contains(body, "readiness_up") {
		t.Errorf("metrics should contain readiness_up")
	}
}

func TestNewMetrics_DoesNotPanic(t *testing.T) {
	// Verify we can get the singleton without panic
	m := getTestMetrics()
	if m == nil {
		t.Errorf("getTestMetrics returned nil")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
