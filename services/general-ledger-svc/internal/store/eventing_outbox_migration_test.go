package store_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/eventing/outbox"
)

func migrationDir(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "../../deployments/migrations")
}

// The migration must carry outbox.SchemaSQL verbatim. If the library's schema
// changes and this copy does not, VerifySchema would only catch a missing
// column at startup — this catches any drift at test time.
func TestEventingOutboxMigrationMatchesLibrary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(migrationDir(t), "000013_add_eventing_outbox.up.sql"))
	require.NoError(t, err)
	migration := strings.ReplaceAll(string(raw), "\r\n", "\n")
	assert.Contains(t, migration, outbox.SchemaSQL,
		"000013 must contain outbox.SchemaSQL verbatim; copy it again from services/eventing/outbox/schema.go")
}

func applyMigration(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	sql, err := os.ReadFile(filepath.Join(migrationDir(t), name))
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), string(sql))
	require.NoError(t, err, "apply %s", name)
}

// recordingWriter is an outbox.Writer that keeps what it was given.
type recordingWriter struct {
	mu   sync.Mutex
	msgs []outbox.Message
}

func (w *recordingWriter) WriteMessages(_ context.Context, msgs ...outbox.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, msgs...)
	return nil
}

func (w *recordingWriter) Probe(context.Context) error { return nil }

// 000013 must carry every undelivered legacy event into the new outbox so
// retiring the old relay loses nothing — and must not resurrect delivered
// ones, or turn the old relay's dead letters into fresh retries.
func TestMigration000013_CarriesOverUndeliveredLegacyEvents(t *testing.T) {
	pool := openTestPool(t) // applies everything; reset below to 000012
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DROP TABLE IF EXISTS eventing_outbox`)
	require.NoError(t, err)

	tenant := uuid.New().String()
	entity := uuid.New().String()
	pending, delivered, dead := uuid.New().String(), uuid.New().String(), uuid.New().String()
	legacyPayload := `{"event_type":"journal.posted","source_service":"general-ledger-svc","payload":{"journal_id":"j-1"}}`
	_, err = pool.Exec(ctx, `
		INSERT INTO outbox_events (outbox_event_id, aggregate_type, aggregate_id, event_type,
		                           tenant_id, legal_entity_id, correlation_id, payload,
		                           published_at, publish_attempts, last_error)
		VALUES ($1, 'JOURNAL', 'j-1', 'journal.posted', $4, $5, 'c-1', $6::jsonb, NULL, 3, 'broker down'),
		       ($2, 'JOURNAL', 'j-2', 'journal.posted', $4, $5, 'c-2', $6::jsonb, now(), 1, NULL),
		       ($3, 'JOURNAL', 'j-3', 'journal.posted', $4, $5, 'c-3', $6::jsonb, NULL, 10, 'record too large')`,
		pending, delivered, dead, tenant, entity, legacyPayload)
	require.NoError(t, err)

	applyMigration(t, pool, "000013_add_eventing_outbox.up.sql")

	type row struct {
		state, region, code string
		attempts            int
	}
	read := func(id string) (row, bool) {
		var r row
		var code *string
		err := pool.QueryRow(ctx, `
			SELECT publish_state, region, attempt_count, last_error_code
			  FROM eventing_outbox WHERE event_id = $1`, id).Scan(&r.state, &r.region, &r.attempts, &code)
		if err != nil {
			return row{}, false
		}
		if code != nil {
			r.code = *code
		}
		return r, true
	}

	p, ok := read(pending)
	require.True(t, ok, "undelivered legacy event was not carried over")
	assert.Equal(t, outbox.StatePending, p.state)
	assert.Equal(t, 0, p.attempts, "outage-era attempts must not count against the new retry budget")
	assert.Equal(t, "legacy", p.region)

	_, ok = read(delivered)
	assert.False(t, ok, "an already-delivered legacy event must not be published again")

	d, ok := read(dead)
	require.True(t, ok)
	assert.Equal(t, outbox.StateQuarantined, d.state, "the old relay's dead letters need an owner, not an automatic retry")
	assert.Equal(t, "MIGRATED_DEAD_LETTER", d.code)

	// The old table is evidence and stays intact.
	var legacyRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenant).Scan(&legacyRows))
	assert.Equal(t, 3, legacyRows)

	// The new relay delivers the carried-over event under its original id —
	// the X-Event-ID the old relay used — with only publishedat added.
	w := &recordingWriter{}
	relay, err := outbox.NewRelay(pool, w, outbox.DefaultConfig(), nil)
	require.NoError(t, err)
	res, err := relay.DrainOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Published, "only the pending legacy event should be delivered")
	require.Len(t, w.msgs, 1)
	msg := w.msgs[0]
	require.Len(t, msg.Headers, 1)
	assert.Equal(t, outbox.EventIDHeader, msg.Headers[0].Key)
	assert.Equal(t, pending, string(msg.Headers[0].Value))

	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(msg.Value, &body))
	var eventType string
	require.NoError(t, json.Unmarshal(body["event_type"], &eventType))
	assert.Equal(t, "journal.posted", eventType, "legacy envelope delivered unchanged")
	var publishedAt time.Time
	require.NoError(t, json.Unmarshal(body["publishedat"], &publishedAt))
	assert.WithinDuration(t, time.Now(), publishedAt, time.Minute)
}

// The down migration removes only the new table.
func TestMigration000013_DownThenUp(t *testing.T) {
	pool := openTestPool(t)
	applyMigration(t, pool, "000013_add_eventing_outbox.down.sql")
	require.Error(t, outbox.VerifySchema(context.Background(), pool), "down must drop eventing_outbox")
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_events`).Scan(&n),
		"down must leave the legacy outbox table alone")
	applyMigration(t, pool, "000013_add_eventing_outbox.up.sql")
	require.NoError(t, outbox.VerifySchema(context.Background(), pool))
}
