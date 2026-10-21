package ar09

import (
	"context"
	"errors"
	"sync"
)

// Notice is one customer communication handed to a Deliverer. Body and ArtifactHash are the recorded evidence.
type Notice struct {
	TenantID        string
	LegalEntityID   string
	CaseID          string
	CustomerPartyID string
	StageNo         int
	Channel         string
	TemplateKey     string
	Body            string
	ArtifactHash    string
	// IdempotencyKey is stable per (case, stage) so a provider can de-duplicate a retried hand-off.
	IdempotencyKey string
}

// Receipt is the channel's answer. It is communication evidence only: a receipt is NEVER a payment and no code path
// converts one into a balance, settlement or adjustment.
type Receipt struct {
	Delivered   bool // true = the channel confirmed delivery; false = accepted for delivery
	ProviderRef string
}

// Deliverer sends a notice to the customer (external party). An error is recorded as DELIVERY_FAILED; it never changes
// receivable truth.
type Deliverer interface {
	Deliver(ctx context.Context, n Notice) (Receipt, error)
}

// ErrDeliveryNotConfigured is returned by UnconfiguredDeliverer.
var ErrDeliveryNotConfigured = errors.New("customer communication channel is not configured")

// UnconfiguredDeliverer is the honest default: no channel is wired, so every notice is recorded DELIVERY_FAILED. It never
// fakes success.
//
// A real adapter needs a channel for EXTERNAL customers. notification-svc today only accepts internal recipients
// (POST /v1/notifications/events/ingest requires recipient_principal_id, a platform principal, and a template_key whose intent
// version is published), so it cannot address a customer contact by e-mail / postal address. A real adapter needs either
// (a) an external-recipient (contact address) variant of the ingest contract plus a published communication intent for each
// dunning template_key, or (b) a direct provider (SMTP / print-and-post) client; both must report an idempotent provider
// reference and honour the per-stage IdempotencyKey.
type UnconfiguredDeliverer struct{}

func (UnconfiguredDeliverer) Deliver(context.Context, Notice) (Receipt, error) {
	return Receipt{}, ErrDeliveryNotConfigured
}

// FakeDeliverer records notices for tests. Err, when set, makes every delivery fail.
type FakeDeliverer struct {
	mu        sync.Mutex
	Err       error
	Confirmed bool // report Delivered=true
	sent      []Notice
}

func (f *FakeDeliverer) Deliver(_ context.Context, n Notice) (Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return Receipt{}, f.Err
	}
	f.sent = append(f.sent, n)
	return Receipt{Delivered: f.Confirmed, ProviderRef: "fake-" + n.IdempotencyKey}, nil
}

// Sent returns a copy of the notices delivered so far.
func (f *FakeDeliverer) Sent() []Notice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Notice(nil), f.sent...)
}

// SetErr changes the failure mode.
func (f *FakeDeliverer) SetErr(err error) { f.mu.Lock(); f.Err = err; f.mu.Unlock() }
