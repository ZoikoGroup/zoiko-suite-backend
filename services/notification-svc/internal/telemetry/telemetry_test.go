package telemetry_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/notification-svc/internal/telemetry"
)

func getCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	err := counter.Write(&m)
	require.NoError(t, err)
	return m.GetCounter().GetValue()
}

func getHistogramCount(t *testing.T, observer prometheus.Observer) uint64 {
	t.Helper()
	metric, ok := observer.(prometheus.Metric)
	require.True(t, ok, "observer must implement prometheus.Metric")
	var m dto.Metric
	err := metric.Write(&m)
	require.NoError(t, err)
	return m.GetHistogram().GetSampleCount()
}

func TestTelemetry_DeliveryMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := telemetry.NewMetricsWithRegisterer("test-svc", reg)

	// 1. Observe delivery attempt & latency
	m.ObserveDelivery("MARKETING", "smtp-primary", "delivered", 0.125)

	counter := m.DeliveryAttemptsTotal.WithLabelValues("MARKETING", "smtp-primary", "delivered")
	assert.Equal(t, 1.0, getCounterValue(t, counter))

	hist := m.DeliveryDuration.WithLabelValues("MARKETING", "smtp-primary", "delivered")
	assert.Equal(t, uint64(1), getHistogramCount(t, hist))

	// 2. Observe failed delivery
	m.ObserveDelivery("TRANSACTIONAL", "smtp-secondary", "failed", 0.450)
	failedCounter := m.DeliveryAttemptsTotal.WithLabelValues("TRANSACTIONAL", "smtp-secondary", "failed")
	assert.Equal(t, 1.0, getCounterValue(t, failedCounter))
}

func TestTelemetry_IntentMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := telemetry.NewMetricsWithRegisterer("test-svc", reg)

	m.RecordIntent("OPERATIONAL", "A1", "dispatched")
	m.RecordIntent("MARKETING", "M1", "suppressed")

	valDispatched := getCounterValue(t, m.DeliveryIntentsTotal.WithLabelValues("OPERATIONAL", "A1", "dispatched"))
	assert.Equal(t, 1.0, valDispatched)

	valSuppressed := getCounterValue(t, m.DeliveryIntentsTotal.WithLabelValues("MARKETING", "M1", "suppressed"))
	assert.Equal(t, 1.0, valSuppressed)
}

func TestTelemetry_WebhookAndDLQMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := telemetry.NewMetricsWithRegisterer("test-svc", reg)

	// Webhook outcome events (bounce / complaint / delivered)
	m.RecordWebhookEvent("sendgrid", "bounce")
	m.RecordWebhookEvent("sendgrid", "complaint")
	m.RecordWebhookEvent("mailgun", "delivered")

	assert.Equal(t, 1.0, getCounterValue(t, m.WebhookEventsTotal.WithLabelValues("sendgrid", "bounce")))
	assert.Equal(t, 1.0, getCounterValue(t, m.WebhookEventsTotal.WithLabelValues("sendgrid", "complaint")))
	assert.Equal(t, 1.0, getCounterValue(t, m.WebhookEventsTotal.WithLabelValues("mailgun", "delivered")))

	// Webhook DLQ volume
	m.RecordDLQ("sendgrid", "routed")
	m.RecordDLQ("sendgrid", "reprocessed")

	assert.Equal(t, 1.0, getCounterValue(t, m.WebhookDLQTotal.WithLabelValues("sendgrid", "routed")))
	assert.Equal(t, 1.0, getCounterValue(t, m.WebhookDLQTotal.WithLabelValues("sendgrid", "reprocessed")))
}

func TestTelemetry_NilSafety(t *testing.T) {
	var m *telemetry.Metrics
	// Must not panic on nil receiver
	assert.NotPanics(t, func() {
		m.ObserveDelivery("M1", "smtp", "ok", 0.1)
		m.RecordIntent("M1", "S0", "ok")
		m.RecordWebhookEvent("smtp", "bounce")
		m.RecordDLQ("smtp", "routed")
	})
}
