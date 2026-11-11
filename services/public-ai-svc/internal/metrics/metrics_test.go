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
	CommitRevocation("kb-rules-test", "GB")
	ReleaseRejected("checksum")
	TurnOutcome("ANSWERED", "kb-rules-test", "GB")
	TurnCancelled()
	EventDLPBlocked("rules-test")
	IndexHealth(1, 1, 0, 0, 1)
	CTAOffered("rules-test")
	HandoffClicked("rules-test")
	Feedback("useful", "")
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	exported := w.Body.String()

	var used []string
	for _, f := range []string{
		"../../../../deployments/prometheus-rules-public-ai.yml",
		// Doc 08 §15 (H17): the dashboard reads the same names.
		"../../../../deployments/grafana/provisioning/dashboards/public-ai-svc.json",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		used = append(used, regexp.MustCompile(`public_ai_[a-z_]+`).FindAllString(string(b), -1)...)
	}
	if len(used) < 8 {
		t.Fatalf("rules reference only %d metrics", len(used))
	}
	for _, m := range used {
		if strings.HasPrefix(m, "public_ai_canary_") {
			continue // written by cmd/canary -metrics-file (textfile collector), checked there
		}
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

// OPR-019 (Doc 08 §13): metric labels are fixed enums. A label that names a
// visitor, session, request or free text would explode cardinality and leak
// identifiers into monitoring, so none may exist on any exported metric.
func TestNoHighCardinalityLabels(t *testing.T) {
	CommitRevocation("kb-1", "not-a-market-code")
	TurnOutcome("ANSWERED", "", "united kingdom")
	forbidden := regexp.MustCompile(`(?i)^(?:session(?:_id)?|request(?:_id)?|turn(?:_id)?|trace(?:_id)?|span(?:_id)?|user(?:_id)?|visitor(?:_id)?|ip|client_ip|email|phone|message|text|query|path|url|grant|token)$`)
	mfs, err := Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if forbidden.MatchString(l.GetName()) {
					t.Errorf("%s has high-cardinality label %q", mf.GetName(), l.GetName())
				}
				if l.GetName() == "market" && l.GetValue() != "none" && !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(l.GetValue()) {
					t.Errorf("%s market label %q is not a bounded ISO code", mf.GetName(), l.GetValue())
				}
				if l.GetName() == "release" && l.GetValue() == "" {
					t.Errorf("%s has an empty release label", mf.GetName())
				}
			}
		}
	}
}
