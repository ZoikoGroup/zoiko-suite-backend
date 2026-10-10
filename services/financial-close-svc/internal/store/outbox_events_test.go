//go:build integration

// Package store_test — proof that financial-close-svc's period lifecycle
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
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-close-svc/internal/domain"
	svcmiddleware "zoiko.io/financial-close-svc/internal/middleware"
)

// outboxEvent is the subset of one eventing_outbox row this file asserts on.
type outboxEvent struct {
	eventType string
}

// outboxEventsFor returns every outbox row for one aggregate, scoped to the
// given tenant, oldest first.
func outboxEventsFor(t *testing.T, ctx context.Context, tenantID, aggregateID string) []outboxEvent {
	t.Helper()
	rows, err := testPool.Query(ctx,
		`SELECT event_type FROM eventing_outbox WHERE tenant_id = $1 AND aggregate_id = $2 ORDER BY created_at`,
		tenantID, aggregateID)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxEvent
	for rows.Next() {
		var e outboxEvent
		require.NoError(t, rows.Scan(&e.eventType))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

// legacyEventTypeOf reads the legacy event_type field out of one outbox
// row's JSON payload (BYTEA, so convert_from reads it as text) — proof the
// backward-compatible field actually carries the pre-standard name existing
// consumers filter on, not a blank or the canonical string.
func legacyEventTypeOf(t *testing.T, ctx context.Context, tenantID, aggregateID, canonicalType string) string {
	t.Helper()
	var payload string
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox
		 WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3
		 ORDER BY created_at DESC LIMIT 1`,
		tenantID, aggregateID, canonicalType).Scan(&payload))
	return payload
}

// TestPgStore_ApplyPeriodTransition_EnqueuesSoftClosedWithLegacyType proves
// the generic fact-driven enqueue path produces a valid canonical type
// (kebab-case — envelope.New rejects the underscore the original fact
// string used) AND still carries the pre-standard "period.soft_closed"
// legacy name, so an existing consumer filtering on the old event_type
// field keeps working across the outbox migration.
func TestPgStore_ApplyPeriodTransition_EnqueuesSoftClosedWithLegacyType(t *testing.T) {
	f := setupIsolationFixture(t, "soft-closed-enqueue")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)

	_, err := testStore.ApplyPeriodTransition(ctx, f.fiscalPeriodID, []string{"OPEN"},
		domain.PeriodUpdate{To: "SOFT_CLOSE", PrincipalID: "closer-1", At: time.Now().UTC()},
		"soft-closed", "period.soft_closed", "corr-soft-close", "closer-1")
	require.NoError(t, err)

	events := outboxEventsFor(t, ctx, f.tenantID, f.fiscalPeriodID)
	require.Len(t, events, 1, "expected exactly 1 outbox row for the soft-close transition")
	assert.Contains(t, events[0].eventType, "period.soft-closed",
		"canonical type must be kebab-case, not the old snake_case fact string")

	payload := legacyEventTypeOf(t, ctx, f.tenantID, f.fiscalPeriodID, events[0].eventType)
	assert.Contains(t, payload, `"event_type":"period.soft_closed"`,
		"legacy event_type must still carry the pre-standard name for existing consumers")
}

// TestPgStore_CreateCloseEvidence_EnqueuesClosedEvent proves a hard close
// enqueues period.closed atomically with the evidence row.
func TestPgStore_CreateCloseEvidence_EnqueuesClosedEvent(t *testing.T) {
	f := setupIsolationFixture(t, "close-evidence-enqueue")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)

	evidence := &domain.CloseEvidence{
		EvidenceID:       uuid.New().String(),
		TenantID:         f.tenantID,
		FiscalPeriodID:   f.fiscalPeriodID,
		TrialBalanceHash: "sha256:deadbeef",
		Signature:        "sig-1",
		GeneratedAt:      time.Now().UTC(),
	}
	require.NoError(t, testStore.CreateCloseEvidence(ctx, evidence, "corr-close-evidence", "closer-1", "2026-Q4", f.legalEntityID))

	events := outboxEventsFor(t, ctx, f.tenantID, f.fiscalPeriodID)
	require.Len(t, events, 1, "expected exactly 1 outbox row for the close evidence")
	assert.Contains(t, events[0].eventType, "period.closed")

	payload := legacyEventTypeOf(t, ctx, f.tenantID, f.fiscalPeriodID, events[0].eventType)
	assert.Contains(t, payload, `"event_type":"period.closed"`)
}

// TestPgStore_CreateControlRun_EnqueuesExceptionOnlyWhenExceptional proves
// CreateControlRun enqueues subledger-control-exception for an EXCEPTION
// run, with the legacy "subledger.control.exception" name (NOT
// "period.subledger_control_exception" — this fact never carried the
// "period." prefix under the old Publisher), and enqueues nothing at all
// for a MATCHED run.
func TestPgStore_CreateControlRun_EnqueuesExceptionOnlyWhenExceptional(t *testing.T) {
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	matched := &domain.SubledgerControlRun{
		ControlRunID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-09", Subledger: "AP", ControlAccountCode: "2000-AP",
		SubledgerTotalAmount: 500, GLControlBalanceAmount: 500, DifferenceAmount: 0,
		Status: "MATCHED", RunAt: time.Now().UTC(), RunByPrincipalID: "runner-1",
	}
	require.NoError(t, testStore.CreateControlRun(ctx, matched, "corr-matched", "runner-1"))
	assert.Empty(t, outboxEventsFor(t, ctx, tenantID, matched.ControlRunID),
		"a MATCHED run must not enqueue any outbox event")

	exception := &domain.SubledgerControlRun{
		ControlRunID: uuid.New().String(), TenantID: tenantID, LegalEntityID: legalEntityID,
		FiscalPeriod: "2026-09", Subledger: "AR", ControlAccountCode: "1200-AR",
		SubledgerTotalAmount: 700, GLControlBalanceAmount: 650, DifferenceAmount: 50,
		Status: "EXCEPTION", RunAt: time.Now().UTC(), RunByPrincipalID: "runner-1",
	}
	require.NoError(t, testStore.CreateControlRun(ctx, exception, "corr-exception", "runner-1"))
	events := outboxEventsFor(t, ctx, tenantID, exception.ControlRunID)
	require.Len(t, events, 1, "expected exactly 1 outbox row for the EXCEPTION run")
	assert.Contains(t, events[0].eventType, "subledger-control-exception")

	payload := legacyEventTypeOf(t, ctx, tenantID, exception.ControlRunID, events[0].eventType)
	assert.Contains(t, payload, `"event_type":"subledger.control.exception"`,
		"legacy name has no period. prefix — it never did, even before the outbox migration")
}

// TestPgStore_CreateReopenRequest_EnqueuesReopenRequestedEvent proves
// requesting a reopen enqueues period.reopen-requested (canonical,
// kebab-case) with the legacy "period.reopen_requested" name intact.
func TestPgStore_CreateReopenRequest_EnqueuesReopenRequestedEvent(t *testing.T) {
	f := setupIsolationFixture(t, "reopen-request-enqueue")
	ctx := svcmiddleware.WithTenant(context.Background(), f.tenantID)

	// CreateReopenRequest only accepts HARD_CLOSED/RECLOSED periods — get
	// the fixture there first. No fact: this test is about the request
	// event, not the close event (covered separately above).
	_, err := testStore.ApplyPeriodTransition(ctx, f.fiscalPeriodID, []string{"OPEN"},
		domain.PeriodUpdate{To: domain.PeriodHardClosed, PrincipalID: "closer-1", At: time.Now().UTC()},
		"", "", "corr-setup-close", "closer-1")
	require.NoError(t, err)

	req := &domain.ReopenRequest{
		RequestID: uuid.New().String(), TenantID: f.tenantID, FiscalPeriodID: f.fiscalPeriodID,
		RequestedByPrincipalID: "requester-1", Reason: "late invoice discovered",
		ReopenUntil: time.Now().UTC().AddDate(0, 0, 7), Status: "PENDING", CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, testStore.CreateReopenRequest(ctx, req, "corr-reopen-request", "requester-1"))

	events := outboxEventsFor(t, ctx, f.tenantID, f.fiscalPeriodID)
	require.Len(t, events, 1)
	assert.Contains(t, events[0].eventType, "reopen-requested")

	payload := legacyEventTypeOf(t, ctx, f.tenantID, f.fiscalPeriodID, events[0].eventType)
	assert.Contains(t, payload, `"event_type":"period.reopen_requested"`)
}
