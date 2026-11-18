package telemetry_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"zoiko.io/evidence-requirements-svc/internal/telemetry"
)

// TestER05_MetricsServesEveryEvidenceSeries scrapes /metrics and finds each
// domain series ER-5 asks for, with every label value present from zero.
func TestER05_MetricsServesEveryEvidenceSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := telemetry.NewEvidenceMetrics(reg, "evidence-requirements-svc")
	m.EvaluationRecorded("MISSING")
	m.OwnerUnavailable("document-vault-svc")
	m.SetOutboxUnpublished(7)

	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	for _, want := range []string{
		`evidence_evaluations_total{outcome="MISSING",service="evidence-requirements-svc"} 1`,
		`evidence_evaluations_total{outcome="SATISFIED",service="evidence-requirements-svc"} 0`,
		`evidence_evaluations_total{outcome="NO_REQUIREMENTS_DEFINED",service="evidence-requirements-svc"} 0`,
		`evidence_owner_unavailable_total{owner="document-vault-svc",service="evidence-requirements-svc"} 1`,
		`evidence_owner_unavailable_total{owner="evidence-manifest-svc",service="evidence-requirements-svc"} 0`,
		`evidence_owner_unavailable_total{owner="esignature-integration-svc",service="evidence-requirements-svc"} 0`,
		`evidence_outbox_unpublished{service="evidence-requirements-svc"} 7`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics does not serve %s", want)
		}
	}
}
