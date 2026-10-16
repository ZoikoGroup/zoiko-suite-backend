// Package events_test covers ConflictConsumer's dispatch, refusal and
// delivery-guarantee rules. The inbox itself (receipt + effect in one
// transaction) is proven against real Postgres in zoiko.io/eventing/inbox and in
// internal/store's integration tests; here a fake Processor mimics its
// observable behaviour so the consumer's own decisions can be tested without a
// database.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
	"zoiko.io/bank-reconciliation-svc/internal/events"
	"zoiko.io/eventing/inbox"
)

type raisedCall struct {
	tenantID string
	req      domain.RaiseEvidenceConflictRequest
}

type fakeConflictStore struct {
	lines    map[string]*domain.StatementLine
	raised   []raisedCall
	raiseFailures int // the next N raises fail transiently
}

func newFakeConflictStore() *fakeConflictStore {
	return &fakeConflictStore{lines: map[string]*domain.StatementLine{}}
}

func (f *fakeConflictStore) GetStatementLine(_ context.Context, statementLineID string) (*domain.StatementLine, error) {
	return f.lines[statementLineID], nil
}

func (f *fakeConflictStore) RaiseEvidenceConflictInTx(_ context.Context, _ pgx.Tx, tenantID string, req domain.RaiseEvidenceConflictRequest) (*domain.EvidenceConflict, bool, error) {
	if f.raiseFailures > 0 {
		f.raiseFailures--
		return nil, false, errors.New("serialization failure")
	}
	f.raised = append(f.raised, raisedCall{tenantID: tenantID, req: req})
	return &domain.EvidenceConflict{ConflictID: "c-1", TenantID: tenantID, StatementLineID: req.StatementLineID, PaymentID: req.PaymentID}, true, nil
}

// fakeProcessor reproduces the inbox's observable contract: one effect per
// event id, retry budget then quarantine, poison quarantines at once, and a
// quarantined event is held.
type fakeProcessor struct {
	maxAttempts int
	state       map[string]string
	attempts    map[string]int
	calls       int
	down        error // the inbox cannot be reached
}

func newFakeProcessor(max int) *fakeProcessor {
	return &fakeProcessor{maxAttempts: max, state: map[string]string{}, attempts: map[string]int{}}
}

func (p *fakeProcessor) Process(ctx context.Context, msg inbox.Message, fn inbox.Handler) (inbox.Result, error) {
	p.calls++
	if p.down != nil {
		return inbox.Result{}, p.down
	}
	switch p.state[msg.EventID] {
	case inbox.StateCompleted:
		return inbox.Result{Outcome: inbox.Duplicate}, nil
	case inbox.StateQuarantine:
		return inbox.Result{Outcome: inbox.Held}, nil
	}
	p.attempts[msg.EventID]++
	ref, err := fn(ctx, nil, inbox.Receipt{EventID: msg.EventID, TenantID: msg.TenantID, Attempt: p.attempts[msg.EventID]})
	if err == nil {
		p.state[msg.EventID] = inbox.StateCompleted
		return inbox.Result{Outcome: inbox.Processed, ResultRef: ref, Attempt: p.attempts[msg.EventID]}, nil
	}
	st := inbox.StateFailed
	if errors.Is(err, inbox.ErrPoison) || p.attempts[msg.EventID] >= p.maxAttempts {
		st = inbox.StateQuarantine
	}
	p.state[msg.EventID] = st
	return inbox.Result{FailureState: st, Attempt: p.attempts[msg.EventID]}, err
}

func newConsumer(store *fakeConflictStore, p *fakeProcessor) *events.ConflictConsumer {
	c := events.NewConflictConsumer(zap.NewNop(), store, p)
	c.SetRetryDelay(0)
	return c
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func conflictEvent(t *testing.T, eventID, tenantID string, payload interface{}) []byte {
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

func knownLine(store *fakeConflictStore) {
	store.lines["line-1"] = &domain.StatementLine{StatementLineID: "line-1", TenantID: "tenant-a", LegalEntityID: "entity-a", Status: domain.StatementLineStatusMatched}
}

var okPayload = map[string]interface{}{
	"payment_id": "pay-1", "statement_line_id": "line-1",
	"provider_request_id": "prov-1", "bank_rec_status": "MATCHED",
	"reported_status": "SETTLED", "current_status": "REJECTED",
}

func TestConflictConsumer_Handle_RaisesConflictForKnownLine(t *testing.T) {
	store := newFakeConflictStore()
	knownLine(store)
	proc := newFakeProcessor(3)
	if err := newConsumer(store, proc).Handle(context.Background(), conflictEvent(t, "evt-1", "tenant-a", okPayload)); err != nil {
		t.Fatal(err)
	}
	if len(store.raised) != 1 {
		t.Fatalf("expected exactly 1 raise, got %d", len(store.raised))
	}
	got := store.raised[0]
	if got.tenantID != "tenant-a" || got.req.LegalEntityID != "entity-a" {
		t.Errorf("tenant/entity: %s %s", got.tenantID, got.req.LegalEntityID)
	}
	if got.req.PaymentID != "pay-1" || got.req.StatementLineID != "line-1" || got.req.ProviderConfirmedStatus != "REJECTED" || got.req.SourceEventID != "evt-1" {
		t.Errorf("unexpected conflict request: %+v", got.req)
	}
	if proc.state["evt-1"] != inbox.StateCompleted {
		t.Error("the receipt must be recorded as completed")
	}
}

// A replayed event (same event_id) must not raise a second conflict.
func TestConflictConsumer_Handle_ReplayRaisesNothingTwice(t *testing.T) {
	store := newFakeConflictStore()
	knownLine(store)
	c := newConsumer(store, newFakeProcessor(3))
	raw := conflictEvent(t, "evt-1", "tenant-a", okPayload)
	for i := 0; i < 3; i++ {
		if err := c.Handle(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.raised) != 1 {
		t.Fatalf("expected one raise across three deliveries, got %d", len(store.raised))
	}
}

// A line this tenant does not own is retried (it may simply not be imported
// yet) and then quarantined — never raised against, never silently dropped.
func TestConflictConsumer_Handle_UnknownStatementLine_RetriedThenQuarantined(t *testing.T) {
	store := newFakeConflictStore()
	proc := newFakeProcessor(3)
	err := newConsumer(store, proc).Handle(context.Background(), conflictEvent(t, "evt-1", "tenant-a",
		map[string]interface{}{"payment_id": "pay-1", "statement_line_id": "line-does-not-exist", "current_status": "REJECTED"}))
	if err != nil {
		t.Fatalf("a quarantined delivery has a disposition and must be acknowledged: %v", err)
	}
	if len(store.raised) != 0 {
		t.Fatalf("expected no conflict for an unknown statement line, got %d", len(store.raised))
	}
	if proc.state["evt-1"] != inbox.StateQuarantine || proc.attempts["evt-1"] != 3 {
		t.Fatalf("state=%s attempts=%d, want quarantined after the 3-attempt budget", proc.state["evt-1"], proc.attempts["evt-1"])
	}
}

// A transient failure on the first attempt must not strand the event: the retry
// applies it exactly once.
func TestConflictConsumer_Handle_TransientFailureRetriedOnce(t *testing.T) {
	store := newFakeConflictStore()
	knownLine(store)
	store.raiseFailures = 1
	proc := newFakeProcessor(5)
	if err := newConsumer(store, proc).Handle(context.Background(), conflictEvent(t, "evt-1", "tenant-a", okPayload)); err != nil {
		t.Fatal(err)
	}
	if len(store.raised) != 1 || proc.attempts["evt-1"] != 2 || proc.state["evt-1"] != inbox.StateCompleted {
		t.Fatalf("raised=%d attempts=%d state=%s, want 1 raise on attempt 2", len(store.raised), proc.attempts["evt-1"], proc.state["evt-1"])
	}
}

func TestConflictConsumer_Handle_PoisonPayloadQuarantinedAtOnce(t *testing.T) {
	for name, payload := range map[string]interface{}{
		"no statement line id": map[string]interface{}{"payment_id": "pay-1"},
		"no payment id":        map[string]interface{}{"statement_line_id": "line-1"},
		"wrong payload shape":  "not an object",
	} {
		store := newFakeConflictStore()
		knownLine(store)
		proc := newFakeProcessor(5)
		if err := newConsumer(store, proc).Handle(context.Background(), conflictEvent(t, "evt-p", "tenant-a", payload)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if proc.state["evt-p"] != inbox.StateQuarantine || proc.attempts["evt-p"] != 1 || len(store.raised) != 0 {
			t.Errorf("%s: state=%s attempts=%d raised=%d, want quarantined after 1 attempt", name, proc.state["evt-p"], proc.attempts["evt-p"], len(store.raised))
		}
	}
}

func TestConflictConsumer_Handle_HeldEventIsNotProcessed(t *testing.T) {
	store := newFakeConflictStore()
	knownLine(store)
	proc := newFakeProcessor(3)
	proc.state["evt-1"] = inbox.StateQuarantine
	if err := newConsumer(store, proc).Handle(context.Background(), conflictEvent(t, "evt-1", "tenant-a", okPayload)); err != nil {
		t.Fatal(err)
	}
	if len(store.raised) != 0 {
		t.Fatal("a quarantined event needs an approved repair; redelivery must not process it")
	}
}

// If the inbox cannot be reached the delivery has no disposition: Handle must
// say so, so the Kafka offset stays uncommitted and nothing is lost.
func TestConflictConsumer_Handle_InboxUnavailable_NotAcknowledged(t *testing.T) {
	store := newFakeConflictStore()
	knownLine(store)
	proc := newFakeProcessor(3)
	proc.down = errors.New("connection refused")
	c := newConsumer(store, proc)
	raw := conflictEvent(t, "evt-1", "tenant-a", okPayload)
	if err := c.Handle(context.Background(), raw); err == nil {
		t.Fatal("an unrecorded delivery must not be acknowledged")
	}
	if len(store.raised) != 0 {
		t.Fatal("must not process without a receipt")
	}
	proc.down = nil
	if err := c.Handle(context.Background(), raw); err != nil || len(store.raised) != 1 {
		t.Fatalf("redelivery after recovery must process once: %v raised=%d", err, len(store.raised))
	}
}

// Other event types on the same topic are ignored without touching the inbox.
func TestConflictConsumer_Handle_IgnoresOtherEventTypes(t *testing.T) {
	store := newFakeConflictStore()
	proc := newFakeProcessor(3)
	raw := mustJSON(t, map[string]interface{}{
		"event_id": "evt-1", "event_type": "PAYMENT_STATUS_CONFLICT_RESOLVED", "tenant_id": "tenant-a",
		"payload": map[string]interface{}{},
	})
	if err := newConsumer(store, proc).Handle(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if len(store.raised) != 0 || proc.calls != 0 {
		t.Fatalf("unrelated event touched the consumer: raised=%d inbox calls=%d", len(store.raised), proc.calls)
	}
}

func TestConflictConsumer_Handle_MissingTenantOrEventID_Dropped(t *testing.T) {
	for name, raw := range map[string][]byte{
		"no tenant":   conflictEvent(t, "evt-1", "", okPayload),
		"no event id": conflictEvent(t, "", "tenant-a", okPayload),
		"not json":    []byte(`{`),
	} {
		store := newFakeConflictStore()
		knownLine(store)
		proc := newFakeProcessor(3)
		if err := newConsumer(store, proc).Handle(context.Background(), raw); err != nil {
			t.Fatalf("%s: an unidentifiable event cannot be retried, got %v", name, err)
		}
		if len(store.raised) != 0 || proc.calls != 0 {
			t.Errorf("%s: raised=%d inbox calls=%d, want nothing", name, len(store.raised), proc.calls)
		}
	}
}
