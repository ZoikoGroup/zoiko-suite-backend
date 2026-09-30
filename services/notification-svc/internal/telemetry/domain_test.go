package telemetry

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// main.go registers Metrics and Domain on the same (default) registry. Two
// series with the same name and different label sets make MustRegister panic,
// which is a crash at startup, not a test failure — so this pins that the two
// sets coexist. The ported Domain originally reused
// notification_delivery_attempts_total and notification_delivery_duration_seconds.
func TestDomainAndMetricsRegisterTogether(t *testing.T) {
	reg := prometheus.NewRegistry()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("registering Metrics and Domain together panicked: %v", p)
		}
	}()
	NewMetricsWithRegisterer("notification-svc", reg)
	NewDomainWithRegisterer("notification-svc", reg)
}

// A nil *Domain is a valid no-op, so handler and worker tests need no metrics.
func TestDomainNilSafe(t *testing.T) {
	var d *Domain
	d.ObserveAttempt("EMAIL", OutcomeDelivered, OriginRequest, 0.1)
	d.ObserveConclusion("EMAIL", "SENT")
	d.ObserveRetryScheduled("EMAIL")
	d.ObserveRetryExhausted()
	d.ObserveStrandedReclaimed()
}
