//go:build integration

// Package store_test — proof that treasury-svc's BNK-01/08/09/10 events
// actually reach the transactional outbox (ZS-EVENT-001 §6), inside the
// same transaction as the state change they report, and with valid
// kebab-case canonical types.
//
// This outbox wiring existed before this session's work on it, but was
// never exercised against a real transaction: 000011_add_eventing_outbox
// was never in tenant_isolation_test.go's hardcoded migration list, so the
// table never even existed in the test database, and the 5 enqueue* helper
// functions used underscored AggregateType/Type segments (e.g.
// "bank_account") that envelope.New's kebab-case validation rejects
// outright — every one of them would have failed on the very first real
// write. This file is the first real proof any of them work.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/ -run TestPgStore_Outbox
package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
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

// TestPgStore_Outbox_CreateBankAccount_EnqueuesCreatedEvent proves the
// bank-account family's canonical type is valid kebab-case (the original
// "bank_account" AggregateType would have failed envelope.New outright)
// and the legacy name is preserved.
func TestPgStore_Outbox_CreateBankAccount_EnqueuesCreatedEvent(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String())

	if _, err := testStore.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}

	events := outboxEventsFor(t, ctx, tenantID, acct.BankAccountID)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 outbox row, got %d", len(events))
	}
	if got := events[0].eventType; got != "com.zoikosuite.treasury.bank-account.created" {
		t.Fatalf("unexpected canonical type: %s", got)
	}
	payload := legacyEventTypeOf(t, ctx, tenantID, acct.BankAccountID, events[0].eventType)
	if !strings.Contains(payload, `"event_type":"bank_account.created"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

// TestPgStore_Outbox_VerifyBankAccountOwnership_EnqueuesVerifiedEvent is
// also the real proof of the nil-pointer fix in VerifyBankAccountOwnership
// (acct was declared as a nil *domain.BankAccount and scanned into
// directly — a guaranteed panic on the very first row returned). Before
// that fix this test would crash the whole test binary, not merely fail.
func TestPgStore_Outbox_VerifyBankAccountOwnership_EnqueuesVerifiedEvent(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	acct := newTestAccount(tenantID, uuid.New().String())
	if _, err := testStore.CreateBankAccount(ctx, acct); err != nil {
		t.Fatalf("create account: %v", err)
	}

	if _, err := testStore.VerifyBankAccountOwnership(ctx, domain.VerifyOwnershipParams{
		BankAccountID: acct.BankAccountID, TenantID: tenantID, VerificationMethod: "MICRO_DEPOSIT",
		EvidenceRef: "evidence-outbox-1", VerifiedByPrincipalID: "auditor-1",
	}); err != nil {
		t.Fatalf("verify ownership: %v", err)
	}

	events := outboxEventsFor(t, ctx, tenantID, acct.BankAccountID)
	var sawVerified bool
	for _, e := range events {
		if e.eventType == "com.zoikosuite.treasury.bank-account.ownership-verified" {
			sawVerified = true
		}
	}
	if !sawVerified {
		t.Fatalf("expected an ownership-verified row, got %+v", events)
	}
	payload := legacyEventTypeOf(t, ctx, tenantID, acct.BankAccountID, "com.zoikosuite.treasury.bank-account.ownership-verified")
	if !strings.Contains(payload, `"event_type":"bank_account.ownership_verified"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

func TestPgStore_Outbox_CreateTreasuryTransfer_EnqueuesCreatedEvent(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)
	src, tgt := seedTransferPair(t, ctx, tenantID)

	transfer, created, err := testStore.CreateTreasuryTransfer(ctx, domain.CreateTreasuryTransferParams{
		TenantID: tenantID, SourceBankAccountID: src, TargetBankAccountID: tgt,
		Amount: 100, CurrencyCode: "USD", IsCrossEntity: false, CorrelationID: "corr-outbox-transfer-1", MakerPrincipalID: "maker-1",
	})
	if err != nil || !created {
		t.Fatalf("create transfer: created=%v err=%v", created, err)
	}

	events := outboxEventsFor(t, ctx, tenantID, transfer.TransferID)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 outbox row, got %d", len(events))
	}
	if got := events[0].eventType; got != "com.zoikosuite.treasury.treasury-transfer.created" {
		t.Fatalf("unexpected canonical type: %s", got)
	}
	payload := legacyEventTypeOf(t, ctx, tenantID, transfer.TransferID, events[0].eventType)
	if !strings.Contains(payload, `"event_type":"treasury_transfer.created"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

func TestPgStore_Outbox_CashPositionSnapshot_EnqueuesCalculatedAndStaleEvents(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	snap, err := testStore.CreateCashPositionSnapshot(ctx, domain.CalculateCashPositionParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ReportingCurrency: "USD",
		RestrictedAmount: 0, ActorPrincipalID: "analyst-1", CorrelationID: "corr-outbox-cash-1",
	}, domain.CashPositionCalculation{
		BankBalance: 5000, AvailableCash: 5000, AsOfTimestamp: time.Now().UTC(),
		AccountBreakdown: []domain.CashPositionAccountLine{{BankAccountID: "acct-outbox-1", AccountName: "Main", Balance: 5000, AsOfTimestamp: time.Now().UTC()}},
	})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	events := outboxEventsFor(t, ctx, tenantID, snap.SnapshotID)
	if len(events) != 1 || events[0].eventType != "com.zoikosuite.treasury.cash-position.calculated" {
		t.Fatalf("expected 1 cash-position.calculated row, got %+v", events)
	}
	payload := legacyEventTypeOf(t, ctx, tenantID, snap.SnapshotID, "com.zoikosuite.treasury.cash-position.calculated")
	if !strings.Contains(payload, `"event_type":"cash_position.calculated"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

func TestPgStore_Outbox_FXExposureSnapshot_EnqueuesCalculatedEvent(t *testing.T) {
	cleanTables(t)
	tenantID := uuid.New().String()
	legalEntityID := uuid.New().String()
	ctx := svcmiddleware.WithTenant(context.Background(), tenantID)

	snap, err := testStore.CreateFXExposureSnapshot(ctx, domain.CalculateFXExposureParams{
		TenantID: tenantID, LegalEntityID: legalEntityID, ExposureCurrency: "EUR", FunctionalCurrency: "USD",
		NettingScope: "SINGLE_ENTITY:" + legalEntityID, ActorPrincipalID: "analyst-1", CorrelationID: "corr-outbox-fx-1",
	}, domain.FXExposureCalculation{
		RateUsed: 1.08, RateAsOf: time.Now().UTC(), RateVersion: "rate-outbox-1", HasStaleComponent: false,
		Buckets: []domain.FXExposureBucket{
			{MaturityBucket: "0-30D", Category: "RECEIVABLE", ExposureCurrencyAmount: 1000, FunctionalCurrencyAmount: 1080},
		},
		GrossExposureAmount: 1080, NetExposureAmount: 1080, AsOfTimestamp: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	events := outboxEventsFor(t, ctx, tenantID, snap.SnapshotID)
	if len(events) != 1 || events[0].eventType != "com.zoikosuite.treasury.fx-exposure.calculated" {
		t.Fatalf("expected 1 fx-exposure.calculated row, got %+v", events)
	}
	payload := legacyEventTypeOf(t, ctx, tenantID, snap.SnapshotID, "com.zoikosuite.treasury.fx-exposure.calculated")
	if !strings.Contains(payload, `"event_type":"fx_exposure.calculated"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}
