//go:build integration

// Real-Postgres tests for the outbox Relay's broker-down handling.
//
// Run: go test -v -tags=integration -count=1 -timeout=180s ./internal/outbox/
package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/outbox"
)

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	port := uint32(16701 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V16).Port(port).Database("finctrl_outbox_test").
		Username("postgres").Password("postgres"))
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	dsn := fmt.Sprintf("host=localhost port=%d dbname=finctrl_outbox_test user=postgres password=postgres sslmode=disable", port)
	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("connect: %v\n", err)
		_ = pg.Stop()
		os.Exit(1)
	}
	for i := 0; i < 75; i++ {
		if err = testPool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Printf("postgres not ready: %v\n", err)
		_ = pg.Stop()
		os.Exit(1)
	}
	// Every *.up.sql by glob, never a named list — same convention as internal/store.
	files, _ := filepath.Glob("../../deployments/migrations/*.up.sql")
	for _, f := range files {
		sqlBytes, rerr := os.ReadFile(f)
		if rerr == nil {
			_, rerr = testPool.Exec(ctx, string(sqlBytes))
		}
		if rerr != nil {
			fmt.Printf("migration %s: %v\n", f, rerr)
			_ = pg.Stop()
			os.Exit(1)
		}
	}
	code := m.Run()
	testPool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

// insertOutboxRow writes one unpublished row directly, bypassing outbox.Insert's
// transaction requirement — these tests only need a row to relay, not a
// business mutation to pair it with.
func insertOutboxRow(t *testing.T, ctx context.Context, eventType string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := testPool.Exec(ctx, `
		INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type, tenant_id, legal_entity_id, payload)
		VALUES ($1, 'control_run', 'run-1', $2, $3, $4, '{}'::jsonb)
	`, id, eventType, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	return id
}

type rowState struct {
	PublishAttempts int
	PublishedAt     *time.Time
	LastError       *string
}

func readRow(t *testing.T, ctx context.Context, id string) rowState {
	t.Helper()
	var s rowState
	err := testPool.QueryRow(ctx, `
		SELECT publish_attempts, published_at, last_error FROM outbox_events WHERE outbox_event_id = $1
	`, id).Scan(&s.PublishAttempts, &s.PublishedAt, &s.LastError)
	require.NoError(t, err)
	return s
}

// alwaysFailPublisher simulates every publish attempt failing — the Relay
// cannot tell from this alone whether the broker is down or the event is
// malformed; that distinction is exactly what the Prober supplies.
type alwaysFailPublisher struct{ err error }

func (p alwaysFailPublisher) PublishOutbox(context.Context, outbox.Delivery) error { return p.err }

type fakeProber struct{ err error }

func (p fakeProber) Probe(context.Context) error { return p.err }

func TestRelayOnce_BrokerDown_DoesNotChargeAttempts(t *testing.T) {
	ctx := context.Background()
	id := insertOutboxRow(t, ctx, "control.run.certified")

	publisher := alwaysFailPublisher{err: errors.New("dial tcp: connection refused")}
	prober := fakeProber{err: errors.New("broker unreachable")}
	relay := outbox.NewRelay(testPool, publisher, time.Hour, 50, zap.NewNop()).WithProber(prober)

	// Three ticks, as if the broker stayed down across several poll cycles.
	relay.RelayOnce(ctx)
	relay.RelayOnce(ctx)
	relay.RelayOnce(ctx)

	row := readRow(t, ctx, id)
	assert.Equal(t, 0, row.PublishAttempts, "a confirmed-down broker must never spend this event's attempt budget")
	assert.Nil(t, row.PublishedAt)
	assert.Nil(t, row.LastError, "no per-event error was ever recorded — the batch was released, not failed")
}

func TestRelayOnce_EventRejectedWhileBrokerIsUp_ChargesAttempts(t *testing.T) {
	ctx := context.Background()
	id := insertOutboxRow(t, ctx, "control.exception.opened")

	publisher := alwaysFailPublisher{err: errors.New("message size too large")}
	prober := fakeProber{err: nil} // broker reachable: the rejection is the event's own fault
	relay := outbox.NewRelay(testPool, publisher, time.Hour, 50, zap.NewNop()).WithProber(prober)

	relay.RelayOnce(ctx)

	row := readRow(t, ctx, id)
	assert.Equal(t, 1, row.PublishAttempts, "a reachable broker rejecting the event must still charge the attempt")
	assert.Nil(t, row.PublishedAt)
	require.NotNil(t, row.LastError)
	assert.Contains(t, *row.LastError, "message size too large")
}

func TestRelayOnce_NoProber_PreservesOriginalChargeOnFailureBehaviour(t *testing.T) {
	ctx := context.Background()
	id := insertOutboxRow(t, ctx, "control.run.failed")

	publisher := alwaysFailPublisher{err: errors.New("boom")}
	relay := outbox.NewRelay(testPool, publisher, time.Hour, 50, zap.NewNop()) // no WithProber

	relay.RelayOnce(ctx)

	row := readRow(t, ctx, id)
	assert.Equal(t, 1, row.PublishAttempts, "with no prober configured, behaviour must be unchanged from before this fix")
}

func TestRelayOnce_SuccessfulPublish_MarksPublished(t *testing.T) {
	ctx := context.Background()
	id := insertOutboxRow(t, ctx, "control.run.ready_for_certification")

	publisher := alwaysFailPublisher{err: nil}
	relay := outbox.NewRelay(testPool, publisher, time.Hour, 50, zap.NewNop())

	relay.RelayOnce(ctx)

	row := readRow(t, ctx, id)
	assert.Equal(t, 0, row.PublishAttempts)
	assert.NotNil(t, row.PublishedAt)
}
