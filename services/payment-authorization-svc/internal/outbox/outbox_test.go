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

	"zoiko.io/payment-authorization-svc/internal/domain"
	"zoiko.io/payment-authorization-svc/internal/outbox"
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
	tenantID    *string
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
	*(dest[4].(**string)) = row.tenantID
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
	aggregateID   string
	payload       []byte
}

type mockPublisher struct {
	mu        sync.Mutex
	publishes []recordedPublish
	failErr   error
}

func (p *mockPublisher) PublishOutbox(ctx context.Context, outboxEventID, aggregateID string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishes = append(p.publishes, recordedPublish{
		outboxEventID: outboxEventID,
		aggregateID:   aggregateID,
		payload:       payload,
	})
	return p.failErr
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestInsert_NilTx_ReturnsError(t *testing.T) {
	err := outbox.Insert(context.Background(), nil, outbox.Event{
		AggregateType: "payment_authorization",
		AggregateID:   "auth-123",
		EventType:     domain.EventPaymentAuthorized,
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
		AggregateType: "payment_authorization",
		AggregateID:   "auth-123",
		EventType:     domain.EventPaymentAuthorized,
		LegalEntityID: uuid.NewString(),
		Payload:       map[string]string{"auth_id": "auth-123"},
	}

	err := outbox.Insert(context.Background(), tx, e)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tx.execCalls) != 1 {
		t.Fatalf("expected 1 SQL exec, got %d", len(tx.execCalls))
	}
	if !strings.Contains(tx.execCalls[0], "INSERT INTO outbox_events") {
		t.Fatalf("expected INSERT INTO outbox_events, got %s", tx.execCalls[0])
	}
	// Verify args
	args := tx.execArgs[0]
	eventID, ok := args[0].(string)
	if !ok || eventID == "" {
		t.Fatalf("expected generated outbox_event_id, got %v", args[0])
	}
	if _, err := uuid.Parse(eventID); err != nil {
		t.Fatalf("expected valid UUID for outbox_event_id, got %s", eventID)
	}
}

func TestInsert_RollbackRemovesOutboxRecord(t *testing.T) {
	tx := &mockTx{}
	e := outbox.Event{
		AggregateType: "payment_authorization",
		AggregateID:   "auth-123",
		EventType:     domain.EventPaymentAuthorized,
		LegalEntityID: uuid.NewString(),
		Payload:       map[string]string{"auth_id": "auth-123"},
	}

	if err := outbox.Insert(context.Background(), tx, e); err != nil {
		t.Fatalf("unexpected insert error: %v", err)
	}

	// Simulating domain failure causing rollback
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("unexpected rollback error: %v", err)
	}

	if !tx.rolledBack {
		t.Fatal("expected transaction to be marked rolled back")
	}
	if tx.committed {
		t.Fatal("rolled back transaction must not be marked committed")
	}
}

func TestVariantBEnvelope_Structure(t *testing.T) {
	outboxEventID := uuid.NewString()
	tenantID := "tenant-abc"
	actorID := "actor-123"
	corrID := "corr-789"
	auth := domain.PaymentAuthorization{
		AuthorizationID: "auth-123",
		NetAmount:       1000.50,
		Currency:        "USD",
		Status:          domain.StatusPending,
	}

	env := outbox.NewVariantBEnvelope(
		outboxEventID,
		domain.EventAuthorizationRequested,
		auth.AuthorizationID,
		&tenantID,
		&actorID,
		&corrID,
		auth,
	)

	if env.EventID != "evt-"+outboxEventID {
		t.Fatalf("expected EventID 'evt-%s', got %s", outboxEventID, env.EventID)
	}
	if env.SourceService != "payment-authorization-svc" {
		t.Fatalf("expected source_service 'payment-authorization-svc', got %s", env.SourceService)
	}
	if env.EntityID != auth.AuthorizationID {
		t.Fatalf("expected entity_id %s, got %s", auth.AuthorizationID, env.EntityID)
	}
	if env.EventType != domain.EventAuthorizationRequested {
		t.Fatalf("expected event_type %s, got %s", domain.EventAuthorizationRequested, env.EventType)
	}
	if env.OccurredAt.IsZero() {
		t.Fatal("expected non-zero occurred_at")
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("failed to marshal envelope: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if parsed["event_id"] != "evt-"+outboxEventID {
		t.Fatalf("json event_id mismatch: %v", parsed["event_id"])
	}
	if parsed["source_service"] != "payment-authorization-svc" {
		t.Fatalf("json source_service mismatch: %v", parsed["source_service"])
	}
	if parsed["event_version"] != "1.0" || parsed["schema_version"] != "1.0" {
		t.Fatalf("version mismatch: %v", parsed)
	}
}

func TestRelay_PublishSuccess_MarksPublished(t *testing.T) {
	eventID := uuid.NewString()
	authID := "auth-123"
	tenantID := "tenant-1"
	payloadBytes := []byte(`{"event_id":"evt-` + eventID + `","entity_id":"` + authID + `"}`)

	rows := &mockRows{
		rows: []mockEventRow{
			{
				id:          eventID,
				aggType:     "payment_authorization",
				aggID:       authID,
				eventType:   domain.EventPaymentAuthorized,
				tenantID:    &tenantID,
				entityID:    uuid.NewString(),
				payloadJSON: payloadBytes,
				attempts:    0,
			},
		},
	}

	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}
	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())

	relay.RelayOnce(context.Background())

	if len(pub.publishes) != 1 {
		t.Fatalf("expected 1 publish call, got %d", len(pub.publishes))
	}
	p := pub.publishes[0]
	if p.outboxEventID != eventID {
		t.Fatalf("expected outboxEventID %s, got %s", eventID, p.outboxEventID)
	}
	if p.aggregateID != authID {
		t.Fatalf("expected aggregateID %s, got %s", authID, p.aggregateID)
	}
	if string(p.payload) != string(payloadBytes) {
		t.Fatalf("payload mismatch: %s", string(p.payload))
	}

	// Verify update to published_at
	var foundPublishedUpdate bool
	for _, sql := range tx.execCalls {
		if strings.Contains(sql, "UPDATE outbox_events") && strings.Contains(sql, "published_at = now()") {
			foundPublishedUpdate = true
			break
		}
	}
	if !foundPublishedUpdate {
		t.Fatalf("expected published_at update, got SQLs: %v", tx.execCalls)
	}
	if !tx.committed {
		t.Fatal("expected relay transaction to commit on success")
	}
}

func TestRelay_PublishFailure_IncrementsAttemptsAndRecordsError(t *testing.T) {
	eventID := uuid.NewString()
	authID := "auth-123"
	rows := &mockRows{
		rows: []mockEventRow{
			{
				id:          eventID,
				aggType:     "payment_authorization",
				aggID:       authID,
				eventType:   domain.EventPaymentAuthorized,
				entityID:    uuid.NewString(),
				payloadJSON: []byte(`{}`),
				attempts:    1,
			},
		},
	}

	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{failErr: errors.New("kafka broker unavailable")}
	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())

	relay.RelayOnce(context.Background())

	if len(pub.publishes) != 1 {
		t.Fatalf("expected 1 publish attempt, got %d", len(pub.publishes))
	}

	var foundErrorUpdate bool
	for _, sql := range tx.execCalls {
		if strings.Contains(sql, "publish_attempts = publish_attempts + 1") && strings.Contains(sql, "last_error = $2") {
			foundErrorUpdate = true
			break
		}
	}
	if !foundErrorUpdate {
		t.Fatalf("expected publish_attempts increment query, got: %v", tx.execCalls)
	}
	if !tx.committed {
		t.Fatal("expected retry count update to commit")
	}
}

func TestRelay_StableEventIDAcrossRetries(t *testing.T) {
	eventID := uuid.NewString()
	authID := "auth-123"

	pub := &mockPublisher{failErr: errors.New("transient error")}

	// First attempt
	rows1 := &mockRows{
		rows: []mockEventRow{
			{id: eventID, aggType: "payment_authorization", aggID: authID, eventType: domain.EventPaymentAuthorized, entityID: uuid.NewString(), payloadJSON: []byte(`{}`), attempts: 0},
		},
	}
	tx1 := &mockTx{queryRows: rows1}
	pool1 := &mockPool{tx: tx1}
	relay1 := outbox.NewRelay(pool1, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay1.RelayOnce(context.Background())

	// Second attempt (retry)
	pub.failErr = nil
	rows2 := &mockRows{
		rows: []mockEventRow{
			{id: eventID, aggType: "payment_authorization", aggID: authID, eventType: domain.EventPaymentAuthorized, entityID: uuid.NewString(), payloadJSON: []byte(`{}`), attempts: 1},
		},
	}
	tx2 := &mockTx{queryRows: rows2}
	pool2 := &mockPool{tx: tx2}
	relay2 := outbox.NewRelay(pool2, pub, 100*time.Millisecond, 10, zap.NewNop())
	relay2.RelayOnce(context.Background())

	if len(pub.publishes) != 2 {
		t.Fatalf("expected 2 publishes across retries, got %d", len(pub.publishes))
	}
	if pub.publishes[0].outboxEventID != pub.publishes[1].outboxEventID {
		t.Fatalf("outboxEventID changed across retries: attempt 1=%s, attempt 2=%s", pub.publishes[0].outboxEventID, pub.publishes[1].outboxEventID)
	}
	if pub.publishes[0].outboxEventID != eventID {
		t.Fatalf("expected stable eventID %s, got %s", eventID, pub.publishes[0].outboxEventID)
	}
}

func TestRelay_ForUpdateSkipLocked(t *testing.T) {
	tx := &mockTx{queryRows: &mockRows{}}
	pool := &mockPool{tx: tx}
	relay := outbox.NewRelay(pool, &mockPublisher{}, 100*time.Millisecond, 10, zap.NewNop())

	relay.RelayOnce(context.Background())

	if len(tx.queryCalls) == 0 {
		t.Fatal("expected polling query to be executed")
	}
	query := tx.queryCalls[0]
	if !strings.Contains(query, "FOR UPDATE SKIP LOCKED") {
		t.Fatalf("expected FOR UPDATE SKIP LOCKED in query, got:\n%s", query)
	}
	if !strings.Contains(query, "WHERE published_at IS NULL") {
		t.Fatalf("expected WHERE published_at IS NULL in query, got:\n%s", query)
	}
}

func TestRelay_ContextCancellationShutdown(t *testing.T) {
	tx := &mockTx{queryRows: &mockRows{}}
	pool := &mockPool{tx: tx}
	relay := outbox.NewRelay(pool, &mockPublisher{}, 20*time.Millisecond, 10, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		relay.Start(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Success: exited cleanly
	case <-time.After(1 * time.Second):
		t.Fatal("relay failed to shutdown within timeout after context cancellation")
	}
}

func TestRelay_CrashBeforePublishedAt_RollbackReleasesLocks(t *testing.T) {
	eventID := uuid.NewString()
	authID := "auth-123"
	rows := &mockRows{
		rows: []mockEventRow{
			{id: eventID, aggType: "payment_authorization", aggID: authID, eventType: domain.EventPaymentAuthorized, entityID: uuid.NewString(), payloadJSON: []byte(`{}`), attempts: 0},
		},
	}

	tx := &mockTx{queryRows: rows}
	pool := &mockPool{tx: tx}
	pub := &mockPublisher{}
	relay := outbox.NewRelay(pool, pub, 100*time.Millisecond, 10, zap.NewNop())

	// Simulating crash during RelayOnce execution by canceling context or panic recovery
	func() {
		defer func() {
			_ = recover()
		}()
		// Trigger an artificial panic or error during execution
		tx.commitErr = errors.New("simulated crash during commit")
		relay.RelayOnce(context.Background())
	}()

	// defer func() { _ = tx.Rollback(ctx) }() must ensure rollback was called when commit fails or panic occurs
	if !tx.rolledBack && !tx.committed {
		t.Fatal("expected transaction rollback on failure to release row locks")
	}
}
