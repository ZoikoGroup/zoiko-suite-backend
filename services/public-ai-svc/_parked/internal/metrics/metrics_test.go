package metrics

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every metric an alert rule uses is one this service exports, so a renamed
// metric cannot silently disable an alert.
func TestAlertRulesUseExportedMetrics(t *testing.T) {
	ObserveHTTP("/x", "GET", 200, time.Millisecond)
	ObserveTurn("ANSWERED", "ANSWERED", time.Millisecond)
	ObserveModel("ok", 1, 1)
	RateLimited("ip")
	EventsPublished(1)
	EventsDeadLettered(1)
	SetReleaseReady(true)
	SetBuildInfo("test-sha", "mock:mock-extractive-v1")
	SetSpendRatio("day", 0.1)
	ObserveStage("model", time.Millisecond)
	SecuritySignal("RULES_TEST")
	OutboxOldest(time.Second)
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	exported := w.Body.String()

	rules, err := os.ReadFile("../../../../deployments/prometheus-rules-public-ai.yml")
	if err != nil {
		t.Fatal(err)
	}
	used := regexp.MustCompile(`public_ai_[a-z_]+`).FindAllString(string(rules), -1)
	if len(used) < 8 {
		t.Fatalf("rules reference only %d metrics", len(used))
	}
	for _, m := range used {
		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(m, "_bucket"), "_count"), "_sum")
		if !strings.Contains(exported, "# TYPE "+base+" ") {
			t.Errorf("alert rule uses %s, which is not exported", m)
		}
	}
}

// Doc 09 §20: offers and clicks are counted separately, with enum labels.
func TestConversionCounters(t *testing.T) {
	CTAOffered("demo")
	CTAOffered("demo")
	HandoffClicked("demo")
	Feedback("not_useful", "")
	SecuritySignal("PROMPT_INJECTION")
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		`public_ai_cta_offered_total{route_key="demo"} 2`,
		`public_ai_handoff_clicked_total{route_key="demo"} 1`,
		`public_ai_feedback_total{rating="not_useful",reason="none"} 1`,
		`public_ai_security_signals_total{reason_class="PROMPT_INJECTION"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
}
