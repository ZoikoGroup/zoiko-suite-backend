// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires (event_version, tenant_id, legal_entity_id,
// actor_id), sourced from real data on domain.VendorInvoice — not left
// empty or fabricated.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
	"zoiko.io/accounts-payable-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SourceService string `json:"source_service"`
	CorrelationID string `json:"correlation_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
}

func decodeOne(t *testing.T, w *fakeWriter) envelope {
	t.Helper()
	require.Len(t, w.msgs, 1)
	var env envelope
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &env))
	return env
}

func invoice() domain.VendorInvoice {
	validatedBy := "validator-1"
	approvedBy := "approver-1"
	paymentBy := "payer-1"
	return domain.VendorInvoice{
		InvoiceID:                     "invoice-1",
		TenantID:                      "tenant-1",
		LegalEntityID:                 "entity-1",
		VendorID:                      "vendor-1",
		Amount:                        1000,
		CurrencyCode:                  "USD",
		CorrelationID:                 "corr-1",
		CreatedByPrincipalID:          "creator-1",
		ValidatedByPrincipalID:        &validatedBy,
		ApprovedByPrincipalID:         &approvedBy,
		PaymentRequestedByPrincipalID: &paymentBy,
		CreatedAt:                     time.Now().UTC(),
	}
}

func TestPublishVendorInvoiceReceived_EnvelopeCarriesCreator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.accounts-payable.events", w)

	p.PublishVendorInvoiceReceived(context.Background(), invoice())

	env := decodeOne(t, w)
	assert.Equal(t, "vendor.invoice.received", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "creator-1", env.ActorID)
}

// TestPublishPaymentRequested_EnvelopeCarriesPaymentRequester is the
// important one: payment initiation is doc §3.7's #1 named mandatory
// idempotency/evidence case, and before this fix the envelope carried no
// actor at all — meaning the audit trail for who actually triggered a
// vendor payment request lived nowhere on the event itself.
func TestPublishPaymentRequested_EnvelopeCarriesPaymentRequester(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.accounts-payable.events", w)
	inv := invoice()

	p.PublishPaymentRequested(context.Background(), inv)

	env := decodeOne(t, w)
	assert.Equal(t, "payment.requested", env.EventType)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "payer-1", env.ActorID)
	assert.NotEqual(t, inv.CreatedByPrincipalID, env.ActorID, "must attribute whoever requested PAYMENT, not whoever created the invoice")
}

func TestPublishVendorInvoiceApproved_EnvelopeCarriesApprover(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.accounts-payable.events", w)

	p.PublishVendorInvoiceApproved(context.Background(), invoice())

	env := decodeOne(t, w)
	assert.Equal(t, "approver-1", env.ActorID)
}

// TestPublishPaymentRequested_NilActorPointer_OmitsRatherThanPanics — this
// should not realistically happen (requesting payment implies
// PaymentRequestedByPrincipalID was just set), but a nil pointer must never
// panic the publish path.
func TestPublishPaymentRequested_NilActorPointer_OmitsRatherThanPanics(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.accounts-payable.events", w)
	inv := invoice()
	inv.PaymentRequestedByPrincipalID = nil

	assert.NotPanics(t, func() {
		p.PublishPaymentRequested(context.Background(), inv)
	})
	env := decodeOne(t, w)
	assert.Empty(t, env.ActorID)
}

func TestPublishOutbox_KeyAndHeader(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.accounts-payable.events", w)

	outboxEventID := "11111111-2222-3333-4444-555555555555"
	aggregateID := "inv-999"
	payload := []byte(`{"event_type":"vendor.invoice.received","payload":{"invoice_id":"inv-999"}}`)

	err := p.PublishOutbox(context.Background(), outboxEventID, aggregateID, payload)
	require.NoError(t, err)
	require.Len(t, w.msgs, 1)

	msg := w.msgs[0]
	assert.Equal(t, "zoiko.accounts-payable.events", msg.Topic)
	assert.Equal(t, []byte(aggregateID), msg.Key)
	assert.Equal(t, payload, msg.Value)

	foundHeader := false
	for _, h := range msg.Headers {
		if h.Key == "X-Event-ID" {
			foundHeader = true
			assert.Equal(t, []byte(outboxEventID), h.Value)
		}
	}
	assert.True(t, foundHeader, "expected X-Event-ID header to be set")
}

func TestPublishOutbox_PropagatesError(t *testing.T) {
	expectedErr := errors.New("kafka broken pipe")
	w := &fakeWriter{err: expectedErr}
	p := events.NewPublisher(zap.NewNop(), "zoiko.accounts-payable.events", w)

	err := p.PublishOutbox(context.Background(), "id-1", "inv-1", []byte("{}"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, expectedErr) || strings.Contains(err.Error(), "kafka broken pipe"))
}
