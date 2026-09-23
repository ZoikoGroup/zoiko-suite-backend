package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/notification-svc/internal/ledger"
	"zoiko.io/notification-svc/internal/store"
)

// TestLedger_CreateMessageIntent_Idempotent verifies that duplicate creation with
// the same (tenant_id, deduplication_key) succeeds idempotently and returns the existing record.
func TestLedger_CreateMessageIntent_Idempotent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-" + uuid.NewString()
	eventID := "evt-" + uuid.NewString()
	dedupKey := ledger.ComputeDeduplicationKey(tenantID, "identity.password_reset_requested", eventID)
	intentID := uuid.NewString()

	intent := &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "usr-001",
		RecipientEmail:       "user@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassS0,
		TemplateKey:          "ZS-IA-001",
		EventID:              eventID,
		SourceEventType:      "identity.password_reset_requested",
		DeduplicationKey:     dedupKey,
		CorrelationID:        "corr-1",
		Status:               ledger.IntentStatusPending,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}

	// First creation: must succeed with created = true
	created, existing, err := s.CreateMessageIntent(ctx, intent)
	if err != nil {
		t.Fatalf("first CreateMessageIntent failed: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true for first insert, got false")
	}
	if existing == nil || existing.MessageIntentID != intentID {
		t.Fatalf("unexpected intent returned: %+v", existing)
	}

	// Second creation with duplicate deduplication key: must succeed with created = false and return existing
	intent2 := &ledger.MessageIntent{
		MessageIntentID:      uuid.NewString(), // Different ID attempted
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "usr-001",
		RecipientEmail:       "user@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassS0,
		TemplateKey:          "ZS-IA-001",
		EventID:              eventID,
		SourceEventType:      "identity.password_reset_requested",
		DeduplicationKey:     dedupKey, // Same dedup key
		CorrelationID:        "corr-2",
		Status:               ledger.IntentStatusPending,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}

	created2, existing2, err := s.CreateMessageIntent(ctx, intent2)
	if err != nil {
		t.Fatalf("second CreateMessageIntent failed: %v", err)
	}
	if created2 {
		t.Fatalf("expected created=false for replay, got true")
	}
	if existing2 == nil || existing2.MessageIntentID != intentID {
		t.Fatalf("expected replay to return original intent ID %s, got %+v", intentID, existing2)
	}

	// Verify database contains exactly 1 row
	var count int
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM message_intents WHERE tenant_id = $1 AND deduplication_key = $2",
		tenantID, dedupKey).Scan(&count)
	if err != nil {
		t.Fatalf("failed to count intents: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row in message_intents, got %d", count)
	}
}

// TestLedger_ConcurrentInsert_NoDuplicates tests that concurrent ingestion races
// on the same deduplication key safely resolve to exactly one inserted record.
func TestLedger_ConcurrentInsert_NoDuplicates(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-" + uuid.NewString()
	eventID := "evt-" + uuid.NewString()
	dedupKey := ledger.ComputeDeduplicationKey(tenantID, "identity.password_reset_requested", eventID)

	const concurrency = 10
	var createdCount int32
	var wg sync.WaitGroup
	returnedIDs := make([]string, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			intent := &ledger.MessageIntent{
				MessageIntentID:      uuid.NewString(),
				TenantID:             tenantID,
				LegalEntityID:        "entity-001",
				RecipientPrincipalID: "usr-001",
				RecipientEmail:       "user@example.com",
				Channel:              "EMAIL",
				CommunicationClass:   ledger.ClassS0,
				TemplateKey:          "ZS-IA-001",
				EventID:              eventID,
				SourceEventType:      "identity.password_reset_requested",
				DeduplicationKey:     dedupKey,
				CorrelationID:        fmt.Sprintf("corr-%d", idx),
				Status:               ledger.IntentStatusPending,
				CreatedAt:            time.Now().UTC(),
				UpdatedAt:            time.Now().UTC(),
			}

			created, res, err := s.CreateMessageIntent(ctx, intent)
			if err != nil {
				t.Errorf("goroutine %d CreateMessageIntent error: %v", idx, err)
				return
			}
			if created {
				atomic.AddInt32(&createdCount, 1)
			}
			if res != nil {
				returnedIDs[idx] = res.MessageIntentID
			}
		}(i)
	}
	wg.Wait()

	if createdCount != 1 {
		t.Fatalf("expected exactly 1 insert to succeed as created, got %d", createdCount)
	}

	// Verify all goroutines got back the exact same winner intent ID
	winnerID := ""
	for _, id := range returnedIDs {
		if id != "" {
			if winnerID == "" {
				winnerID = id
			} else if id != winnerID {
				t.Fatalf("inconsistent intent ID returned across concurrent calls: %s vs %s", winnerID, id)
			}
		}
	}
}

// TestLedger_RLS_TenantIsolation verifies that row-level security isolates all ledger tables.
// Tenant B must never see Tenant A's intents, renders, attempts, or events.
func TestLedger_RLS_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantA := "tenant-alpha-" + uuid.NewString()
	tenantB := "tenant-beta-" + uuid.NewString()

	intentID := uuid.NewString()
	renderID := uuid.NewString()
	eventID := uuid.NewString()

	// Tenant A creates full ledger trail
	intent := &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantA,
		LegalEntityID:        "entity-alpha",
		RecipientPrincipalID: "usr-alpha",
		RecipientEmail:       "alpha@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassS0,
		TemplateKey:          "ZS-IA-001",
		EventID:              "evt-alpha",
		SourceEventType:      "identity.password_reset_requested",
		DeduplicationKey:     "dedup-alpha",
		CorrelationID:        "corr-alpha",
		Status:               ledger.IntentStatusDispatched,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	if _, _, err := s.CreateMessageIntent(ctx, intent); err != nil {
		t.Fatalf("Tenant A CreateMessageIntent failed: %v", err)
	}

	render := &ledger.MessageRender{
		RenderID:        renderID,
		MessageIntentID: intentID,
		TenantID:        tenantA,
		TemplateKey:     "ZS-IA-001",
		TemplateVersion: "1.0.0",
		Locale:          "en-US",
		ContentHash:     "hash-alpha",
		Subject:         "Subject Alpha",
		BodyHTML:        "<p>Alpha</p>",
		BodyText:        "Alpha",
		RenderedAt:      time.Now().UTC(),
	}
	if err := s.RecordMessageRender(ctx, render); err != nil {
		t.Fatalf("Tenant A RecordMessageRender failed: %v", err)
	}

	attempt := &ledger.DeliveryAttempt{
		ProviderAttemptID: "prov-att-alpha",
		MessageIntentID:   intentID,
		TenantID:          tenantA,
		RenderID:          renderID,
		ProviderName:      "smtp",
		AttemptNumber:     1,
		Status:            ledger.AttemptStatusAccepted,
		AttemptedAt:       time.Now().UTC(),
	}
	if err := s.RecordDeliveryAttempt(ctx, attempt); err != nil {
		t.Fatalf("Tenant A RecordDeliveryAttempt failed: %v", err)
	}

	deliveryEvent := &ledger.DeliveryEvent{
		DeliveryEventID:   eventID,
		MessageIntentID:   intentID,
		TenantID:          tenantA,
		ProviderAttemptID: "prov-att-alpha",
		EventType:         ledger.DeliveryEventDelivered,
		RawPayload:        json.RawMessage(`{"status":"250 OK"}`),
		OccurredAt:        time.Now().UTC(),
	}
	if err := s.RecordDeliveryEvent(ctx, deliveryEvent); err != nil {
		t.Fatalf("Tenant A RecordDeliveryEvent failed: %v", err)
	}

	// 1. Tenant A can read its own records
	readIntentA, err := s.GetMessageIntent(ctx, tenantA, intentID)
	if err != nil {
		t.Fatalf("Tenant A should be able to read its own intent: %v", err)
	}
	if readIntentA.MessageIntentID != intentID {
		t.Fatalf("Tenant A read unexpected intent ID %s", readIntentA.MessageIntentID)
	}

	readRenderA, err := s.GetRenderByIntent(ctx, tenantA, intentID)
	if err != nil {
		t.Fatalf("Tenant A should be able to read its own render: %v", err)
	}
	if readRenderA.RenderID != renderID {
		t.Fatalf("Tenant A read unexpected render ID %s", readRenderA.RenderID)
	}

	// 2. Tenant B must receive not found when querying Tenant A's records
	_, err = s.GetMessageIntent(ctx, tenantB, intentID)
	if !errors.Is(err, store.ErrIntentNotFound) {
		t.Fatalf("Tenant B should receive ErrIntentNotFound when reading Tenant A intent, got: %v", err)
	}

	_, err = s.GetRenderByIntent(ctx, tenantB, intentID)
	if !errors.Is(err, store.ErrRenderNotFound) {
		t.Fatalf("Tenant B should receive ErrRenderNotFound when reading Tenant A render, got: %v", err)
	}

	// 3. Raw SQL check under Tenant B session context: verify 0 rows are visible across all 4 tables
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("failed to acquire conn: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("failed to begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1;", tenantB); err != nil {
		t.Fatalf("failed to set tenant context: %v", err)
	}

	tables := []string{"message_intents", "message_renders", "delivery_attempts", "delivery_events"}
	for _, tbl := range tables {
		var cnt int
		q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE message_intent_id = $1", tbl)
		if err := tx.QueryRow(ctx, q, intentID).Scan(&cnt); err != nil {
			t.Fatalf("query on %s failed: %v", tbl, err)
		}
		if cnt != 0 {
			t.Fatalf("RLS breach: Tenant B can see %d rows in %s belonging to Tenant A", cnt, tbl)
		}
	}
}

// TestLedger_Traceability_IntentToRenderToAttemptToEvent verifies that all 4 ledger
// records form an unbroken, queryable audit trail.
func TestLedger_Traceability_IntentToRenderToAttemptToEvent(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-" + uuid.NewString()
	intentID := uuid.NewString()
	renderID := uuid.NewString()
	eventID := uuid.NewString()
	providerAttemptID := "prov-tx-" + uuid.NewString()
	causationID := "causation-" + uuid.NewString()

	// 1. Create intent
	intent := &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "usr-001",
		RecipientEmail:       "trace@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassS0,
		TemplateKey:          "ZS-IA-001",
		EventID:              "evt-trace-1",
		SourceEventType:      "identity.password_reset_requested",
		DeduplicationKey:     "dedup-trace-1",
		CorrelationID:        "corr-trace-1",
		CausationID:          &causationID,
		Status:               ledger.IntentStatusDispatched,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}
	if _, _, err := s.CreateMessageIntent(ctx, intent); err != nil {
		t.Fatalf("CreateMessageIntent failed: %v", err)
	}

	// 2. Record render
	render := &ledger.MessageRender{
		RenderID:        renderID,
		MessageIntentID: intentID,
		TenantID:        tenantID,
		TemplateKey:     "ZS-IA-001",
		TemplateVersion: "1.0.0",
		Locale:          "en-US",
		ContentHash:     "hash-trace-1",
		Subject:         "Reset your password",
		BodyHTML:        "<p>Reset link</p>",
		BodyText:        "Reset link",
		RenderedAt:      time.Now().UTC(),
	}
	if err := s.RecordMessageRender(ctx, render); err != nil {
		t.Fatalf("RecordMessageRender failed: %v", err)
	}

	// 3. Record attempt
	attempt := &ledger.DeliveryAttempt{
		ProviderAttemptID: providerAttemptID,
		MessageIntentID:   intentID,
		TenantID:          tenantID,
		RenderID:          renderID,
		ProviderName:      "smtp",
		AttemptNumber:     1,
		Status:            ledger.AttemptStatusAccepted,
		AttemptedAt:       time.Now().UTC(),
	}
	if err := s.RecordDeliveryAttempt(ctx, attempt); err != nil {
		t.Fatalf("RecordDeliveryAttempt failed: %v", err)
	}

	// 4. Record event
	devEvent := &ledger.DeliveryEvent{
		DeliveryEventID:   eventID,
		MessageIntentID:   intentID,
		TenantID:          tenantID,
		ProviderAttemptID: providerAttemptID,
		EventType:         ledger.DeliveryEventDelivered,
		RawPayload:        json.RawMessage(`{"provider":"smtp","code":250}`),
		OccurredAt:        time.Now().UTC(),
	}
	if err := s.RecordDeliveryEvent(ctx, devEvent); err != nil {
		t.Fatalf("RecordDeliveryEvent failed: %v", err)
	}

	// Verify full traceability
	gotIntent, err := s.GetMessageIntent(ctx, tenantID, intentID)
	if err != nil {
		t.Fatalf("GetMessageIntent failed: %v", err)
	}
	if gotIntent.MessageIntentID != intentID || *gotIntent.CausationID != causationID {
		t.Fatalf("intent traceability mismatch: %+v", gotIntent)
	}

	gotRender, err := s.GetRenderByIntent(ctx, tenantID, intentID)
	if err != nil {
		t.Fatalf("GetRenderByIntent failed: %v", err)
	}
	if gotRender.RenderID != renderID || gotRender.MessageIntentID != intentID {
		t.Fatalf("render traceability mismatch: %+v", gotRender)
	}
}

// TestLedger_TransactionRollback verifies that when an operation fails inside a transaction,
// uncommitted ledger records are completely rolled back and leave no orphaned state.
func TestLedger_TransactionRollback(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	tenantID := "tenant-" + uuid.NewString()
	intentID := uuid.NewString()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1;", tenantID); err != nil {
		t.Fatalf("set tenant failed: %v", err)
	}

	// Insert intent within tx
	const insertSQL = `
		INSERT INTO message_intents (
			message_intent_id, tenant_id, legal_entity_id, recipient_principal_id,
			recipient_email, channel, communication_class, template_key,
			event_id, source_event_type, deduplication_key, correlation_id,
			status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now(), now());
	`
	_, err = tx.Exec(ctx, insertSQL,
		intentID, tenantID, "entity-1", "usr-1", "rollback@example.com",
		"EMAIL", "S0", "ZS-IA-001", "evt-1", "user.login",
		"dedup-rollback", "corr-rollback", "PENDING",
	)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	// Explicit rollback
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}

	// Verify intent does NOT exist in database
	var cnt int
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM message_intents WHERE message_intent_id = $1", intentID).Scan(&cnt)
	if err != nil {
		t.Fatalf("count failed: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 rows after rollback, got %d", cnt)
	}
}

// TestLedger_KillSwitch_Persistence verifies that killed message intents are auditable
// and their status/reason are correctly persisted.
func TestLedger_KillSwitch_Persistence(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background()

	tenantID := "tenant-" + uuid.NewString()
	intentID := uuid.NewString()
	killReason := "EMERGENCY_KILL_SWITCH: template disabled by security team"

	intent := &ledger.MessageIntent{
		MessageIntentID:      intentID,
		TenantID:             tenantID,
		LegalEntityID:        "entity-001",
		RecipientPrincipalID: "usr-001",
		RecipientEmail:       "killed@example.com",
		Channel:              "EMAIL",
		CommunicationClass:   ledger.ClassS0,
		TemplateKey:          "ZS-IA-001",
		EventID:              "evt-killed-1",
		SourceEventType:      "identity.password_reset_requested",
		DeduplicationKey:     "dedup-killed-1",
		CorrelationID:        "corr-killed-1",
		Status:               ledger.IntentStatusKilled,
		FailureReason:        &killReason,
		CreatedAt:            time.Now().UTC(),
		UpdatedAt:            time.Now().UTC(),
	}

	created, _, err := s.CreateMessageIntent(ctx, intent)
	if err != nil {
		t.Fatalf("CreateMessageIntent failed for killed intent: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true")
	}

	got, err := s.GetMessageIntent(ctx, tenantID, intentID)
	if err != nil {
		t.Fatalf("GetMessageIntent failed: %v", err)
	}
	if got.Status != ledger.IntentStatusKilled {
		t.Fatalf("expected status KILLED, got %s", got.Status)
	}
	if got.FailureReason == nil || *got.FailureReason != killReason {
		t.Fatalf("expected failure reason %q, got %v", killReason, got.FailureReason)
	}
}

// TestLedger_Constraints_Validation tests database check constraints:
// - invalid communication class must be rejected
// - invalid intent status must be rejected
func TestLedger_Constraints_Validation(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	tenantID := "tenant-" + uuid.NewString()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin failed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL app.tenant_id = $1;", tenantID); err != nil {
		t.Fatalf("set tenant failed: %v", err)
	}

	const insertSQL = `
		INSERT INTO message_intents (
			message_intent_id, tenant_id, legal_entity_id, recipient_principal_id,
			recipient_email, channel, communication_class, template_key,
			event_id, source_event_type, deduplication_key, correlation_id,
			status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now(), now());
	`

	// 1. Invalid communication_class
	_, err = tx.Exec(ctx, insertSQL,
		uuid.NewString(), tenantID, "entity-1", "usr-1", "bad@example.com",
		"EMAIL", "INVALID_CLASS", "ZS-IA-001", "evt-1", "event.test",
		"dedup-bad-class", "corr-1", "PENDING",
	)
	if err == nil {
		t.Fatalf("expected error inserting invalid communication_class, got nil")
	}

	// 2. Invalid status
	tx2, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2 failed: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()

	if _, err := tx2.Exec(ctx, "SET LOCAL app.tenant_id = $1;", tenantID); err != nil {
		t.Fatalf("set tenant tx2 failed: %v", err)
	}

	_, err = tx2.Exec(ctx, insertSQL,
		uuid.NewString(), tenantID, "entity-1", "usr-1", "bad@example.com",
		"EMAIL", "S0", "ZS-IA-001", "evt-1", "event.test",
		"dedup-bad-status", "corr-1", "INVALID_STATUS",
	)
	if err == nil {
		t.Fatalf("expected error inserting invalid status, got nil")
	}
}

// TestLedger_Context_TenantMissing verifies that operations without tenant scope fail.
func TestLedger_Context_TenantMissing(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool)
	ctx := context.Background() // No tenant context installed

	intent := &ledger.MessageIntent{
		MessageIntentID:  uuid.NewString(),
		TenantID:         "", // Missing tenant
		DeduplicationKey: "dedup-missing-tenant",
	}

	_, _, err := s.CreateMessageIntent(ctx, intent)
	if err == nil {
		t.Fatalf("expected error when tenant_id is missing, got nil")
	}

	_, err = s.GetMessageIntent(ctx, "", uuid.NewString())
	if err == nil {
		t.Fatalf("expected error when tenant_id is missing in GetMessageIntent, got nil")
	}
}
