package release

import (
	"testing"

	"zoiko.io/public-ai-svc/internal/metrics"
)

// Doc 03 §22: loading a release publishes index-health gauges.
func TestIndexHealthPublished(t *testing.T) {
	_ = setup(t)
	mfs, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "public_ai_index_health" {
			continue
		}
		for _, m := range mf.GetMetric() {
			got[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
		}
	}
	if got["chunks"] <= 0 || got["sources"] <= 0 {
		t.Fatalf("index health: %v", got)
	}
	for _, k := range []string{"revocations", "open_conflicts", "oldest_approval_days"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing measure %s", k)
		}
	}
}
