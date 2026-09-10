package consumer_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/financial-close-svc/internal/consumer"
	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

type fakeStore struct {
	edges     []*domain.LineageEdge
	tenantIDs []string
	err       error
}

func (f *fakeStore) RecordLineageEdge(ctx context.Context, edge *domain.LineageEdge) error {
	if f.err != nil {
		return f.err
	}
	f.edges = append(f.edges, edge)
	f.tenantIDs = append(f.tenantIDs, svcmiddleware.TenantFromContext(ctx))
	return nil
}

func newConsumer(t *testing.T) (*consumer.Consumer, *fakeStore) {
	t.Helper()
	fake := &fakeStore{}
	log, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("zap.NewDevelopment: %v", err)
	}
	return consumer.New(fake, log), fake
}

func envelopeJSON(t *testing.T, eventType, tenantID, legalEntityID string, payload map[string]any) []byte {
	t.Helper()
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := map[string]any{
		"event_type":      eventType,
		"tenant_id":       tenantID,
		"legal_entity_id": legalEntityID,
		"payload":         json.RawMessage(rawPayload),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// ── One test per source event_type ───────────────────────────────────────────

func TestHandle_DepreciationRunEmitted_RecordsEdge(t *testing.T) {
	c, fake := newConsumer(t)
	raw := envelopeJSON(t, "depreciation.run.accounting_event.emitted", "t-1", "le-1", map[string]any{
		"run_id": "run-dep-1", "journal_id": "jnl-1",
	})
	if err := c.Handle(context.Background(), "evt-1", raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fake.edges) != 1 {
		t.Fatalf("expected 1 edge recorded, got %d", len(fake.edges))
	}
	e := fake.edges[0]
	if e.FromType != "depreciation_run" || e.FromID != "run-dep-1" || e.ToType != "journal" || e.ToID != "jnl-1" || e.LegalEntityID != "le-1" {
		t.Fatalf("unexpected edge: %+v", e)
	}
	if fake.tenantIDs[0] != "t-1" {
		t.Fatalf("expected tenant context injected from the envelope's own tenant_id, got %q", fake.tenantIDs[0])
	}
}

func TestHandle_AssetEventEmitted_UsesEventIDAsFromID(t *testing.T) {
	c, fake := newConsumer(t)
	raw := envelopeJSON(t, "asset.event.accounting_event.emitted", "t-1", "le-1", map[string]any{
		"event_id": "evt-asset-1", "journal_id": "jnl-2",
	})
	if err := c.Handle(context.Background(), "evt-2", raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fake.edges) != 1 {
		t.Fatalf("expected 1 edge recorded, got %d", len(fake.edges))
	}
	e := fake.edges[0]
	if e.FromType != "asset_event" || e.FromID != "evt-asset-1" || e.ToID != "jnl-2" {
		t.Fatalf("unexpected edge: %+v", e)
	}
}

func TestHandle_InventoryAccountingEventEmitted_RecordsEdge(t *testing.T) {
	c, fake := newConsumer(t)
	raw := envelopeJSON(t, "inventory.accounting_event.emitted", "t-1", "le-1", map[string]any{
		"run_id": "run-val-1", "journal_id": "jnl-3",
	})
	if err := c.Handle(context.Background(), "evt-3", raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fake.edges) != 1 || fake.edges[0].FromType != "inventory_valuation_run" {
		t.Fatalf("expected 1 inventory_valuation_run edge, got %+v", fake.edges)
	}
}

func TestHandle_ProjectRecognitionAccountingEventEmitted_RecordsEdge(t *testing.T) {
	c, fake := newConsumer(t)
	raw := envelopeJSON(t, "project.recognition.accounting_event.emitted", "t-1", "le-1", map[string]any{
		"run_id": "run-rec-1", "journal_id": "jnl-4",
	})
	if err := c.Handle(context.Background(), "evt-4", raw); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fake.edges) != 1 || fake.edges[0].FromType != "project_recognition_run" {
		t.Fatalf("expected 1 project_recognition_run edge, got %+v", fake.edges)
	}
}

// ── Irrelevant events on the same topics are ignored, not errors ────────────

func TestHandle_UnrelatedEventType_Ignored(t *testing.T) {
	c, fake := newConsumer(t)
	raw := envelopeJSON(t, "asset.registered", "t-1", "le-1", map[string]any{"asset_id": "AST-1"})
	if err := c.Handle(context.Background(), "evt-5", raw); err != nil {
		t.Fatalf("expected nil error for an unrelated event_type, got %v", err)
	}
	if len(fake.edges) != 0 {
		t.Fatalf("expected no edges recorded for an unrelated event, got %d", len(fake.edges))
	}
}

// ── Malformed / incomplete messages are real errors (retried, then DLQ'd) ───

func TestHandle_MalformedEnvelope_ReturnsError(t *testing.T) {
	c, _ := newConsumer(t)
	if err := c.Handle(context.Background(), "evt-6", []byte("not json")); err == nil {
		t.Fatal("expected an error for a malformed envelope")
	}
}

func TestHandle_MissingJournalID_ReturnsError(t *testing.T) {
	c, fake := newConsumer(t)
	raw := envelopeJSON(t, "project.recognition.accounting_event.emitted", "t-1", "le-1", map[string]any{
		"run_id": "run-rec-2",
	})
	if err := c.Handle(context.Background(), "evt-7", raw); err == nil {
		t.Fatal("expected an error for a missing journal_id")
	}
	if len(fake.edges) != 0 {
		t.Fatalf("expected no edge recorded for an incomplete event, got %d", len(fake.edges))
	}
}

func TestHandle_MissingTenantID_ReturnsError(t *testing.T) {
	c, _ := newConsumer(t)
	raw := envelopeJSON(t, "project.recognition.accounting_event.emitted", "", "le-1", map[string]any{
		"run_id": "run-rec-3", "journal_id": "jnl-5",
	})
	if err := c.Handle(context.Background(), "evt-8", raw); err == nil {
		t.Fatal("expected an error for a missing tenant_id — the store layer would otherwise fail closed with ErrIdentityMissing anyway")
	}
}

// ── Store failure propagates so Runner retries/dead-letters ─────────────────

func TestHandle_StoreFailure_ReturnsError(t *testing.T) {
	log, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("zap.NewDevelopment: %v", err)
	}
	fake := &fakeStore{err: errors.New("db unavailable")}
	c := consumer.New(fake, log)
	raw := envelopeJSON(t, "project.recognition.accounting_event.emitted", "t-1", "le-1", map[string]any{
		"run_id": "run-rec-4", "journal_id": "jnl-6",
	})
	if err := c.Handle(context.Background(), "evt-9", raw); err == nil {
		t.Fatal("expected the store error to propagate")
	}
}
