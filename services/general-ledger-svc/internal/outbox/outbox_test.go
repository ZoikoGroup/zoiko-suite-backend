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

	"zoiko.io/general-ledger-svc/internal/outbox"
)

// ── Mock DB Tx & Pool ───────────────────────────────────────────────────────

type mockTx struct {
	execCalls   []string
	execArgs    [][]any
	queryCalls  []string
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
		AggregateType: "JOURNAL",
		AggregateID:   "j-123",
		EventType:     "journal.created",
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		CorrelationID: "corr-123",
		Payload:       map[string]any{"journal_id": "j-123"},
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
		AggregateType: "JOURNAL",
		AggregateID:   "j-123",
		EventType:     "journal.created",
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		CorrelationID: "corr-123",
		Payload:       map[string]any{"journal_id": "j-123"},
	}
	if err := outbox.Insert(context.Background(), tx, e); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tx.execArgs) == 0 {
		t.Fatal("expected Exec to be called")
	}
	generatedID := tx.execArgs[0][0].(string)
	if _, err := uuid.Parse(generatedID); err != nil {
		t.Fatalf("expected valid UUID for outbox_event_id, got %q: %v", generatedID, err)
	}
}

func TestInsert_PreservesExplicitEventID(t *testing.T) {
	tx := &mockTx{}
	explicitID := uuid.NewString()
	e := outbox.Event{
		OutboxEventID: explicitID,
		AggregateType: "JOURNAL",
		AggregateID:   "j-123",
		EventType:     "journal.created",
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		CorrelationID: "corr-123",
		Payload:       map[string]any{"journal_id": "j-123"},
	}
	if err := outbox.Insert(context.Background(), tx, e); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotID := tx.execArgs[0][0].(string)
	if gotID != explicitID {
		t.Fatalf("expected explicit outbox_event_id %s, got %s", explicitID, gotID)
	}
}

func TestInsert_SerializesHeadersAndPayload(t *testing.T) {
	tx := &mockTx{}
	actor := "principal-123"
	e := outbox.Event{
		AggregateType: "JOURNAL",
		AggregateID:   "j-123",
		EventType:     "journal.created",
		TenantID:      "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222",
		ActorID:       &actor,
		CorrelationID: "corr-123",
		Headers:       map[string]string{"X-Source": "test-runner"},
		Payload:       map[string]any{"journal_id": "j-123", "fiscal_period": "2026-01"},
	}
	if err := outbox.Insert(context.Background(), tx, e); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	headersJSON := tx.execArgs[0][8].([]byte)
	var parsedHeaders map[string]string
	if err := json.Unmarshal(headersJSON, &parsedHeaders); err != nil {
		t.Fatalf("unmarshal headers failed: %v", err)
	}
	if parsedHeaders["X-Source"] != "test-runner" {
		t.Fatalf("expected header X-Source=test-runner, got %v", parsedHeaders)
	}

	payloadJSON := tx.execArgs[0][9].([]byte)
	var parsedPayload map[string]any
	if err := json.Unmarshal(payloadJSON, &parsedPayload); err != nil {
		t.Fatalf("unmarshal payload failed: %v", err)
	}
	if parsedPayload["journal_id"] != "j-123" {
		t.Fatalf("expected journal_id=j-123 in payload, got %v", parsedPayload)
	}
}

// ── Wire Contract Tests ─────────────────────────────────────────────────────

func TestVariantAEnvelope_NoEventIDInBody(t *testing.T) {
	env, err := outbox.NewVariantAEnvelope(
		"journal.created",
		"corr-123",
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"principal-123",
		map[string]any{
			"journal_id":      "j-123",
			"tenant_id":       "11111111-1111-1111-1111-111111111111",
			"legal_entity_id": "22222222-2222-2222-2222-222222222222",
			"fiscal_period":   "2026-01",
		},
	)
	if err != nil {
		t.Fatalf("failed to construct Variant A envelope: %v", err)
	}

	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("failed to marshal envelope: %v", err)
	}

	var rawMap map[string]any
	if err := json.Unmarshal(body, &rawMap); err != nil {
		t.Fatalf("failed to unmarshal into map: %v", err)
	}

	// CRITICAL: Variant A contract forbids event_id in the JSON body
	if _, exists := rawMap["event_id"]; exists {
		t.Fatalf("CRITICAL CONTRACT VIOLATION: event_id found in Variant A JSON body: %s", string(body))
	}
}

func TestVariantAEnvelope_WireContractShape(t *testing.T) {
	env, err := outbox.NewVariantAEnvelope(
		"journal.posted",
		"corr-456",
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"principal-post",
		map[string]any{
			"journal_id": "j-456",
		},
	)
	if err != nil {
		t.Fatalf("failed to construct Variant A envelope: %v", err)
	}

	if env.EventType != "journal.posted" {
		t.Fatalf("expected event_type=journal.posted, got %s", env.EventType)
	}
	if env.EventVersion != "1.0" {
		t.Fatalf("expected event_version=1.0, got %s", env.EventVersion)
	}
	if env.SchemaVersion != "1.0" {
		t.Fatalf("expected schema_version=1.0, got %s", env.SchemaVersion)
	}
	if env.SourceService != "general-ledger-svc" {
		t.Fatalf("expected source_service=general-ledger-svc, got %s", env.SourceService)
	}
	if env.CorrelationID != "corr-456" {
		t.Fatalf("expected correlation_id=corr-456, got %s", env.CorrelationID)
	}
	if env.TenantID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("expected tenant_id=11111111-1111-1111-1111-111111111111, got %s", env.TenantID)
	}
	if env.LegalEntityID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("expected legal_entity_id=22222222-2222-2222-2222-222222222222, got %s", env.LegalEntityID)
	}
	if env.ActorID != "principal-post" {
		t.Fatalf("expected actor_id=principal-post, got %s", env.ActorID)
	}
}

// ── Relay Engine Tests ──────────────────────────────────────────────────────

func TestRelay_PollsWithForUpdateSkipLocked(t *testing.T) {
	tx := &mockTx{
		queryRows: newMockRows(nil),
	}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}
	log := zap.NewNop()

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 25, log)
	relay.RelayOnce(context.Background())

	if len(tx.queryCalls) == 0 {
		t.Fatal("expected query to be executed by RelayOnce")
	}
	query := tx.queryCalls[0]
	if !strings.Contains(query, "FOR UPDATE SKIP LOCKED") {
		t.Fatalf("expected query to contain FOR UPDATE SKIP LOCKED, got:\n%s", query)
	}
}

func TestRelay_PublishesEventsAndMarksPublishedAt(t *testing.T) {
	evtID := uuid.NewString()
	env, _ := outbox.NewVariantAEnvelope(
		"journal.created", "c1", "t1", "e1", "p1",
		map[string]any{"journal_id": "j1"},
	)
	envJSON, _ := json.Marshal(env)

	actor := "p1"
	rows := newMockRows([][]any{
		{
			evtID, "JOURNAL", "j1", "journal.created",
			"t1", "e1", actor, "c1", []byte("{}"),
			envJSON, 0,
		},
	})
	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}
	log := zap.NewNop()

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, log)
	relay.RelayOnce(context.Background())

	if len(pub.publishes) != 1 {
		t.Fatalf("expected 1 publish call, got %d", len(pub.publishes))
	}
	call := pub.publishes[0]
	if call.outboxEventID != evtID {
		t.Fatalf("expected outboxEventID=%s, got %s", evtID, call.outboxEventID)
	}
	if call.aggregateID != "j1" {
		t.Fatalf("expected aggregateID=j1 (partition key), got %s", call.aggregateID)
	}

	var foundPublishedUpdate bool
	for _, sql := range tx.execCalls {
		if strings.Contains(sql, "UPDATE outbox_events") && strings.Contains(sql, "published_at = now()") {
			foundPublishedUpdate = true
			break
		}
	}
	if !foundPublishedUpdate {
		t.Fatalf("expected outbox_events to be marked published, calls: %v", tx.execCalls)
	}
	if !tx.committed {
		t.Fatal("expected tx to be committed on success")
	}
}

func TestRelay_IncrementsAttemptsAndRecordsLastErrorOnPublishFailure(t *testing.T) {
	evtID := uuid.NewString()
	env, _ := outbox.NewVariantAEnvelope(
		"journal.posted", "c2", "t1", "e1", "p1",
		map[string]any{"journal_id": "j2"},
	)
	envJSON, _ := json.Marshal(env)

	actor := "p1"
	rows := newMockRows([][]any{
		{
			evtID, "JOURNAL", "j2", "journal.posted",
			"t1", "e1", actor, "c2", []byte("{}"),
			envJSON, 0,
		},
	})
	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{err: errors.New("kafka broker unavailable")}
	log := zap.NewNop()

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, log)
	relay.RelayOnce(context.Background())

	var foundErrorUpdate bool
	for _, sql := range tx.execCalls {
		if strings.Contains(sql, "publish_attempts = publish_attempts + 1") && strings.Contains(sql, "last_error = $2") {
			foundErrorUpdate = true
			break
		}
	}
	if !foundErrorUpdate {
		t.Fatalf("expected publish failure to increment attempts and record last_error, got calls: %v", tx.execCalls)
	}
}

func TestRelay_GracefulShutdown(t *testing.T) {
	tx := &mockTx{queryRows: newMockRows(nil)}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}
	log := zap.NewNop()

	relay := outbox.NewRelay(pool, pub, 20*time.Millisecond, 10, log)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		relay.Start(ctx)
		close(stopped)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-stopped:
		// Clean exit
	case <-time.After(500 * time.Millisecond):
		t.Fatal("outbox relay failed to shut down within timeout after context cancellation")
	}
}

func TestRelay_ConcurrentWorkersDoNotClaimSameLockedRow(t *testing.T) {
	// Verifies that multi-replica safety relies on Postgres row-level locking
	// with FOR UPDATE SKIP LOCKED so concurrent relays do not duplicate events.
	tx1 := &mockTx{queryRows: newMockRows(nil)}
	tx2 := &mockTx{queryRows: newMockRows(nil)}

	pool1 := &mockPool{tx: tx1}
	pool2 := &mockPool{tx: tx2}

	pub := &mockPublisher{}
	log := zap.NewNop()

	r1 := outbox.NewRelay(pool1, pub, 50*time.Millisecond, 10, log)
	r2 := outbox.NewRelay(pool2, pub, 50*time.Millisecond, 10, log)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r1.RelayOnce(context.Background())
	}()
	go func() {
		defer wg.Done()
		r2.RelayOnce(context.Background())
	}()
	wg.Wait()

	if len(tx1.queryCalls) == 0 || len(tx2.queryCalls) == 0 {
		t.Fatal("expected both relays to query with lock")
	}
	if !strings.Contains(tx1.queryCalls[0], "FOR UPDATE SKIP LOCKED") ||
		!strings.Contains(tx2.queryCalls[0], "FOR UPDATE SKIP LOCKED") {
		t.Fatal("both concurrent queries must specify FOR UPDATE SKIP LOCKED")
	}
}

func TestRelay_CrashBeforePublishedAtCommit_EventEligibleForRetryWithSameXEventID(t *testing.T) {
	// Demonstrates at-least-once outbox semantics:
	// If Kafka publish succeeds but the database process crashes before the
	// UPDATE published_at commit completes, the row remains published_at IS NULL.
	// On next poll, the exact same outbox_event_id is fetched and republished,
	// allowing downstream consumers to deduplicate using the stable X-Event-ID header.
	evtID := uuid.NewString()
	env, _ := outbox.NewVariantAEnvelope(
		"journal.posted", "c3", "t1", "e1", "p1",
		map[string]any{"journal_id": "j3"},
	)
	envJSON, _ := json.Marshal(env)

	actor := "p1"
	rows1 := newMockRows([][]any{
		{
			evtID, "JOURNAL", "j3", "journal.posted",
			"t1", "e1", actor, "c3", []byte("{}"),
			envJSON, 0,
		},
	})
	// Simulate DB commit error (e.g. crash/kill right before commit)
	tx1 := &mockTx{
		queryRows: rows1,
		commitErr: errors.New("db connection terminated unexpectedly"),
	}
	pool1 := &mockPool{tx: tx1}
	pub := &mockPublisher{}
	log := zap.NewNop()

	relay1 := outbox.NewRelay(pool1, pub, 50*time.Millisecond, 10, log)
	relay1.RelayOnce(context.Background())

	if len(pub.publishes) != 1 {
		t.Fatalf("first attempt published %d times, expected 1", len(pub.publishes))
	}
	firstEventID := pub.publishes[0].outboxEventID

	// Next cycle: row was not committed as published, so it is picked up again
	rows2 := newMockRows([][]any{
		{
			evtID, "JOURNAL", "j3", "journal.posted",
			"t1", "e1", actor, "c3", []byte("{}"),
			envJSON, 1,
		},
	})
	tx2 := &mockTx{queryRows: rows2}
	pool2 := &mockPool{tx: tx2}
	relay2 := outbox.NewRelay(pool2, pub, 50*time.Millisecond, 10, log)
	relay2.RelayOnce(context.Background())

	if len(pub.publishes) != 2 {
		t.Fatalf("expected 2 total publish calls after retry, got %d", len(pub.publishes))
	}
	secondEventID := pub.publishes[1].outboxEventID

	// Crucial invariant: X-Event-ID MUST be identical across redeliveries for consumer deduplication
	if firstEventID != secondEventID || firstEventID != evtID {
		t.Fatalf("expected stable X-Event-ID across redelivery: %s vs %s", firstEventID, secondEventID)
	}
}
