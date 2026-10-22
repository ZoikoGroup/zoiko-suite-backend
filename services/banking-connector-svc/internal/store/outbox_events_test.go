//go:build integration

// Package store_test — proof that banking-connector-svc's BNK-02/03/04
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
// Writes happen over appRolePool (a real NOSUPERUSER NOBYPASSRLS role, see
// rls_test.go), so this also proves the eventing_outbox RLS policy
// (migration 007) does not block ordinary application writes — only the
// relay's own app.outbox_relay marker and the owning tenant may see a row.
// Assertions read back over the admin (superuser) pool: these tests are
// about whether the write happened at all, not about tenant-scoped
// readback, which rls_test.go already covers separately.
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

	"github.com/jackc/pgx/v5/pgxpool"

	"zoiko.io/banking-connector-svc/internal/domain"
	"zoiko.io/banking-connector-svc/internal/middleware"
	"zoiko.io/banking-connector-svc/internal/store"
)

// outboxEvent is the subset of one eventing_outbox row this file asserts on.
type outboxEvent struct {
	eventType string
}

// outboxEventsFor returns every outbox row for one aggregate, scoped to the
// given tenant, oldest first.
func outboxEventsFor(t *testing.T, ctx context.Context, admin *pgxpool.Pool, tenantID, aggregateID string) []outboxEvent {
	t.Helper()
	rows, err := admin.Query(ctx,
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

// legacyEventTypeOf reads the legacy event_type field out of one outbox
// row's JSON payload (BYTEA, so convert_from reads it as text) — proof the
// backward-compatible field actually carries the pre-standard name existing
// consumers filter on, not a blank or the canonical string.
func legacyEventTypeOf(t *testing.T, ctx context.Context, admin *pgxpool.Pool, tenantID, aggregateID, canonicalType string) string {
	t.Helper()
	var payload string
	err := admin.QueryRow(ctx,
		`SELECT convert_from(payload, 'UTF8') FROM eventing_outbox
		 WHERE tenant_id = $1 AND aggregate_id = $2 AND event_type = $3
		 ORDER BY created_at DESC LIMIT 1`,
		tenantID, aggregateID, canonicalType).Scan(&payload)
	if err != nil {
		t.Fatalf("read legacy payload: %v", err)
	}
	return payload
}

func TestPgStore_Outbox_CreateConnection_EnqueuesCreatedEvent(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	ctx := middleware.WithTenant(context.Background(), "tenant-outbox-conn")
	conn := &domain.BankConnection{
		LegalEntityID: "le-outbox", BankName: "Outbox Test Bank", BIC: "OTBKUS33XXX",
		AccountNumber: "GB00OTBK1234567890", Currency: "USD", Status: "CONNECTED",
	}
	if err := s.CreateConnection(ctx, conn, "actor-1", "corr-create-conn"); err != nil {
		t.Fatalf("create connection: %v", err)
	}

	events := outboxEventsFor(t, ctx, admin, "tenant-outbox-conn", conn.ConnectionID)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 outbox row for the created connection, got %d", len(events))
	}
	if got := events[0].eventType; got != "com.zoikosuite.banking.bank-connection.created" {
		t.Fatalf("unexpected canonical type: %s", got)
	}
	payload := legacyEventTypeOf(t, ctx, admin, "tenant-outbox-conn", conn.ConnectionID, events[0].eventType)
	if !strings.Contains(payload, `"event_type":"banking.connection.created"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

func TestPgStore_Outbox_RecordStatement_EnqueuesIngestedEvent(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	ctx := middleware.WithTenant(context.Background(), "tenant-outbox-recstmt")
	connID := seedConnection(t, ctx, s, "tenant-outbox-recstmt")

	stmt := &domain.BankStatement{
		ConnectionID:    connID,
		StatementFormat: "MT940",
		StatementDate:   time.Now(),
		OpeningBalance:  1000,
		ClosingBalance:  1500,
	}
	if err := s.RecordStatement(ctx, stmt, "le-outbox", "actor-1", "corr-record-stmt-1"); err != nil {
		t.Fatalf("record statement: %v", err)
	}

	events := outboxEventsFor(t, ctx, admin, "tenant-outbox-recstmt", stmt.StatementID)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 outbox row, got %d", len(events))
	}
	if got := events[0].eventType; got != "com.zoikosuite.banking.bank-statement.ingested" {
		t.Fatalf("unexpected canonical type: %s", got)
	}
	payload := legacyEventTypeOf(t, ctx, admin, "tenant-outbox-recstmt", stmt.StatementID, events[0].eventType)
	if !strings.Contains(payload, `"event_type":"banking.statement.ingested"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}
}

func TestPgStore_Outbox_InitiateConnection_EnqueuesOnlyOnFirstRequest(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	ctx := middleware.WithTenant(context.Background(), "tenant-outbox-bnk02")
	params := domain.InitiateConnectionParams{
		TenantID: "tenant-outbox-bnk02", LegalEntityID: "le-outbox", BankAccountID: "acct-outbox",
		BankName: "Outbox Bank", CreatedByPrincipalID: "actor-1", CorrelationID: "dedup-key-1",
		RequestCorrelationID: "corr-initiate-1",
	}
	conn, created, err := s.InitiateConnection(ctx, params)
	if err != nil || !created {
		t.Fatalf("initiate connection: created=%v err=%v", created, err)
	}

	events := outboxEventsFor(t, ctx, admin, "tenant-outbox-bnk02", conn.ConnectionID)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 outbox row, got %d", len(events))
	}
	if got := events[0].eventType; got != "com.zoikosuite.banking.bank-connection.requested" {
		t.Fatalf("unexpected canonical type: %s", got)
	}
	payload := legacyEventTypeOf(t, ctx, admin, "tenant-outbox-bnk02", conn.ConnectionID, events[0].eventType)
	if !strings.Contains(payload, `"event_type":"banking.connection.requested"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}

	again, created2, err := s.InitiateConnection(ctx, params)
	if err != nil || created2 {
		t.Fatalf("replay: expected created=false, got created=%v err=%v", created2, err)
	}
	if again.ConnectionID != conn.ConnectionID {
		t.Fatalf("replay returned a different connection: %s vs %s", again.ConnectionID, conn.ConnectionID)
	}
	eventsAfterReplay := outboxEventsFor(t, ctx, admin, "tenant-outbox-bnk02", conn.ConnectionID)
	if len(eventsAfterReplay) != 1 {
		t.Fatalf("replay must not enqueue a second event, got %d rows", len(eventsAfterReplay))
	}
}

func TestPgStore_Outbox_IngestStatement_EnqueuesOnlyOnFirstIngest(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	ctx := middleware.WithTenant(context.Background(), "tenant-outbox-bnk03")
	connID := seedConnection(t, ctx, s, "tenant-outbox-bnk03")

	req := domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(),
		ContentHash: "hash-outbox-1",
		Lines: []domain.StatementLineIn{
			{PostedDate: time.Now(), Amount: 100, Currency: "USD", Description: "deposit"},
		},
	}
	result, err := s.IngestStatement(ctx, "tenant-outbox-bnk03", req, "auditor-1", "corr-ingest-1")
	if err != nil || !result.Created {
		t.Fatalf("ingest: created=%v err=%v", result != nil && result.Created, err)
	}

	events := outboxEventsFor(t, ctx, admin, "tenant-outbox-bnk03", result.Statement.StatementID)
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 outbox row, got %d", len(events))
	}
	if got := events[0].eventType; got != "com.zoikosuite.banking.bank-statement.ingested" {
		t.Fatalf("unexpected canonical type: %s", got)
	}
	payload := legacyEventTypeOf(t, ctx, admin, "tenant-outbox-bnk03", result.Statement.StatementID, events[0].eventType)
	if !strings.Contains(payload, `"event_type":"banking.statement.ingested"`) {
		t.Fatalf("legacy event_type missing or wrong in payload: %s", payload)
	}

	second, err := s.IngestStatement(ctx, "tenant-outbox-bnk03", req, "auditor-1", "corr-ingest-2")
	if err != nil || second.Created {
		t.Fatalf("replay: expected created=false, got created=%v err=%v", second != nil && second.Created, err)
	}
	eventsAfterReplay := outboxEventsFor(t, ctx, admin, "tenant-outbox-bnk03", result.Statement.StatementID)
	if len(eventsAfterReplay) != 1 {
		t.Fatalf("replay must not enqueue a second event, got %d rows", len(eventsAfterReplay))
	}
}

func TestPgStore_Outbox_NormalizeTransaction_EnqueuesNormalizedOrException(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	ctx := middleware.WithTenant(context.Background(), "tenant-outbox-bnk04")
	connID := seedConnection(t, ctx, s, "tenant-outbox-bnk04")
	if _, err := s.CreateTransactionMapping(ctx, domain.CreateTransactionMappingParams{
		TenantID: "tenant-outbox-bnk04", BankCode: "WIRE-OUTBOX", Category: "WIRE_IN", ActorPrincipalID: "ops-admin",
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	mapped, err := s.IngestStatement(ctx, "tenant-outbox-bnk04", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-norm-mapped",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 250, Currency: "USD", BankCode: "WIRE-OUTBOX"}},
	}, "auditor-1", "")
	if err != nil {
		t.Fatalf("ingest mapped: %v", err)
	}
	normResult, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-outbox-bnk04", StatementLineID: mapped.Lines[0].LineID, Counterparty: "Acme", ActorPrincipalID: "ops-1", CorrelationID: "corr-norm-1",
	})
	if err != nil || normResult.Transaction == nil {
		t.Fatalf("normalize mapped line: %+v err=%v", normResult, err)
	}
	events := outboxEventsFor(t, ctx, admin, "tenant-outbox-bnk04", normResult.Transaction.TransactionID)
	if len(events) != 1 || events[0].eventType != "com.zoikosuite.banking.canonical-transaction.normalized" {
		t.Fatalf("expected 1 canonical-transaction.normalized row, got %+v", events)
	}

	unmapped, err := s.IngestStatement(ctx, "tenant-outbox-bnk04", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-norm-unmapped",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 50, Currency: "USD", BankCode: "UNKNOWN-CODE"}},
	}, "auditor-1", "")
	if err != nil {
		t.Fatalf("ingest unmapped: %v", err)
	}
	excResult, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-outbox-bnk04", StatementLineID: unmapped.Lines[0].LineID, ActorPrincipalID: "ops-1", CorrelationID: "corr-norm-2",
	})
	if err != nil || excResult.QuarantinedException == nil {
		t.Fatalf("normalize unmapped line: %+v err=%v", excResult, err)
	}
	excEvents := outboxEventsFor(t, ctx, admin, "tenant-outbox-bnk04", excResult.QuarantinedException.ExceptionID)
	if len(excEvents) != 1 || excEvents[0].eventType != "com.zoikosuite.banking.mapping-exception.raised" {
		t.Fatalf("expected 1 mapping-exception.raised row, got %+v", excEvents)
	}

	if _, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-outbox-bnk04", StatementLineID: unmapped.Lines[0].LineID, ActorPrincipalID: "ops-1", CorrelationID: "corr-norm-3",
	}); err != nil {
		t.Fatalf("second normalize of unmapped line: %v", err)
	}
	excEventsAfterReplay := outboxEventsFor(t, ctx, admin, "tenant-outbox-bnk04", excResult.QuarantinedException.ExceptionID)
	if len(excEventsAfterReplay) != 1 {
		t.Fatalf("replay must not enqueue a second exception event, got %d rows", len(excEventsAfterReplay))
	}
}

func TestPgStore_Outbox_ReNormalizeTransaction_EnqueuesRenormalizedEvent(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	ctx := middleware.WithTenant(context.Background(), "tenant-outbox-renorm")
	connID := seedConnection(t, ctx, s, "tenant-outbox-renorm")
	if _, err := s.CreateTransactionMapping(ctx, domain.CreateTransactionMappingParams{
		TenantID: "tenant-outbox-renorm", BankCode: "MISC-RENORM", Category: "MISC", ActorPrincipalID: "ops-admin",
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	result, err := s.IngestStatement(ctx, "tenant-outbox-renorm", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-renorm",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 75, Currency: "USD", BankCode: "MISC-RENORM"}},
	}, "auditor-1", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	normResult, err := s.NormalizeTransaction(ctx, domain.NormalizeTransactionParams{
		TenantID: "tenant-outbox-renorm", StatementLineID: result.Lines[0].LineID, ActorPrincipalID: "ops-1",
	})
	if err != nil || normResult.Transaction == nil {
		t.Fatalf("normalize: %+v err=%v", normResult, err)
	}

	corrected, err := s.ReNormalizeTransaction(ctx, domain.ReNormalizeTransactionParams{
		TenantID: "tenant-outbox-renorm", PriorTransactionID: normResult.Transaction.TransactionID, Category: "MISC_CORRECTED", ActorPrincipalID: "ops-2", CorrelationID: "corr-renorm-1",
	})
	if err != nil {
		t.Fatalf("re-normalize: %v", err)
	}
	events := outboxEventsFor(t, ctx, admin, "tenant-outbox-renorm", corrected.TransactionID)
	if len(events) != 1 || events[0].eventType != "com.zoikosuite.banking.canonical-transaction.renormalized" {
		t.Fatalf("expected 1 canonical-transaction.renormalized row, got %+v", events)
	}
	priorEvents := outboxEventsFor(t, ctx, admin, "tenant-outbox-renorm", normResult.Transaction.TransactionID)
	for _, e := range priorEvents {
		if e.eventType == "com.zoikosuite.banking.canonical-transaction.renormalized" {
			t.Fatalf("the prior (now-superseded) transaction must not carry its own renormalized event")
		}
	}
}

func TestPgStore_Outbox_QuarantineAndApproveMappingException_EnqueueBothEvents(t *testing.T) {
	admin := openAdminPool(t)
	appPool := appRolePool(t, admin)
	s := store.NewPgStore(appPool, store.WithEventRegion("uk"))

	ctx := middleware.WithTenant(context.Background(), "tenant-outbox-mapex")
	connID := seedConnection(t, ctx, s, "tenant-outbox-mapex")
	result, err := s.IngestStatement(ctx, "tenant-outbox-mapex", domain.IngestStatementLinesRequest{
		ConnectionID: connID, StatementFormat: domain.FormatBAI2, StatementDate: time.Now(), ContentHash: "hash-mapex",
		Lines: []domain.StatementLineIn{{PostedDate: time.Now(), Amount: 42, Currency: "USD", Description: "unknown"}},
	}, "auditor-1", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	exc, err := s.QuarantineTransaction(ctx, domain.QuarantineTransactionParams{
		TenantID: "tenant-outbox-mapex", StatementLineID: result.Lines[0].LineID, Reason: "unrecognized code",
		ActorPrincipalID: "ops-1", CorrelationID: "corr-quarantine-1",
	})
	if err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	events := outboxEventsFor(t, ctx, admin, "tenant-outbox-mapex", exc.ExceptionID)
	if len(events) != 1 || events[0].eventType != "com.zoikosuite.banking.mapping-exception.raised" {
		t.Fatalf("expected 1 mapping-exception.raised row, got %+v", events)
	}

	if _, err := s.ApproveMappingException(ctx, domain.ApproveMappingExceptionParams{
		TenantID: "tenant-outbox-mapex", ExceptionID: exc.ExceptionID, Category: "MISC", ApproverPrincipalID: "ops-2", CorrelationID: "corr-approve-1",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	eventsAfterApprove := outboxEventsFor(t, ctx, admin, "tenant-outbox-mapex", exc.ExceptionID)
	var sawApproved bool
	for _, e := range eventsAfterApprove {
		if e.eventType == "com.zoikosuite.banking.mapping-exception.approved" {
			sawApproved = true
		}
	}
	if !sawApproved {
		t.Fatalf("expected a mapping-exception.approved row after approval, got %+v", eventsAfterApprove)
	}
	if len(eventsAfterApprove) != 2 {
		t.Fatalf("expected exactly 2 rows total (raised + approved), got %d: %+v", len(eventsAfterApprove), eventsAfterApprove)
	}
}
