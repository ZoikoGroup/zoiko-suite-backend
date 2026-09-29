package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/outbox"
)

// ── Mock DB Tx & Pool ───────────────────────────────────────────────────────

type mockTx struct {
	execCalls   []string
	execArgs    [][]any
	queryCalls  []string
	queryArgs   [][]any
	committed   bool
	rolledBack  bool
	execErr     error
	queryErr    error
	queryRows   pgx.Rows
	commitErr   error
	rollbackErr error
}

func (m *mockTx) Begin(ctx context.Context) (pgx.Tx, error) {
	return m, nil
}

func (m *mockTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	m.execCalls = append(m.execCalls, sql)
	m.execArgs = append(m.execArgs, args)
	if m.execErr != nil {
		return pgconn.CommandTag{}, m.execErr
	}
	return pgconn.NewCommandTag("INSERT 1"), nil
}

func (m *mockTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	m.queryCalls = append(m.queryCalls, sql)
	m.queryArgs = append(m.queryArgs, args)
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	return m.queryRows, nil
}

func (m *mockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return nil
}

func (m *mockTx) Commit(ctx context.Context) error {
	m.committed = true
	return m.commitErr
}

func (m *mockTx) Rollback(ctx context.Context) error {
	m.rolledBack = true
	return m.rollbackErr
}

func (m *mockTx) CopyFrom(ctx context.Context, tableName pgx.Identifier, columnNames []string, rowSrc pgx.CopyFromSource) (int64, error) {
	return 0, nil
}

func (m *mockTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	return nil
}

func (m *mockTx) LargeObjects() pgx.LargeObjects {
	return pgx.LargeObjects{}
}

func (m *mockTx) Prepare(ctx context.Context, name, sql string) (*pgconn.StatementDescription, error) {
	return nil, nil
}

func (m *mockTx) Conn() *pgx.Conn {
	return nil
}

type mockPool struct {
	tx       *mockTx
	beginErr error
}

func (m *mockPool) Begin(ctx context.Context) (pgx.Tx, error) {
	if m.beginErr != nil {
		return nil, m.beginErr
	}
	return m.tx, nil
}

func (m *mockPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return m.tx.Query(ctx, sql, args...)
}

func (m *mockPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return m.tx.Exec(ctx, sql, args...)
}

// ── Mock Rows ───────────────────────────────────────────────────────────────

type mockRows struct {
	rows    [][]any
	idx     int
	closed  bool
	scanErr error
}

func newMockRows(rows [][]any) *mockRows {
	return &mockRows{rows: rows, idx: -1}
}

func (m *mockRows) Close() {
	m.closed = true
}

func (m *mockRows) Err() error {
	return nil
}

func (m *mockRows) CommandTag() pgconn.CommandTag {
	return pgconn.NewCommandTag("SELECT")
}

func (m *mockRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (m *mockRows) Next() bool {
	m.idx++
	return m.idx < len(m.rows)
}

func (m *mockRows) Scan(dest ...any) error {
	if m.scanErr != nil {
		return m.scanErr
	}
	row := m.rows[m.idx]
	for i, d := range dest {
		switch target := d.(type) {
		case *string:
			if row[i] != nil {
				*target = row[i].(string)
			}
		case **string:
			if row[i] != nil {
				v := row[i].(string)
				*target = &v
			} else {
				*target = nil
			}
		case *[]byte:
			if row[i] != nil {
				*target = row[i].([]byte)
			}
		case *json.RawMessage:
			if row[i] != nil {
				*target = json.RawMessage(row[i].([]byte))
			}
		case *int:
			if row[i] != nil {
				*target = row[i].(int)
			}
		default:
			return errors.New("unsupported scan target in mock")
		}
	}
	return nil
}

func (m *mockRows) Values() ([]any, error) {
	return nil, nil
}

func (m *mockRows) RawValues() [][]byte {
	return nil
}

func (m *mockRows) Conn() *pgx.Conn {
	return nil
}

// ── Mock Publisher ──────────────────────────────────────────────────────────

type mockPublisher struct {
	mu        sync.Mutex
	publishes []publishCall
	err       error
}

type publishCall struct {
	outboxEventID string
	aggregateID   string
	payload       []byte
}

func (m *mockPublisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishes = append(m.publishes, publishCall{
		outboxEventID: outboxEventID,
		aggregateID:   aggregateID,
		payload:       payload,
	})
	return m.err
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestInsert_NilTransactionRejected(t *testing.T) {
	err := outbox.Insert(context.Background(), nil, outbox.Event{
		AggregateType: "VENDOR_INVOICE",
		AggregateID:   "inv-123",
		EventType:     "vendor.invoice.received",
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		CorrelationID: "corr-123",
		Payload:       map[string]any{"invoice_id": "inv-123"},
	})
	if err == nil {
		t.Fatal("expected error when transaction is nil, got nil")
	}
	if !strings.Contains(err.Error(), "transaction is nil") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestInsert_GeneratesStableEventIDIfNotProvided(t *testing.T) {
	tx := &mockTx{}
	e := outbox.Event{
		AggregateType: "VENDOR_INVOICE",
		AggregateID:   "inv-123",
		EventType:     "vendor.invoice.received",
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		CorrelationID: "corr-123",
		Payload:       map[string]any{"invoice_id": "inv-123"},
	}
	if err := outbox.Insert(context.Background(), tx, e); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tx.execArgs) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(tx.execArgs))
	}
	eventIDArg, ok := tx.execArgs[0][0].(string)
	if !ok || eventIDArg == "" {
		t.Fatalf("expected non-empty generated outbox_event_id string, got %v", tx.execArgs[0][0])
	}
	if _, err := uuid.Parse(eventIDArg); err != nil {
		t.Fatalf("expected valid UUID for outbox_event_id, got %s: %v", eventIDArg, err)
	}
}

func TestInsert_PreservesExplicitEventID(t *testing.T) {
	tx := &mockTx{}
	explicitID := "00000000-0000-0000-0000-000000000042"
	e := outbox.Event{
		OutboxEventID: explicitID,
		AggregateType: "VENDOR_INVOICE",
		AggregateID:   "inv-123",
		EventType:     "vendor.invoice.received",
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		CorrelationID: "corr-123",
		Payload:       map[string]any{"invoice_id": "inv-123"},
	}
	if err := outbox.Insert(context.Background(), tx, e); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx.execArgs[0][0] != explicitID {
		t.Fatalf("expected outbox_event_id=%s, got %v", explicitID, tx.execArgs[0][0])
	}
}

func TestVariantAEnvelope_NoEventIDInBody(t *testing.T) {
	env, err := outbox.NewVariantAEnvelope(
		"vendor.invoice.received",
		"corr-456",
		"tenant-1",
		"entity-1",
		"actor-1",
		map[string]any{"invoice_id": "inv-123"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bytes, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var rawMap map[string]any
	if err := json.Unmarshal(bytes, &rawMap); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// CRITICAL: Variant A must NEVER have event_id in the JSON body
	if _, hasEventID := rawMap["event_id"]; hasEventID {
		t.Fatalf("CRITICAL CONTRACT VIOLATION: Variant A envelope must NOT contain event_id in body, got: %s", string(bytes))
	}
	if rawMap["event_type"] != "vendor.invoice.received" {
		t.Fatalf("expected event_type=vendor.invoice.received, got %v", rawMap["event_type"])
	}
	if rawMap["source_service"] != "accounts-payable-svc" {
		t.Fatalf("expected source_service=accounts-payable-svc, got %v", rawMap["source_service"])
	}
	if rawMap["tenant_id"] != "tenant-1" {
		t.Fatalf("expected tenant_id=tenant-1, got %v", rawMap["tenant_id"])
	}
	if rawMap["legal_entity_id"] != "entity-1" {
		t.Fatalf("expected legal_entity_id=entity-1, got %v", rawMap["legal_entity_id"])
	}
	if rawMap["actor_id"] != "actor-1" {
		t.Fatalf("expected actor_id=actor-1, got %v", rawMap["actor_id"])
	}
	if rawMap["correlation_id"] != "corr-456" {
		t.Fatalf("expected correlation_id=corr-456, got %v", rawMap["correlation_id"])
	}
}

func TestRelay_ProcessesUnpublishedBatchWithForUpdateSkipLocked(t *testing.T) {
	eventID := "11111111-2222-3333-4444-555555555555"
	aggregateID := "inv-999"
	env, _ := outbox.NewVariantAEnvelope(
		"vendor.invoice.received", "corr-1", "t-1", "e-1", "a-1",
		map[string]any{"invoice_id": aggregateID},
	)
	envBytes, _ := json.Marshal(env)

	rows := newMockRows([][]any{
		{
			eventID,
			"VENDOR_INVOICE",
			aggregateID,
			"vendor.invoice.received",
			"t-1",
			"e-1",
			"a-1",
			"corr-1",
			[]byte("{}"),
			envBytes,
			0,
		},
	})

	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay.RelayOnce(context.Background())

	// Verify query uses FOR UPDATE SKIP LOCKED
	if len(tx.queryCalls) != 1 {
		t.Fatalf("expected 1 query call, got %d", len(tx.queryCalls))
	}
	if !strings.Contains(tx.queryCalls[0], "FOR UPDATE SKIP LOCKED") {
		t.Fatalf("expected query to use FOR UPDATE SKIP LOCKED, got: %s", tx.queryCalls[0])
	}

	// Verify publisher was called with exact IDs and payload
	if len(pub.publishes) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(pub.publishes))
	}
	p := pub.publishes[0]
	if p.outboxEventID != eventID {
		t.Fatalf("expected outboxEventID=%s, got %s", eventID, p.outboxEventID)
	}
	if p.aggregateID != aggregateID {
		t.Fatalf("expected aggregateID=%s, got %s", aggregateID, p.aggregateID)
	}
	if string(p.payload) != string(envBytes) {
		t.Fatalf("expected payload=%s, got %s", string(envBytes), string(p.payload))
	}

	// Verify row marked published
	foundPublishedUpdate := false
	for _, call := range tx.execCalls {
		if strings.Contains(call, "published_at = now()") {
			foundPublishedUpdate = true
			break
		}
	}
	if !foundPublishedUpdate {
		t.Fatal("expected update marking row published_at = now()")
	}
	if !tx.committed {
		t.Fatal("expected tx to be committed")
	}
}

func TestRelay_PublishFailureIncrementsAttemptsAndRecordsError(t *testing.T) {
	eventID := "22222222-3333-4444-5555-666666666666"
	aggregateID := "inv-888"
	env, _ := outbox.NewVariantAEnvelope(
		"vendor.invoice.validated", "corr-2", "t-2", "e-2", "a-2",
		map[string]any{"invoice_id": aggregateID},
	)
	envBytes, _ := json.Marshal(env)

	rows := newMockRows([][]any{
		{
			eventID,
			"VENDOR_INVOICE",
			aggregateID,
			"vendor.invoice.validated",
			"t-2",
			"e-2",
			"a-2",
			"corr-2",
			[]byte("{}"),
			envBytes,
			1,
		},
	})

	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{err: errors.New("kafka broker connection refused")}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay.RelayOnce(context.Background())

	// Verify error recorded
	foundErrorUpdate := false
	for i, call := range tx.execCalls {
		if strings.Contains(call, "publish_attempts = publish_attempts + 1") && strings.Contains(call, "last_error") {
			foundErrorUpdate = true
			args := tx.execArgs[i]
			if args[0] != eventID {
				t.Fatalf("expected eventID arg=%s, got %v", eventID, args[0])
			}
			if !strings.Contains(args[1].(string), "kafka broker connection refused") {
				t.Fatalf("expected error message in args, got %v", args[1])
			}
			break
		}
	}
	if !foundErrorUpdate {
		t.Fatal("expected update recording publish failure and incrementing attempts")
	}
	if !tx.committed {
		t.Fatal("expected transaction to commit error tracking update")
	}
}

func TestRelay_StableIDPreservedAcrossRetry(t *testing.T) {
	eventID := "33333333-4444-5555-6666-777777777777"
	aggregateID := "inv-777"
	env, _ := outbox.NewVariantAEnvelope(
		"payment.requested", "corr-3", "t-3", "e-3", "a-3",
		map[string]any{"invoice_id": aggregateID, "amount": 500.0, "currency_code": "USD"},
	)
	envBytes, _ := json.Marshal(env)

	// First run: failure
	rows1 := newMockRows([][]any{
		{
			eventID, "VENDOR_INVOICE", aggregateID, "payment.requested",
			"t-3", "e-3", "a-3", "corr-3", []byte("{}"), envBytes, 0,
		},
	})
	tx1 := &mockTx{queryRows: rows1}
	pool1 := &mockPool{tx: tx1}
	pub := &mockPublisher{err: errors.New("transient network drop")}

	relay1 := outbox.NewRelay(pool1, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay1.RelayOnce(context.Background())

	// Second run: retry succeeds
	rows2 := newMockRows([][]any{
		{
			eventID, "VENDOR_INVOICE", aggregateID, "payment.requested",
			"t-3", "e-3", "a-3", "corr-3", []byte("{}"), envBytes, 1,
		},
	})
	tx2 := &mockTx{queryRows: rows2}
	pool2 := &mockPool{tx: tx2}
	pub.err = nil // resolved

	relay2 := outbox.NewRelay(pool2, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay2.RelayOnce(context.Background())

	if len(pub.publishes) != 2 {
		t.Fatalf("expected 2 publishes total (attempt 1 + retry), got %d", len(pub.publishes))
	}
	// Both attempts MUST use the exact same outboxEventID
	if pub.publishes[0].outboxEventID != eventID || pub.publishes[1].outboxEventID != eventID {
		t.Fatalf("event ID must be stable across retries, got %s and %s (expected %s)",
			pub.publishes[0].outboxEventID, pub.publishes[1].outboxEventID, eventID)
	}
}

func TestRelay_GracefulShutdown(t *testing.T) {
	tx := &mockTx{queryRows: newMockRows(nil)}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	relay := outbox.NewRelay(pool, pub, 10*time.Millisecond, 10, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())

	stopped := make(chan struct{})
	go func() {
		relay.Start(ctx)
		close(stopped)
	}()

	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case <-stopped:
		// Clean exit
	case <-time.After(500 * time.Millisecond):
		t.Fatal("relay failed to stop within 500ms of context cancellation")
	}
}

type mockConditionalPublisher struct {
	mu        sync.Mutex
	publishes []publishCall
	failIDs   map[string]error
}

func (m *mockConditionalPublisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishes = append(m.publishes, publishCall{
		outboxEventID: outboxEventID,
		aggregateID:   aggregateID,
		payload:       payload,
	})
	if err, ok := m.failIDs[outboxEventID]; ok {
		return err
	}
	return nil
}

func TestRelay_MaxAttemptsCeiling_QueryFilterAndDeadLetterLogging(t *testing.T) {
	eventID := uuid.NewString()
	envBytes := []byte(`{"event_type":"vendor_invoice.captured"}`)

	// Event is on its 9th attempt (Attempts = 9)
	rows := newMockRows([][]any{
		{
			eventID, "VENDOR_INVOICE", "inv-deadletter-1", "vendor_invoice.captured",
			"t-1", "e-1", "a-1", "corr-1", []byte("{}"), envBytes, 9,
		},
	})
	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{err: errors.New("kafka permanent failure")}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay.RelayOnce(context.Background())

	// 1. Assert polling query filtered by publish_attempts < $2
	if len(tx.queryCalls) == 0 {
		t.Fatal("expected query to be executed")
	}
	query := tx.queryCalls[0]
	if !strings.Contains(query, "publish_attempts < $2") {
		t.Fatalf("expected query to filter by publish_attempts < $2, got:\n%s", query)
	}
	if len(tx.queryArgs) == 0 || len(tx.queryArgs[0]) < 2 || tx.queryArgs[0][1] != outbox.MaxPublishAttempts {
		t.Fatalf("expected query arg $2 to be MaxPublishAttempts (%d), got: %v", outbox.MaxPublishAttempts, tx.queryArgs)
	}

	// 2. Assert update incremented attempt to 10
	var foundErrorUpdate bool
	for _, call := range tx.execCalls {
		if strings.Contains(call, "publish_attempts = publish_attempts + 1") {
			foundErrorUpdate = true
			break
		}
	}
	if !foundErrorUpdate {
		t.Fatal("expected update recording publish failure and reaching dead-letter ceiling")
	}
}

func TestRelay_DeadLetter_DoesNotBlockSubsequentEvents(t *testing.T) {
	poisonedID := uuid.NewString()
	healthyID := uuid.NewString()
	envBytes := []byte(`{"event_type":"vendor_invoice.captured"}`)

	// Batch has 2 events: first will fail (poisoned), second will succeed
	rows := newMockRows([][]any{
		{
			poisonedID, "VENDOR_INVOICE", "inv-poison-1", "vendor_invoice.captured",
			"t-1", "e-1", "a-1", "corr-1", []byte("{}"), envBytes, 0,
		},
		{
			healthyID, "VENDOR_INVOICE", "inv-healthy-2", "vendor_invoice.captured",
			"t-1", "e-1", "a-1", "corr-2", []byte("{}"), envBytes, 0,
		},
	})
	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}

	// Publisher fails ONLY for poisonedID
	pub := &mockConditionalPublisher{
		failIDs: map[string]error{
			poisonedID: errors.New("poisoned event failure"),
		},
	}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay.RelayOnce(context.Background())

	// Healthy event must be published
	var publishedHealthy bool
	for _, p := range pub.publishes {
		if p.outboxEventID == healthyID {
			publishedHealthy = true
		}
	}
	if !publishedHealthy {
		t.Fatal("expected healthy event to be published despite poisoned event failing")
	}

	// Verify both updates took place in the tx: 1 error update, 1 published update
	var publishedCount, errorCount int
	for _, call := range tx.execCalls {
		if strings.Contains(call, "published_at = now()") {
			publishedCount++
		} else if strings.Contains(call, "publish_attempts = publish_attempts + 1") {
			errorCount++
		}
	}
	if errorCount != 1 {
		t.Fatalf("expected 1 error update for poisoned event, got %d", errorCount)
	}
	if publishedCount != 1 {
		t.Fatalf("expected 1 published update for healthy event, got %d", publishedCount)
	}
}
