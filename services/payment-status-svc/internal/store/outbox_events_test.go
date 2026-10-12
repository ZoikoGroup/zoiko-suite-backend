//go:build integration

// Package store_test — proof that payment-status-svc's BNK-07 status
// events actually reach the transactional outbox (ZS-EVENT-001 §6), inside
// the same transaction as the state change they report.
//
// Before this file, event emission for these facts lived in
// internal/events.Publisher, called from the HANDLER layer AFTER the store
// transaction had already committed — a direct, synchronous Kafka write
// that could silently lose an event on a broker outage or a crash between
// the commit and the publish call. Moving the enqueue into the store's own
// transaction is the fix; this file is the real proof it works.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/ -run TestPgStore_Outbox
package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/payment-status-svc/internal/domain"
	"zoiko.io/payment-status-svc/internal/middleware"
)

type outboxEvent struct {
	eventType string
}

func outboxEventsFor(t *testing.T, ctx context.Context, tenantID, aggregateID string) []outboxEvent {
	t.Helper()
	rows, err := testPool.Query(ctx,
		`SELECT event_type FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 ORDER BY created_at`,
		tenantID, aggregateID)
	if err != nil {
		t.Fatalf("query eventing_outbox: %v", err)
	}
	defer rows.Close()
	var out []outboxEvent
	for rows.Next() {
		var e outboxEvent
		if err := rows.Scan(&e.eventType); err != nil {
			t.Fatalf("scan outbox row: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox rows: %v", err)
	}
	return out
}

func legacyEventTypeOf(t *testing.T, ctx context.Context, tenantID, aggregateID, canonicalType string) string {
	t.Helper()
	var payload string
	err := testPool.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox
		 WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3
		 ORDER BY created_at DESC LIMIT 1`,
		tenantID, aggregateID, canonicalType).Scan(&payload)
	if err != nil {
		t.Fatalf("read legacy payload: %v", err)
	}
	return payload
}

func TestPgStore_Outbox_RecordPaymentStatus_EnqueuesRecordedEvent(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.New().String(), ProviderRequestID: "pr-1", SourceReference: "src-1",
	}, "actor-1", "corr-record-1")
	if err != nil {
		t.Fatalf("record payment status: %v", err)
	}

	events := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 outbox row, got %d", len(events))
	}
	if got := events[0].eventType; got != "com.zoikosuite.banking.payment-execution-state.recorded" {
		t.Fatalf("unexpected canonical type: %s", got)
	}
	payload := legacyEventTypeOf(t, ctx, tenantID, p.PaymentID, events[0].eventType)
	if !strings.Contains(payload, `"event_type":"PAYMENT_EXECUTION_STATE_RECORDED"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

func TestPgStore_Outbox_ApplyCallbackStatus_EnqueuesOnlyOnFirstApply(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.New().String(), ProviderRequestID: "pr-2", SourceReference: "src-2",
	}, "actor-1", "corr-record-2")
	if err != nil {
		t.Fatalf("record payment status: %v", err)
	}

	payload := domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-accepted-1", ReportedStatus: domain.StatusAccepted}
	updated, applied, err := testStore.ApplyCallbackStatus(ctx, p.PaymentID, payload, domain.EventPaymentAccepted, "PROVIDER_CALLBACK", "provider-callback", payload.ProviderEventRef, true)
	if err != nil || !applied {
		t.Fatalf("apply callback: applied=%v err=%v", applied, err)
	}

	events := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	// RecordPaymentStatus already enqueued 1; this adds a 2nd.
	if len(events) != 2 {
		t.Fatalf("expected exactly 2 outbox rows total, got %d: %+v", len(events), events)
	}
	var sawAccepted bool
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.accepted" {
			sawAccepted = true
		}
	}
	if !sawAccepted {
		t.Fatalf("expected a payment-execution-state.accepted row, got %+v", events)
	}
	legacyPayload := legacyEventTypeOf(t, ctx, tenantID, p.PaymentID, "com.zoikosuite.banking.payment-execution-state.accepted")
	if !strings.Contains(legacyPayload, `"event_type":"PAYMENT_ACCEPTED"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", legacyPayload)
	}

	// Replaying the exact same provider_event_ref must not enqueue a
	// second event — the unique constraint on (payment_id,
	// provider_event_ref) makes this a genuine no-op, applied=false.
	_, applied2, err := testStore.ApplyCallbackStatus(ctx, p.PaymentID, payload, domain.EventPaymentAccepted, "PROVIDER_CALLBACK", "provider-callback", payload.ProviderEventRef, true)
	if err != nil || applied2 {
		t.Fatalf("replay: expected applied=false, got applied=%v err=%v", applied2, err)
	}
	eventsAfterReplay := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	if len(eventsAfterReplay) != 2 {
		t.Fatalf("replay must not enqueue a new event, got %d rows: %+v", len(eventsAfterReplay), eventsAfterReplay)
	}
	_ = updated
}

func TestPgStore_Outbox_ApplyCallbackStatus_PollPathDoesNotEmit(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.New().String(), ProviderRequestID: "pr-3", SourceReference: "src-3",
	}, "actor-1", "corr-record-3")
	if err != nil {
		t.Fatalf("record payment status: %v", err)
	}

	payload := domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "", ReportedStatus: domain.StatusAccepted}
	_, applied, err := testStore.ApplyCallbackStatus(ctx, p.PaymentID, payload, domain.EventPaymentAccepted, "MANUAL_POLL", "actor-1", "", false)
	if err != nil || !applied {
		t.Fatalf("apply (poll path): applied=%v err=%v", applied, err)
	}

	events := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	// Only RecordPaymentStatus's own event — the poll-path apply must not
	// add a second one (emitEvent=false preserves PollPaymentStatus's
	// pre-existing behavior of never publishing).
	if len(events) != 1 {
		t.Fatalf("expected only the RecordPaymentStatus event (poll path must not enqueue), got %d: %+v", len(events), events)
	}
}

// TestPgStore_Outbox_ApplyCallbackStatus_RegressionBlockedDoesNotEmit proves
// two things at once: (1) a regressed callback against an already-final
// payment is correctly blocked and does not enqueue an outbox event (same
// as the old dual-write's posture — it only ever published when applied was
// true), and (2) the SAVEPOINT fix in ApplyCallbackStatus actually works —
// before that fix, the recovery SELECT run after the trigger's error would
// itself fail with "current transaction is aborted" (25P02), turning this
// expected no-op into a hard error instead of a graceful applied=false.
func TestPgStore_Outbox_ApplyCallbackStatus_RegressionBlockedDoesNotEmit(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.New().String(), ProviderRequestID: "pr-regress", SourceReference: "src-regress",
	}, "actor-1", "corr-record-regress")
	if err != nil {
		t.Fatalf("record payment status: %v", err)
	}
	settlePayload := domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settle-regress", ReportedStatus: domain.StatusSettled}
	if _, applied, err := testStore.ApplyCallbackStatus(ctx, p.PaymentID, settlePayload, domain.EventPaymentSettled, "PROVIDER_CALLBACK", "provider-callback", settlePayload.ProviderEventRef, true); err != nil || !applied {
		t.Fatalf("settle setup: applied=%v err=%v", applied, err)
	}

	// Now try to regress the already-SETTLED (final) payment back to ACCEPTED.
	regressPayload := domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-regress-1", ReportedStatus: domain.StatusAccepted}
	updated, applied, err := testStore.ApplyCallbackStatus(ctx, p.PaymentID, regressPayload, domain.EventPaymentAccepted, "PROVIDER_CALLBACK", "provider-callback", regressPayload.ProviderEventRef, true)
	if err != nil {
		t.Fatalf("expected a graceful applied=false, got a hard error (the pre-fix SAVEPOINT bug): %v", err)
	}
	if applied {
		t.Fatalf("expected the regression to be blocked (applied=false)")
	}
	if updated.Status != domain.StatusSettled {
		t.Fatalf("expected status to remain SETTLED after a blocked regression, got %s", updated.Status)
	}

	events := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.accepted" {
			t.Fatalf("a blocked regression must not enqueue an event for the rejected status, got %+v", events)
		}
	}
}

func TestPgStore_Outbox_LinkStatement_EnqueuesOnlyOnConflict(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.New().String(), ProviderRequestID: "pr-4", SourceReference: "src-4",
	}, "actor-1", "corr-record-4")
	if err != nil {
		t.Fatalf("record payment status: %v", err)
	}

	// No conflict: canonical status is PREPARED, statement confirms PREPARED.
	_, conflict, err := testStore.LinkStatement(ctx, p.PaymentID, domain.LinkStatementRequest{
		StatementReference: "stmt-1", ReportedStatus: domain.StatusPrepared,
	}, "actor-1", "corr-link-1")
	if err != nil || conflict {
		t.Fatalf("expected no conflict, got conflict=%v err=%v", conflict, err)
	}
	eventsNoConflict := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	if len(eventsNoConflict) != 1 {
		t.Fatalf("no-conflict link must not enqueue a new event beyond RecordPaymentStatus's own, got %d: %+v", len(eventsNoConflict), eventsNoConflict)
	}

	// Conflict: statement reports SETTLED but canonical status is PREPARED.
	_, conflict2, err := testStore.LinkStatement(ctx, p.PaymentID, domain.LinkStatementRequest{
		StatementReference: "stmt-2", ReportedStatus: domain.StatusSettled, StatementLineID: "line-1", BankRecStatus: "MATCHED",
	}, "actor-1", "corr-link-2")
	if err != nil || !conflict2 {
		t.Fatalf("expected a conflict, got conflict=%v err=%v", conflict2, err)
	}
	eventsAfterConflict := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	var sawConflictRaised bool
	for _, e := range eventsAfterConflict {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.conflict-raised" {
			sawConflictRaised = true
		}
	}
	if !sawConflictRaised {
		t.Fatalf("expected a conflict-raised row, got %+v", eventsAfterConflict)
	}
	legacyPayload := legacyEventTypeOf(t, ctx, tenantID, p.PaymentID, "com.zoikosuite.banking.payment-execution-state.conflict-raised")
	if !strings.Contains(legacyPayload, `"statement_line_id":"line-1"`) {
		t.Fatalf("expected BNK-05 correlation fields preserved in payload: %s", legacyPayload)
	}
}

func TestPgStore_Outbox_ResolveConflict_EnqueuesResolvedEvent(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.New().String(), ProviderRequestID: "pr-5", SourceReference: "src-5",
	}, "actor-1", "corr-record-5")
	if err != nil {
		t.Fatalf("record payment status: %v", err)
	}
	if _, conflict, err := testStore.LinkStatement(ctx, p.PaymentID, domain.LinkStatementRequest{
		StatementReference: "stmt-3", ReportedStatus: domain.StatusSettled,
	}, "actor-1", "corr-link-3"); err != nil || !conflict {
		t.Fatalf("expected a conflict to set up this test, got conflict=%v err=%v", conflict, err)
	}

	resolved, err := testStore.ResolveConflict(ctx, p.PaymentID, domain.ResolveConflictRequest{
		FinalStatus: domain.StatusSettled, Reason: "confirmed with provider directly",
	}, "actor-2", "corr-resolve-1")
	if err != nil {
		t.Fatalf("resolve conflict: %v", err)
	}
	if resolved.HasOpenConflict {
		t.Fatalf("expected conflict to be cleared")
	}

	events := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	var sawResolved bool
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.conflict-resolved" {
			sawResolved = true
		}
	}
	if !sawResolved {
		t.Fatalf("expected a conflict-resolved row, got %+v", events)
	}
}

func TestPgStore_Outbox_RecordReturn_EnqueuesReturnedEvent(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.New().String(), ProviderRequestID: "pr-6", SourceReference: "src-6",
	}, "actor-1", "corr-record-6")
	if err != nil {
		t.Fatalf("record payment status: %v", err)
	}
	settlePayload := domain.ProviderCallbackPayload{PaymentID: p.PaymentID, ProviderEventRef: "evt-settled-1", ReportedStatus: domain.StatusSettled}
	if _, applied, err := testStore.ApplyCallbackStatus(ctx, p.PaymentID, settlePayload, domain.EventPaymentSettled, "PROVIDER_CALLBACK", "provider-callback", settlePayload.ProviderEventRef, true); err != nil || !applied {
		t.Fatalf("settle setup: applied=%v err=%v", applied, err)
	}

	returned, err := testStore.RecordReturn(ctx, p.PaymentID, domain.RecordReturnRequest{
		ProviderEventRef: "evt-return-1", Reason: "customer disputed",
	}, "actor-2")
	if err != nil {
		t.Fatalf("record return: %v", err)
	}
	if returned.Status != domain.StatusReturned {
		t.Fatalf("expected RETURNED status, got %s", returned.Status)
	}

	events := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	var sawReturned bool
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.returned" {
			sawReturned = true
		}
	}
	if !sawReturned {
		t.Fatalf("expected a returned row, got %+v", events)
	}

	// Re-applying the exact same provider_event_ref must not enqueue a
	// second return event.
	if _, err := testStore.RecordReturn(ctx, p.PaymentID, domain.RecordReturnRequest{
		ProviderEventRef: "evt-return-1", Reason: "replay",
	}, "actor-2"); err == nil {
		t.Fatalf("expected replay of the same provider_event_ref to be rejected")
	}
	eventsAfterReplay := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	returnedCount := 0
	for _, e := range eventsAfterReplay {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.returned" {
			returnedCount++
		}
	}
	if returnedCount != 1 {
		t.Fatalf("expected exactly 1 returned event after replay attempt, got %d", returnedCount)
	}
}

func TestPgStore_Outbox_CancelPayment_EnqueuesCancelledEvent(t *testing.T) {
	tenantID := uuid.New().String()
	ctx := middleware.WithTenant(context.Background(), tenantID)

	p, err := testStore.RecordPaymentStatus(ctx, tenantID, domain.RecordPaymentStatusRequest{
		LegalEntityID: uuid.New().String(), ProviderRequestID: "pr-7", SourceReference: "src-7",
	}, "actor-1", "corr-record-7")
	if err != nil {
		t.Fatalf("record payment status: %v", err)
	}

	cancelled, err := testStore.CancelPayment(ctx, p.PaymentID, domain.CancelRequest{Reason: "duplicate"}, "actor-1", "corr-cancel-1")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.Status != domain.StatusCancelled {
		t.Fatalf("expected CANCELLED status, got %s", cancelled.Status)
	}

	events := outboxEventsFor(t, ctx, tenantID, p.PaymentID)
	var sawCancelled bool
	for _, e := range events {
		if e.eventType == "com.zoikosuite.banking.payment-execution-state.cancelled" {
			sawCancelled = true
		}
	}
	if !sawCancelled {
		t.Fatalf("expected a cancelled row, got %+v", events)
	}
	legacyPayload := legacyEventTypeOf(t, ctx, tenantID, p.PaymentID, "com.zoikosuite.banking.payment-execution-state.cancelled")
	if !strings.Contains(legacyPayload, `"event_type":"PAYMENT_CANCELLED"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", legacyPayload)
	}
}
