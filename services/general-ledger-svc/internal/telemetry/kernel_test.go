package telemetry

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"zoiko.io/general-ledger-svc/internal/kernel"
)

var _ kernel.Observer = (*KernelMetrics)(nil)

// counter reads one series of a counter from the default registry, matching every label given.
func counter(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
	series:
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range want {
				if got[k] != v {
					continue series
				}
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue()
			}
			return float64(m.GetHistogram().GetSampleCount())
		}
	}
	return 0
}

func TestKernelMetricsCountByOutcomeFamilyAndStatus(t *testing.T) {
	m := NewKernelMetrics("general-ledger-svc")
	m.Command("post_event", kernel.OutcomeOK, 20*time.Millisecond)
	m.Command("post_event", kernel.OutcomeExisting, time.Millisecond)
	m.Command("post_event", kernel.OutcomeExisting, time.Millisecond)
	m.Exception("AK-BAL", "UNBALANCED")
	m.ReconciliationResult("AR_GL", "BREAK")

	svc := map[string]string{"service": "general-ledger-svc"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range svc {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	if got := counter(t, "gl_kernel_commands_total", with(map[string]string{"command": "post_event", "outcome": "existing"})); got != 2 {
		t.Fatalf("duplicate-prevention hits: %v", got)
	}
	if got := counter(t, "gl_kernel_exceptions_total", with(map[string]string{"family": "AK-BAL", "code": "UNBALANCED"})); got != 1 {
		t.Fatalf("imbalance failures: %v", got)
	}
	if got := counter(t, "gl_kernel_reconciliation_runs_total", with(map[string]string{"type": "AR_GL", "status": "BREAK"})); got != 1 {
		t.Fatalf("breaks: %v", got)
	}
	if got := counter(t, "gl_kernel_command_duration_seconds", with(map[string]string{"command": "post_event"})); got != 3 {
		t.Fatalf("every command is timed: %v", got)
	}
}
