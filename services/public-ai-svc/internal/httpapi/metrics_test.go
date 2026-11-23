package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Doc 08 §14 metrics: request counts with outcome labels and latency
// histograms are exported at /metrics (outside /public-ai/v1), and no
// session, grant, request id or message text is ever a label or value.
func TestMetricsEndpoint(t *testing.T) {
	h := newHarness(t, time.Hour)
	s := h.newSession("key-00000077")
	secret := "my payroll id is 12345"
	if w := h.turn(s, turnUUID, secret); w.Code != http.StatusOK {
		t.Fatalf("turn: %d", w.Code)
	}
	w := h.req(http.MethodGet, "/metrics", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`public_ai_http_requests_total{code="200",method="POST",route="/public-ai/v1/turns"}`,
		`public_ai_http_request_duration_seconds_bucket{route="/public-ai/v1/turns"`,
		"public_ai_release_ready 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
	for _, leak := range []string{s.SessionID, s.SessionGrant, turnUUID, "payroll", "12345", "req_"} {
		if strings.Contains(body, leak) {
			t.Fatalf("/metrics exposes %q", leak)
		}
	}
	if w := h.req(http.MethodGet, "/public-ai/v1/metrics", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("metrics must not be served under the public API, got %d", w.Code)
	}
}
