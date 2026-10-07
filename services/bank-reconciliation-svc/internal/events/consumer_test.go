// Package events_test covers ConflictConsumer's dispatch and refusal
// rules. Before this consumer existed, payment-status-svc's own
// PAYMENT_STATUS_CONFLICT_RAISED event had no subscriber at all, so every
// assertion here is about behavior that previously never ran.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/events"
)

type raisedCall struct {
	tenantID string
	req      domain.RaiseEvidenceConflictRequest
}

type fakeConflictStore struct {
	lines map[string]*domain.StatementLine

	raised    []raisedCall
	raiseErr  error
	processed map[string]bool
	markErr   error
	isProcErr error
}

func newFakeConflictStore() *fakeConflictStore {
	return &fakeConflictStore{
		lines:     map[string]*domain.StatementLine{},
		processed: map[string]bool{},
	}
}

func (f *fakeConflictStore) GetStatementLine(_ context.Context, statementLineID string) (*domain.StatementLine, error) {
	l, ok := f.lines[statementLineID]
	if !ok {
		return nil, nil
	}
	return l, nil
}

func (f *fakeConflictStore) RaiseEvidenceConflict(_ context.Context, tenantID string, req domain.RaiseEvidenceConflictRequest) (*domain.EvidenceConflict, bool, error) {
	if f.raiseErr != nil {
		return nil, false, f.raiseErr
	}
	f.raised = append(f.raised, raisedCall{tenantID: tenantID, req: req})
	return &domain.EvidenceConflict{ConflictID: "c-1", TenantID: tenantID, StatementLineID: req.StatementLineID, PaymentID: req.PaymentID}, true, nil
}

func (f *fakeConflictStore) IsEventProcessed(_ context.Context, _, eventID string) (bool, error) {
	if f.isProcErr != nil {
		return false, f.isProcErr
	}
	return f.processed[eventID], nil
}

func (f *fakeConflictStore) MarkEventProcessed(_ context.Context, _, eventID string) error {
	if f.markErr != nil {
		return f.markErr
	}
	f.processed[eventID] = true
	return nil
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func conflictEvent(t *testing.T, eventID, tenantID string, payload map[string]interface{}) []byte {
	t.Helper()
	return mustJSON(t, map[string]interface{}{
		"event_id":       eventID,
		"event_type":     "PAYMENT_STATUS_CONFLICT_RAISED",
		"tenant_id":      tenantID,
		"actor_id":       "payment-status-svc",
		"correlation_id": "corr-1",
		"payload":        payload,
	})
}

func TestConflictConsumer_Handle_RaisesConflictForKnownLine(t *testing.T) {
	store := newFakeConflictStore()
	store.lines["line-1"] = &domain.StatementLine{StatementLineID: "line-1", TenantID: "tenant-a", LegalEntityID: "entity-a", Status: domain.StatementLineStatusMatched}
	c := events.NewConflictConsumer(zap.NewNop(), store)

	raw := conflictEvent(t, "evt-1", "tenant-a", map[string]interface{}{
		"payment_id": "pay-1", "statement_line_id": "line-1",
		"provider_request_id": "prov-1", "bank_rec_status": "MATCHED",
		"reported_status": "SETTLED", "current_status": "REJECTED",
	})
	c.Handle(context.Background(), raw)

	if len(store.raised) != 1 {
		t.Fatalf("expected exactly 1 RaiseEvidenceConflict call, got %d", len(store.raised))
	}
	got := store.raised[0]
	if got.tenantID != "tenant-a" {
		t.Errorf("expected tenant_id=tenant-a, got %s", got.tenantID)
	}
	if got.req.LegalEntityID != "entity-a" {
		t.Errorf("expected legal_entity_id resolved from the statement line (entity-a), got %s", got.req.LegalEntityID)
	}
	if got.req.PaymentID != "pay-1" || got.req.StatementLineID != "line-1" {
		t.Errorf("unexpected conflict request: %+v", got.req)
	}
	if got.req.ProviderConfirmedStatus != "REJECTED" {
		t.Errorf("expected provider_confirmed_status=REJECTED, got %s", got.req.ProviderConfirmedStatus)
	}
	if got.req.SourceEventID != "evt-1" {
		t.Errorf("expected source_event_id=evt-1, got %s", got.req.SourceEventID)
	}
	if !store.processed["evt-1"] {
		t.Error("expected the event to be marked processed in the inbox after a successful raise")
	}
}

// TestConflictConsumer_Handle_SkipsAlreadyProcessedEvent proves the
// inbox-based idempotency: a replayed event (same event_id) must not
// raise a second conflict.
func TestConflictConsumer_Handle_SkipsAlreadyProcessedEvent(t *testing.T) {
	store := newFakeConflictStore()
	store.lines["line-1"] = &domain.StatementLine{StatementLineID: "line-1", TenantID: "tenant-a", LegalEntityID: "entity-a"}
	store.processed["evt-1"] = true
	c := events.NewConflictConsumer(zap.NewNop(), store)

	raw := conflictEvent(t, "evt-1", "tenant-a", map[string]interface{}{
		"payment_id": "pay-1", "statement_line_id": "line-1", "current_status": "REJECTED",
	})
	c.Handle(context.Background(), raw)

	if len(store.raised) != 0 {
		t.Fatalf("expected the replayed event to raise nothing, got %d calls", len(store.raised))
	}
}

// TestConflictConsumer_Handle_UnknownStatementLine_DoesNotRaise proves a
// conflict is never raised against a line this tenant doesn't actually
// own — the consumer resolves legal_entity_id from ITS OWN record, not
// from the event, specifically to make this refusal possible.
func TestConflictConsumer_Handle_UnknownStatementLine_DoesNotRaise(t *testing.T) {
	store := newFakeConflictStore()
	c := events.NewConflictConsumer(zap.NewNop(), store)

	raw := conflictEvent(t, "evt-1", "tenant-a", map[string]interface{}{
		"payment_id": "pay-1", "statement_line_id": "line-does-not-exist", "current_status": "REJECTED",
	})
	c.Handle(context.Background(), raw)

	if len(store.raised) != 0 {
		t.Fatalf("expected no conflict raised for an unknown statement line, got %d calls", len(store.raised))
	}
}

// TestConflictConsumer_Handle_IgnoresOtherEventTypes proves the consumer
// only acts on PAYMENT_STATUS_CONFLICT_RAISED — any other event type on
// the same topic is silently ignored, not an error.
func TestConflictConsumer_Handle_IgnoresOtherEventTypes(t *testing.T) {
	store := newFakeConflictStore()
	c := events.NewConflictConsumer(zap.NewNop(), store)

	raw := mustJSON(t, map[string]interface{}{
		"event_id": "evt-1", "event_type": "PAYMENT_STATUS_CONFLICT_RESOLVED", "tenant_id": "tenant-a",
		"payload": map[string]interface{}{},
	})
	c.Handle(context.Background(), raw)

	if len(store.raised) != 0 {
		t.Fatalf("expected an unrelated event type to raise nothing, got %d calls", len(store.raised))
	}
}

func TestConflictConsumer_Handle_MissingTenantID_Dropped(t *testing.T) {
	store := newFakeConflictStore()
	c := events.NewConflictConsumer(zap.NewNop(), store)

	raw := conflictEvent(t, "evt-1", "", map[string]interface{}{
		"payment_id": "pay-1", "statement_line_id": "line-1", "current_status": "REJECTED",
	})
	c.Handle(context.Background(), raw)

	if len(store.raised) != 0 {
		t.Fatalf("expected an event with no tenant_id to raise nothing, got %d calls", len(store.raised))
	}
}
