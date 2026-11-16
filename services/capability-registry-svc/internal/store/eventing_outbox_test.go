package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/capability-registry-svc/internal/domain"
	"zoiko.io/eventing/outbox"
)

// The migration must carry outbox.SchemaSQL and outbox.HardeningSQL verbatim.
// VerifySchema only catches a missing column at startup; this catches any
// drift — a changed type, index or trigger — at test time. Needs no database.
func TestEventingOutboxMigrationMatchesLibrary(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations", "000004_add_eventing_outbox.up.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	migration := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.Contains(migration, strings.TrimSpace(outbox.SchemaSQL)) {
		t.Fatal("000004 must contain outbox.SchemaSQL verbatim; copy it again from services/eventing/outbox/schema.go")
	}
	if !strings.Contains(migration, strings.TrimSpace(outbox.HardeningSQL)) {
		t.Fatal("000004 must contain outbox.HardeningSQL verbatim; copy it again from services/eventing/outbox/outbox.go")
	}
}

// expectedPartitionKey is envelope.PartitionKey's formula, written out
// independently so a change on either side is caught here.
func expectedPartitionKey(aggregateType, aggregateID string) string {
	sum := sha256.Sum256([]byte(domain.PlatformScopeID + "\x00" + aggregateType + "\x00" + aggregateID))
	return hex.EncodeToString(sum[:])
}

func countRows(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string) int {
	t.Helper()
	var n int
	if err := q.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

type outboxRow struct {
	eventID, eventType, schemaVersion, tenantID, region, partitionKey, state string
	aggregateID                                                              *string
	aggregateVersion                                                         *int64
	payload                                                                  map[string]any
}

func readNewestOutboxRow(t *testing.T, pool *pgxpool.Pool) outboxRow {
	t.Helper()
	var r outboxRow
	var payload []byte
	if err := pool.QueryRow(context.Background(), `
		SELECT event_id, event_type, schema_version, tenant_id, region, partition_key,
		       publish_state, aggregate_id, aggregate_version, payload
		FROM eventing_outbox ORDER BY outbox_id DESC LIMIT 1`).
		Scan(&r.eventID, &r.eventType, &r.schemaVersion, &r.tenantID, &r.region, &r.partitionKey,
			&r.state, &r.aggregateID, &r.aggregateVersion, &payload); err != nil {
		t.Fatalf("read newest outbox row: %v", err)
	}
	if err := json.Unmarshal(payload, &r.payload); err != nil {
		t.Fatalf("outbox payload is not JSON: %v (%s)", err, payload)
	}
	return r
}

// Every one of the six writes must enqueue exactly one event into
// eventing_outbox in its own transaction, carrying the platform scope, the
// configured region, the parent capability as its aggregate, and the legacy
// event_type consumers filter on today.
func TestEventingOutbox_EveryWriteEnqueuesOneEvent(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := newTestStore(pool)

	capability := newTestCapability("OUTBOX_EVERY_WRITE")
	integrationID := uuid.NewString()
	writes := []struct {
		canonical, legacy string
		write             func(domain.IdempotencyClaim) error
		op                string
	}{
		{eventCapabilityCreated, legacyCapabilityCreated, func(c domain.IdempotencyClaim) error {
			return store.CreateCapability(ctx, capability, c)
		}, "CreateCapability"},
		{eventMarketReleaseCreated, legacyMarketReleaseCreated, func(c domain.IdempotencyClaim) error {
			return store.CreateMarketRelease(ctx, &domain.MarketRelease{
				MarketReleaseID: uuid.NewString(), CapabilityID: capability.CapabilityID, MarketCode: "GB",
				LegalApprovalStatus: "APPROVED", State: domain.MarketReleaseGA, EffectiveFrom: time.Now().UTC(),
				CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "idempotency-test-principal",
			}, c)
		}, "CreateMarketRelease"},
		{eventIntegrationCapabilityCreated, legacyIntegrationCapabilityCreated, func(c domain.IdempotencyClaim) error {
			return store.CreateIntegrationCapability(ctx, &domain.IntegrationCapability{
				IntegrationCapabilityID: integrationID, CapabilityID: capability.CapabilityID, ProviderCode: "stripe",
				Certified: true, HealthStatus: "HEALTHY", CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "idempotency-test-principal",
			}, c)
		}, "CreateIntegrationCapability"},
		{eventIntegrationHealthUpdated, legacyIntegrationHealthUpdated, func(c domain.IdempotencyClaim) error {
			return store.UpdateIntegrationHealth(ctx, integrationID, "DEGRADED", c)
		}, "UpdateIntegrationHealth"},
		{eventReleaseStateChanged, legacyReleaseStateChanged, func(c domain.IdempotencyClaim) error {
			return store.CreateRelease(ctx, &domain.Release{
				ReleaseID: uuid.NewString(), CapabilityID: capability.CapabilityID, State: domain.ReleaseStateGA,
				EffectiveFrom: time.Now().UTC(), CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "idempotency-test-principal",
			}, c)
		}, "SetReleaseState"},
		{eventCapabilityClaimCreated, legacyCapabilityClaimCreated, func(c domain.IdempotencyClaim) error {
			return store.CreateCapabilityClaim(ctx, &domain.CapabilityClaim{
				ClaimID: uuid.NewString(), CapabilityID: capability.CapabilityID, ClaimText: "outbox claim",
				WordingOwnerPrincipalID: "owner", ApprovedByPrincipalID: "approver",
				CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "idempotency-test-principal",
			}, c)
		}, "CreateCapabilityClaim"},
	}

	wantKey := expectedPartitionKey(aggregateCapability, capability.CapabilityID)
	for i, w := range writes {
		claim := testIdempotencyClaim("caller-tenant", w.op, "every-write-"+w.op, "body", uuid.NewString())
		if err := w.write(claim); err != nil {
			t.Fatalf("%s: %v", w.op, err)
		}
		if got := countRows(t, pool, "eventing_outbox"); got != i+1 {
			t.Fatalf("%s: eventing_outbox has %d rows, want %d — each write enqueues exactly one event", w.op, got, i+1)
		}
		row := readNewestOutboxRow(t, pool)
		switch {
		case row.eventType != w.canonical:
			t.Errorf("%s: event_type = %q, want %q", w.op, row.eventType, w.canonical)
		case row.payload["event_type"] != w.legacy:
			t.Errorf("%s: legacy event_type = %v, want %q (existing consumers filter on it)", w.op, row.payload["event_type"], w.legacy)
		case row.tenantID != domain.PlatformScopeID || row.payload["tenant_id"] != domain.PlatformScopeID:
			t.Errorf("%s: tenant = %q / %v, want the platform scope — never the caller's X-Tenant-Id", w.op, row.tenantID, row.payload["tenant_id"])
		case row.region != testEventRegion:
			t.Errorf("%s: region = %q, want %q", w.op, row.region, testEventRegion)
		case row.aggregateID == nil || *row.aggregateID != capability.CapabilityID:
			t.Errorf("%s: aggregate_id = %v, want the parent capability %s", w.op, row.aggregateID, capability.CapabilityID)
		case row.aggregateVersion != nil:
			t.Errorf("%s: aggregate_version = %d, want NULL (this service tracks none)", w.op, *row.aggregateVersion)
		case row.partitionKey != wantKey:
			t.Errorf("%s: partition_key = %s, want %s — one capability's events must share a partition", w.op, row.partitionKey, wantKey)
		case row.state != outbox.StatePending:
			t.Errorf("%s: publish_state = %q, want pending", w.op, row.state)
		case row.payload["actor_id"] != claim.PrincipalID || row.payload["correlation_id"] != claim.CorrelationID:
			t.Errorf("%s: actor/correlation = %v/%v, want %s/%s", w.op, row.payload["actor_id"], row.payload["correlation_id"], claim.PrincipalID, claim.CorrelationID)
		case row.payload["event_id"] != row.eventID || row.schemaVersion != "1.0.0":
			t.Errorf("%s: event id/schema mismatch: body %v column %s schema %s", w.op, row.payload["event_id"], row.eventID, row.schemaVersion)
		}
	}
}

// A transaction that rolls back after the event was enqueued leaves neither
// the business row nor the event. The in-transaction count proves the event
// really was written before the rollback removed it, so this is not passing
// merely because the insert never ran.
func TestEventingOutbox_RolledBackTransactionLeavesNoEvent(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := newTestStore(pool)
	capability := newTestCapability("OUTBOX_ROLLBACK")
	claim := testIdempotencyClaim("caller-tenant", "CreateCapability", "rollback-key", "body", capability.CapabilityID)

	forced := errors.New("forced failure after enqueue")
	err := store.withTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO capabilities (capability_id, capability_code, module_domain, version,
				execution_risk_class, created_at, created_by_principal_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			capability.CapabilityID, capability.CapabilityCode, capability.ModuleDomain, capability.Version,
			capability.ExecutionRiskClass, capability.CreatedAt, capability.CreatedByPrincipalID); err != nil {
			return err
		}
		if err := store.enqueueEvent(ctx, tx, eventCapabilityCreated, legacyCapabilityCreated, capability.CapabilityID, claim, capability); err != nil {
			return err
		}
		if n := countRows(t, tx, "eventing_outbox"); n != 1 {
			t.Fatalf("inside the transaction eventing_outbox has %d rows, want 1", n)
		}
		return forced
	})
	if !errors.Is(err, forced) {
		t.Fatalf("withTx returned %v, want the forced failure", err)
	}
	if c, o := countRows(t, pool, "capabilities"), countRows(t, pool, "eventing_outbox"); c != 0 || o != 0 {
		t.Fatalf("rolled-back transaction left capabilities=%d eventing_outbox=%d, want 0/0", c, o)
	}

	// The real write path: a write the database refuses (nonexistent parent
	// capability) must leave no event and no idempotency claim behind.
	err = store.CreateMarketRelease(ctx, &domain.MarketRelease{
		MarketReleaseID: uuid.NewString(), CapabilityID: uuid.NewString(), MarketCode: "GB",
		LegalApprovalStatus: "APPROVED", State: domain.MarketReleaseGA, EffectiveFrom: time.Now().UTC(),
		CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "idempotency-test-principal",
	}, testIdempotencyClaim("caller-tenant", "CreateMarketRelease", "fk-key", "body", uuid.NewString()))
	if !errors.Is(err, domain.ErrCapabilityNotFound) {
		t.Fatalf("expected ErrCapabilityNotFound, got %v", err)
	}
	if o, k := countRows(t, pool, "eventing_outbox"), countRows(t, pool, "idempotency_keys"); o != 0 || k != 0 {
		t.Fatalf("refused write left eventing_outbox=%d idempotency_keys=%d, want 0/0", o, k)
	}
}

// An idempotent replay returns the original response and must not enqueue a
// second event for the same fact.
func TestEventingOutbox_ReplayEnqueuesNoSecondEvent(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := newTestStore(pool)
	capability := newTestCapability("OUTBOX_REPLAY")
	claim := testIdempotencyClaim("caller-tenant", "CreateCapability", "replay-key", "same-body", capability.CapabilityID)
	if err := store.CreateCapability(ctx, capability, claim); err != nil {
		t.Fatalf("first write: %v", err)
	}
	var replay *domain.IdempotentReplayError
	if err := store.CreateCapability(ctx, newTestCapability("OUTBOX_REPLAY"), claim); !errors.As(err, &replay) {
		t.Fatalf("same-key retry should replay, got %v", err)
	}
	if n := countRows(t, pool, "eventing_outbox"); n != 1 {
		t.Fatalf("replay enqueued another event: eventing_outbox has %d rows, want 1", n)
	}
}

// eventing_outbox may only ever hold the platform scope as tenant_id; the
// CHECK constraint (migration 000004, Part 3) refuses anything else in the
// INSERT itself.
func TestEventingOutbox_PlatformScopeConstraint(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO eventing_outbox (outbox_id, event_id, event_type, schema_version, tenant_id,
			region, partition_key, payload, payload_hash)
		VALUES ($1, 'evt-wrong-tenant', 'com.zoikosuite.platform.capability.created', '1.0.0',
			'some-real-tenant', 'test-region', repeat('a', 64), '\x7b7d', 'sha256:x')`, uuid.New())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "eventing_outbox_platform_scope" {
		t.Fatalf("expected eventing_outbox_platform_scope check violation, got %v", err)
	}
}

// recordingWriter is an outbox.Writer that records what the relay sends.
type recordingWriter struct {
	mu        sync.Mutex
	failCalls int // the first failCalls writes are rejected
	calls     int
	sent      []outbox.Message
}

func (w *recordingWriter) WriteMessages(_ context.Context, msgs ...outbox.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls <= w.failCalls {
		return errors.New("simulated broker rejection")
	}
	w.sent = append(w.sent, msgs...)
	return nil
}

func (w *recordingWriter) Probe(context.Context) error { return nil }

// The shared relay delivers an event this service wrote: keyed on the stored
// partition key, carrying the stored event id as X-Event-ID, then marked
// published so it is never sent again. An event-specific failure leaves the
// event in the outbox for retry with its id unchanged.
func TestEventingOutbox_SharedRelayDeliversServiceEvents(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	capability := newTestCapability("OUTBOX_RELAY")
	if err := newTestStore(pool).CreateCapability(ctx, capability,
		testIdempotencyClaim("caller-tenant", "CreateCapability", "relay-key", "body", capability.CapabilityID)); err != nil {
		t.Fatalf("create capability: %v", err)
	}
	stored := readNewestOutboxRow(t, pool)

	// Two failures: the relay first writes the whole batch, and when that
	// fails with the broker still up (Probe ok) it retries each message alone
	// to blame precisely. Failing both makes this an event-specific failure.
	writer := &recordingWriter{failCalls: 2}
	relay, err := outbox.NewRelay(pool, writer, outbox.DefaultConfig(), zap.NewNop())
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	res, err := relay.DrainOnce(ctx)
	if err != nil || res.Retrying != 1 || res.Published != 0 {
		t.Fatalf("first pass: res=%+v err=%v, want one event scheduled for retry", res, err)
	}
	var state string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT publish_state, attempt_count FROM eventing_outbox WHERE event_id = $1`, stored.eventID).
		Scan(&state, &attempts); err != nil {
		t.Fatalf("read after failure: %v", err)
	}
	if state != outbox.StateFailed || attempts != 1 {
		t.Fatalf("after a failed publish: state=%s attempts=%d, want failed/1", state, attempts)
	}

	// Make the retry due now rather than waiting out the backoff.
	if _, err := pool.Exec(ctx, `UPDATE eventing_outbox SET next_attempt_at = now() WHERE event_id = $1`, stored.eventID); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	res, err = relay.DrainOnce(ctx)
	if err != nil || res.Published != 1 {
		t.Fatalf("retry pass: res=%+v err=%v, want one published", res, err)
	}
	if len(writer.sent) != 1 {
		t.Fatalf("writer received %d messages, want 1", len(writer.sent))
	}
	msg := writer.sent[0]
	if string(msg.Key) != stored.partitionKey {
		t.Errorf("Kafka key = %s, want stored partition key %s", msg.Key, stored.partitionKey)
	}
	var headerID string
	for _, h := range msg.Headers {
		if h.Key == outbox.EventIDHeader {
			headerID = string(h.Value)
		}
	}
	if headerID != stored.eventID {
		t.Errorf("X-Event-ID = %q, want stored event id %q (retry must keep the id)", headerID, stored.eventID)
	}
	if err := pool.QueryRow(ctx, `SELECT publish_state FROM eventing_outbox WHERE event_id = $1`, stored.eventID).Scan(&state); err != nil {
		t.Fatalf("read after publish: %v", err)
	}
	if state != outbox.StatePublished {
		t.Fatalf("publish_state = %s, want published", state)
	}
	if res, err := relay.DrainOnce(ctx); err != nil || res.Claimed != 0 {
		t.Fatalf("a published event was claimed again: res=%+v err=%v", res, err)
	}
}

// Migration 000004 carries every undelivered outbox_events row over, keeps the
// table (evidence), and leaves already-published rows behind. The carried-over
// capability row lands on the same partition as new events for that
// capability, and the shared relay can deliver it with its original event id.
func TestEventingOutbox_MigrationCarriesOverUndeliveredLegacyRows(t *testing.T) {
	_, pool := openEmptyTestPool(t)
	ctx := context.Background()
	applyMigrations(t, pool, func(name string) bool { return name < "000004" })

	capabilityID, integrationID := uuid.NewString(), uuid.NewString()
	legacy := []struct {
		eventID, eventType, entityID string
		published                    bool
	}{
		{"evt-" + uuid.NewString(), "capability.created", capabilityID, false},
		{"evt-" + uuid.NewString(), "integration_capability.health_updated", integrationID, false},
		{"evt-" + uuid.NewString(), "capability.created", uuid.NewString(), true},
	}
	for _, l := range legacy {
		payload := `{"event_id":"` + l.eventID + `","event_type":"` + l.eventType + `","entity_id":"` + l.entityID + `","payload":{"k":"v"}}`
		var publishedAt any
		if l.published {
			publishedAt = time.Now().UTC()
		}
		if _, err := pool.Exec(ctx, `INSERT INTO outbox_events (outbox_event_id, entity_id, payload, published_at, publish_attempts, last_error)
			VALUES ($1, $2, $3, $4, 3, 'broker down')`, uuid.New(), l.entityID, payload, publishedAt); err != nil {
			t.Fatalf("seed legacy row: %v", err)
		}
	}

	applyMigrations(t, pool, func(name string) bool { return strings.HasPrefix(name, "000004") })

	if n := countRows(t, pool, "outbox_events"); n != 3 {
		t.Fatalf("outbox_events has %d rows after migration, want all 3 kept as evidence", n)
	}
	if n := countRows(t, pool, "eventing_outbox"); n != 2 {
		t.Fatalf("eventing_outbox has %d rows, want the 2 undelivered ones only", n)
	}
	wantKeys := map[string]string{
		legacy[0].eventID: expectedPartitionKey(aggregateCapability, capabilityID),
		legacy[1].eventID: expectedPartitionKey("integration-capability", integrationID),
	}
	for eventID, wantKey := range wantKeys {
		var tenant, region, key, state, eventType string
		var attempts int
		var aggregateID *string
		if err := pool.QueryRow(ctx, `SELECT tenant_id, region, partition_key, publish_state, event_type, attempt_count, aggregate_id
			FROM eventing_outbox WHERE event_id = $1`, eventID).
			Scan(&tenant, &region, &key, &state, &eventType, &attempts, &aggregateID); err != nil {
			t.Fatalf("carried-over row %s: %v", eventID, err)
		}
		if tenant != domain.PlatformScopeID || region != "legacy" || key != wantKey ||
			state != outbox.StatePending || attempts != 0 || aggregateID != nil {
			t.Errorf("carried-over row %s: tenant=%s region=%s key=%s state=%s attempts=%d aggregate=%v",
				eventID, tenant, region, key, state, attempts, aggregateID)
		}
	}

	writer := &recordingWriter{}
	relay, err := outbox.NewRelay(pool, writer, outbox.DefaultConfig(), zap.NewNop())
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	if res, err := relay.DrainOnce(ctx); err != nil || res.Published != 2 {
		t.Fatalf("relay over carried-over rows: res=%+v err=%v, want 2 published", res, err)
	}
	for _, msg := range writer.sent {
		var body map[string]any
		if err := json.Unmarshal(msg.Value, &body); err != nil {
			t.Fatalf("delivered body is not JSON: %v", err)
		}
		var headerID string
		for _, h := range msg.Headers {
			if h.Key == outbox.EventIDHeader {
				headerID = string(h.Value)
			}
		}
		if headerID == "" || headerID != body["event_id"] {
			t.Errorf("carried-over event delivered with X-Event-ID %q but body event_id %v — consumer dedupe would break", headerID, body["event_id"])
		}
	}
}
