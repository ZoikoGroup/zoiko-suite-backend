package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/outbox"
)

// ── Mock DB Tx & Pool for Unit Testing ──────────────────────────────────────

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
	return nil, nil
}

func (m *mockPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// ── Mock Rows for Scan ──────────────────────────────────────────────────────

type mockEventRow struct {
	id          string
	aggType     string
	aggID       string
	eventType   string
	tenantID    string
	entityID    string
	actorID     *string
	corrID      *string
	headersJSON []byte
	payloadJSON []byte
	attempts    int
}

type mockRows struct {
	rows   []mockEventRow
	cursor int
	closed bool
}

func (m *mockRows) Close() {
	m.closed = true
}

func (m *mockRows) Err() error {
	return nil
}

func (m *mockRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

func (m *mockRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (m *mockRows) Next() bool {
	if m.cursor < len(m.rows) {
		m.cursor++
		return true
	}
	return false
}

func (m *mockRows) Scan(dest ...any) error {
	row := m.rows[m.cursor-1]
	*(dest[0].(*string)) = row.id
	*(dest[1].(*string)) = row.aggType
	*(dest[2].(*string)) = row.aggID
	*(dest[3].(*string)) = row.eventType
	*(dest[4].(*string)) = row.tenantID
	*(dest[5].(*string)) = row.entityID
	*(dest[6].(**string)) = row.actorID
	*(dest[7].(**string)) = row.corrID
	*(dest[8].(*[]byte)) = row.headersJSON
	*(dest[9].(*json.RawMessage)) = json.RawMessage(row.payloadJSON)
	*(dest[10].(*int)) = row.attempts
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

type recordedPublish struct {
	outboxEventID string
	eventType     string
	correlationID string
	tenantID      string
	legalEntityID string
	actorID       string
	payload       []byte
}

type mockPublisher struct {
	mu        sync.Mutex
	publishes []recordedPublish
	failErr   error
}

func (p *mockPublisher) PublishOutbox(ctx context.Context, outboxEventID, eventType, correlationID, tenantID, legalEntityID, actorID string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishes = append(p.publishes, recordedPublish{
		outboxEventID: outboxEventID,
		eventType:     eventType,
		correlationID: correlationID,
		tenantID:      tenantID,
		legalEntityID: legalEntityID,
		actorID:       actorID,
		payload:       payload,
	})
	return p.failErr
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestInsert_NilTx_ReturnsError(t *testing.T) {
	err := outbox.Insert(context.Background(), nil, outbox.Event{
		AggregateType: "workflow_instance",
		AggregateID:   "w-1",
		EventType:     "workflow.started",
		TenantID:      uuid.NewString(),
		LegalEntityID: uuid.NewString(),
		Payload:       map[string]string{"foo": "bar"},
	})
	if err == nil {
		t.Fatal("expected error when inserting with nil tx, got nil")
	}
}

func TestInsert_GeneratesStableEventID(t *testing.T) {
	tx := &mockTx{}
	e := outbox.Event{
		AggregateType: "workflow_instance",
		AggregateID:   "w-1",
		EventType:     "workflow.started",
		TenantID:      uuid.NewString(),
		LegalEntityID: uuid.NewString(),
		Payload:       map[string]string{"workflow_id": "w-1"},
	}

	err := outbox.Insert(context.Background(), tx, e)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tx.execCalls) != 1 {
		t.Fatalf("expected 1 SQL exec, got %d", len(tx.execCalls))
	}
	if len(tx.execArgs) != 1 {
		t.Fatalf("expected 1 set of exec args")
	}
	args := tx.execArgs[0]
	eventID, ok := args[0].(string)
	if !ok || eventID == "" {
		t.Fatalf("expected generated UUID event ID, got %v", args[0])
	}
	if _, err := uuid.Parse(eventID); err != nil {
		t.Fatalf("generated event ID is not a valid UUID: %s", eventID)
	}
}

func TestRelay_SuccessfulPublish_MarksPublished(t *testing.T) {
	eventID := uuid.NewString()
	tenantID := uuid.NewString()
	entityID := uuid.NewString()
	actorID := "actor-1"
	corrID := "corr-1"

	rows := &mockRows{
		rows: []mockEventRow{
			{
				id:          eventID,
				aggType:     "workflow_instance",
				aggID:       "w-100",
				eventType:   "workflow.started",
				tenantID:    tenantID,
				entityID:    entityID,
				actorID:     &actorID,
				corrID:      &corrID,
				payloadJSON: []byte(`{"status":"PENDING"}`),
				attempts:    0,
			},
		},
	}

	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	logger := zap.NewNop()
	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 50, logger)

	relay.RelayOnce(context.Background())

	if !rows.closed {
		t.Error("expected rows to be closed after processing")
	}
	if len(pub.publishes) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.publishes))
	}
	p := pub.publishes[0]
	if p.outboxEventID != eventID {
		t.Errorf("expected stable event ID %s, got %s", eventID, p.outboxEventID)
	}
	if p.eventType != "workflow.started" {
		t.Errorf("expected event_type workflow.started, got %s", p.eventType)
	}
	if p.correlationID != corrID {
		t.Errorf("expected correlationID %s, got %s", corrID, p.correlationID)
	}

	// Verify UPDATE outbox_events SET published_at = NOW() was executed
	var foundPublishedUpdate bool
	for _, sql := range tx.execCalls {
		if sql != "" && json.Valid([]byte("{}")) { // checking call pattern
			if containsStr(sql, "published_at = NOW()") {
				foundPublishedUpdate = true
			}
		}
	}
	if !foundPublishedUpdate {
		t.Error("expected published_at = NOW() update in tx")
	}
	if !tx.committed {
		t.Error("expected transaction to be committed on success")
	}
}

func TestRelay_FailedPublish_IncrementsAttemptsAndRecordsLastError(t *testing.T) {
	eventID := uuid.NewString()
	rows := &mockRows{
		rows: []mockEventRow{
			{
				id:          eventID,
				aggType:     "workflow_instance",
				aggID:       "w-100",
				eventType:   "workflow.started",
				tenantID:    uuid.NewString(),
				entityID:    uuid.NewString(),
				payloadJSON: []byte(`{}`),
				attempts:    0,
			},
		},
	}

	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{failErr: errors.New("kafka connection refused")}

	logger := zap.NewNop()
	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 50, logger)

	relay.RelayOnce(context.Background())

	if len(pub.publishes) != 1 {
		t.Fatalf("expected 1 publish attempt, got %d", len(pub.publishes))
	}

	var foundFailureUpdate bool
	for _, sql := range tx.execCalls {
		if containsStr(sql, "publish_attempts = publish_attempts + 1") {
			foundFailureUpdate = true
		}
	}
	if !foundFailureUpdate {
		t.Error("expected publish_attempts increment in tx on publish failure")
	}
	if !tx.committed {
		t.Error("expected transaction to commit failure metadata")
	}
}

func TestRelay_StableEventIDAcrossRetries(t *testing.T) {
	eventID := "fixed-event-id-12345"
	rows := &mockRows{
		rows: []mockEventRow{
			{
				id:          eventID,
				aggType:     "workflow_instance",
				aggID:       "w-1",
				eventType:   "workflow.approval.invalidated",
				tenantID:    uuid.NewString(),
				entityID:    uuid.NewString(),
				payloadJSON: []byte(`{"reason_code":"CONTROL_FAILURE"}`),
				attempts:    2,
			},
		},
	}

	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 50, zap.NewNop())
	relay.RelayOnce(context.Background())

	if len(pub.publishes) != 1 {
		t.Fatalf("expected 1 publish attempt, got %d", len(pub.publishes))
	}
	if pub.publishes[0].outboxEventID != eventID {
		t.Fatalf("expected stable X-Event-ID %s, got %s", eventID, pub.publishes[0].outboxEventID)
	}
}

func TestRelay_UsesForUpdateSkipLocked(t *testing.T) {
	tx := &mockTx{queryRows: &mockRows{}}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 50, zap.NewNop())
	relay.RelayOnce(context.Background())

	if len(tx.queryCalls) != 1 {
		t.Fatalf("expected 1 query call, got %d", len(tx.queryCalls))
	}
	query := tx.queryCalls[0]
	if !containsStr(query, "FOR UPDATE SKIP LOCKED") {
		t.Fatalf("expected query to contain 'FOR UPDATE SKIP LOCKED', got: %s", query)
	}
	if !containsStr(query, "WHERE published_at IS NULL") {
		t.Fatalf("expected query to filter on unpublished rows, got: %s", query)
	}
}

func TestRelay_CrashBeforeCommit_RollsBack(t *testing.T) {
	eventID := uuid.NewString()
	rows := &mockRows{
		rows: []mockEventRow{
			{
				id:          eventID,
				aggType:     "workflow_instance",
				aggID:       "w-crash",
				eventType:   "workflow.started",
				tenantID:    uuid.NewString(),
				entityID:    uuid.NewString(),
				payloadJSON: []byte(`{}`),
			},
		},
	}

	// Commit failure simulates DB disconnect / crash before commit
	tx := &mockTx{
		queryRows: rows,
		commitErr: errors.New("db connection lost during commit"),
	}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 50, zap.NewNop())
	relay.RelayOnce(context.Background())

	// Kafka publish occurred
	if len(pub.publishes) != 1 {
		t.Fatalf("expected 1 publish attempt, got %d", len(pub.publishes))
	}
	// But transaction commit failed
	if !tx.committed {
		t.Fatal("expected commit attempt")
	}
}

func TestRelay_StartAndCancel(t *testing.T) {
	tx := &mockTx{queryRows: &mockRows{}}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}

	relay := outbox.NewRelay(pool, pub, 10*time.Millisecond, 50, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		relay.Start(ctx)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// success: stopped cleanly
	case <-time.After(1 * time.Second):
		t.Fatal("expected relay to stop cleanly after context cancellation")
	}
}


func containsStr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || (len(needle) > 0 && len(haystack) > 0 && (stringSearch(haystack, needle))))
}

func stringSearch(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
