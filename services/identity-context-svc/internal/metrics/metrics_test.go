package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"zoiko.io/identity-context-svc/internal/metrics"
)

// TestIC8_MetricsEndpointShowsEverySeries: each IAM-6 series is exposed on
// /metrics once touched. identity_login_throttled_total{route} is the OIDC
// provider's (internal/oidc), so it is not registered here twice.
func TestIC8_MetricsEndpointShowsEverySeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	m.MFAVerifications.WithLabelValues("success").Inc()
	m.LoginLockouts.Inc()
	m.TOTPReplay.Inc()
	m.SessionsActive.Set(3)
	m.SessionRevoc.WithLabelValues("SESSION_CONCURRENCY_CAP").Inc()
	m.PasswordResets.WithLabelValues("APPROVE").Inc()

	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		`identity_mfa_verifications_total{outcome="success"} 1`,
		`identity_login_lockouts_total 1`,
		`identity_totp_replay_refusals_total 1`,
		`identity_sessions_active 3`,
		`identity_session_revocations_total{reason="SESSION_CONCURRENCY_CAP"} 1`,
		`identity_password_resets_total{decision="APPROVE"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}
