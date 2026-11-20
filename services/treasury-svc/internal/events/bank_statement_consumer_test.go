// Package events_test covers BankStatementConsumer's dispatch, decoding and
// delivery-guarantee rules without a database or broker — mirrors
// bank-reconciliation-svc's own ConflictConsumer test shape exactly: a fake
// Processor reproduces the inbox's observable contract (one effect per
// event id, retry budget then quarantine, poison quarantines at once).
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/eventing/inbox"
	"zoiko.io/treasury-svc/internal/domain"
	"zoiko.io/treasury-svc/internal/events"
)

type fakeBalanceWriter struct {
	written []domain.CashBalance
}

func (f *fakeBalanceWriter) CreateCashBalanceInTx(_ context.Context, _ pgx.Tx, bal *domain.CashBalance) error {
	f.written = append(f.written, *bal)
	return nil
}

type fakeRecalculator struct {
	calls int
	err   error
}

func (f *fakeRecalculator) RecalculateAndPublishCashPosition(_ context.Context, _, _, _, _, _ string) (*domain.CashPositionSnapshot, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &domain.CashPositionSnapshot{SnapshotID: "snap-1"}, nil
}

// fakeProcessor reproduces the inbox's observable contract.
type fakeProcessor struct {
	maxAttempts int
	state       map[string]string
	attempts    map[string]int
	down        error
}

func newFakeProcessor(max int) *fakeProcessor {
	return &fakeProcessor{maxAttempts: max, state: map[string]string{}, attempts: map[string]int{}}
}

func (p *fakeProcessor) Process(ctx context.Context, msg inbox.Message, fn inbox.Handler) (inbox.Result, error) {
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

func newConsumer(balances *fakeBalanceWriter, recalc *fakeRecalculator, proc *fakeProcessor) *events.BankStatementConsumer {
	c := events.NewBankStatementConsumer(zap.NewNop(), balances, recalc, proc)
	c.SetRetryDelay(0)
	return c
}

func acceptedEvent(t *testing.T, eventID, tenantID string, payload map[string]interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := map[string]interface{}{
		"id":            eventID,
		"type":          "com.zoikosuite.banking.bank-statement.accepted",
		"event_id":      eventID,
		"event_type":    "banking.statement.accepted",
		"tenantid":      tenantID,
		"correlationid": "corr-1",
		"data":          json.RawMessage(data),
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

var okPayload = map[string]interface{}{
	"statement_id":    "stmt-1",
	"connection_id":   "conn-1",
	"tenant_id":       "tenant-a",
	"legal_entity_id": "entity-a",
	"bank_account_id": "acct-1",
	"closing_balance": 12345.67,
	"opening_balance": 10000.00,
	"currency":        "USD",
	"statement_date":  "2026-11-01T00:00:00Z",
	"accepted_at":     "2026-11-19T00:00:00Z",
}

func TestBankStatementConsumer_Handle_WritesRealBalance(t *testing.T) {
	balances := &fakeBalanceWriter{}
	recalc := &fakeRecalculator{}
	proc := newFakeProcessor(3)
	if err := newConsumer(balances, recalc, proc).Handle(context.Background(), acceptedEvent(t, "evt-1", "tenant-a", okPayload)); err != nil {
		t.Fatal(err)
	}
	if len(balances.written) != 1 {
		t.Fatalf("expected exactly 1 cash balance written, got %d", len(balances.written))
	}
	bal := balances.written[0]
	if bal.BankAccountID != "acct-1" || bal.TenantID != "tenant-a" {
		t.Errorf("bank_account_id/tenant_id: %s %s", bal.BankAccountID, bal.TenantID)
	}
	if bal.LedgerBalance != "12345.6700" || bal.AvailableBalance != "12345.6700" {
		t.Errorf("expected the real closing balance, got ledger=%s available=%s", bal.LedgerBalance, bal.AvailableBalance)
	}
	wantAsOf, _ := time.Parse(time.RFC3339, "2026-11-01T00:00:00Z")
	if !bal.AsOfTimestamp.Equal(wantAsOf) {
		t.Errorf("as_of_timestamp must be the statement's own date, not time.Now(): got %v want %v", bal.AsOfTimestamp, wantAsOf)
	}
	if recalc.calls != 1 {
		t.Errorf("expected recalculation to be attempted once, got %d", recalc.calls)
	}
}

func TestBankStatementConsumer_Handle_DuplicateDeliveryWritesOnce(t *testing.T) {
	balances := &fakeBalanceWriter{}
	recalc := &fakeRecalculator{}
	proc := newFakeProcessor(3)
	c := newConsumer(balances, recalc, proc)
	ev := acceptedEvent(t, "evt-dup", "tenant-a", okPayload)
	if err := c.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if err := c.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(balances.written) != 1 {
		t.Fatalf("redelivery must not write twice, got %d writes", len(balances.written))
	}
	if recalc.calls != 1 {
		t.Errorf("redelivery must not recalculate twice, got %d", recalc.calls)
	}
}

func TestBankStatementConsumer_Handle_IgnoresUnrelatedEventType(t *testing.T) {
	balances := &fakeBalanceWriter{}
	recalc := &fakeRecalculator{}
	proc := newFakeProcessor(3)
	ev := map[string]interface{}{
		"id": "evt-x", "type": "com.zoikosuite.banking.bank-statement.quarantined",
		"tenantid": "tenant-a", "data": json.RawMessage(`{}`),
	}
	b, _ := json.Marshal(ev)
	if err := newConsumer(balances, recalc, proc).Handle(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if len(balances.written) != 0 || recalc.calls != 0 {
		t.Fatalf("an unrelated event type must not be acted on")
	}
}

func TestBankStatementConsumer_Handle_RecalculationFailureDoesNotFailDelivery(t *testing.T) {
	balances := &fakeBalanceWriter{}
	recalc := &fakeRecalculator{err: errors.New("accounts-payable-svc unreachable")}
	proc := newFakeProcessor(3)
	if err := newConsumer(balances, recalc, proc).Handle(context.Background(), acceptedEvent(t, "evt-2", "tenant-a", okPayload)); err != nil {
		t.Fatal(err)
	}
	if len(balances.written) != 1 {
		t.Fatalf("the real balance must still be written even when recalculation fails, got %d", len(balances.written))
	}
}

func TestBankStatementConsumer_Handle_MissingBankAccountIDIsPoisoned(t *testing.T) {
	balances := &fakeBalanceWriter{}
	recalc := &fakeRecalculator{}
	proc := newFakeProcessor(3)
	bad := map[string]interface{}{"tenant_id": "tenant-a", "statement_id": "stmt-2"}
	if err := newConsumer(balances, recalc, proc).Handle(context.Background(), acceptedEvent(t, "evt-3", "tenant-a", bad)); err != nil {
		t.Fatal(err)
	}
	if len(balances.written) != 0 {
		t.Fatalf("a payload with no bank_account_id must never be written")
	}
	if proc.state["evt-3"] != inbox.StateQuarantine {
		t.Errorf("expected the delivery to be quarantined, got state %q", proc.state["evt-3"])
	}
}
