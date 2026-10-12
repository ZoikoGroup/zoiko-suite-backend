package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	"zoiko.io/capability-registry-svc/internal/outbox"
)

func openIdempotencyTestPool(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping PostgreSQL idempotency integration test: TEST_DATABASE_URL not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if !strings.Contains(strings.ToLower(strings.TrimPrefix(parsed.Path, "/")), "test") {
		t.Fatalf("refusing to create a temporary schema in non-test database %q", strings.TrimPrefix(parsed.Path, "/"))
	}

	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(adminPool.Close)

	schema := "capability_registry_idempotency_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop isolated test schema: %v", err)
		}
	})

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL pool config: %v", err)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = make(map[string]string)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open isolated test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	_, filename, _, _ := runtime.Caller(0)
	migrationDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")
	migrations, err := filepath.Glob(filepath.Join(migrationDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("find migrations: %v", err)
	}
	sort.Strings(migrations)
	if len(migrations) == 0 {
		t.Fatal("no capability-registry migrations found")
	}
	for _, migration := range migrations {
		sql, err := os.ReadFile(migration)
		if err != nil {
			t.Fatalf("read migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", filepath.Base(migration), err)
		}
	}
	return schema, pool
}

func newTestCapability(code string) *domain.Capability {
	return &domain.Capability{
		CapabilityID: uuid.NewString(), CapabilityCode: code, ModuleDomain: "test",
		Version: 1, ExecutionRiskClass: "LOW", CreatedAt: time.Now().UTC(),
		CreatedByPrincipalID: "idempotency-test-principal",
	}
}

func testIdempotencyClaim(tenant, operation, key, hash, resourceID string) domain.IdempotencyClaim {
	return domain.IdempotencyClaim{
		TenantID: tenant, Operation: operation, PrincipalID: "idempotency-test-principal",
		Key: key, RequestSHA256: hash, ResourceID: resourceID,
		ResponseStatus: 201, ResponseBody: []byte(`{"result":"original"}`),
		OutboxEventID: uuid.NewString(), OutboxEntityID: resourceID,
		OutboxPayload: []byte(`{"event_id":"evt-test","event_type":"test.created","entity_id":"` + resourceID + `"}`),
	}
}

func TestPgStoreCapabilityClaim_ExpiryReviewDateRoundTrip(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := NewPgStore(pool)

	capability := newTestCapability("CLAIM_EXPIRY_DATE")
	capabilityClaim := testIdempotencyClaim("tenant-date", "CreateCapability", "capability-key", "capability-request", capability.CapabilityID)
	if err := store.CreateCapability(ctx, capability, capabilityClaim); err != nil {
		t.Fatalf("create capability: %v", err)
	}

	date := time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		date *time.Time
	}{
		{name: "present", date: &date},
		{name: "omitted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			claimID := uuid.NewString()
			idempotency := testIdempotencyClaim("tenant-date", "CreateCapabilityClaim", "claim-key-"+test.name, "request-"+test.name, claimID)
			eventID := "evt-" + uuid.NewString()
			idempotency.OutboxPayload = []byte(`{"event_id":"` + eventID + `","event_type":"capability_claim.created"}`)
			claim := &domain.CapabilityClaim{
				ClaimID: claimID, CapabilityID: capability.CapabilityID, ClaimText: "expiry date persistence",
				WordingOwnerPrincipalID: "wording-owner", ApprovedByPrincipalID: "legal-approver",
				ExpiryReviewDate: test.date, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: "idempotency-test-principal",
			}
			if err := store.CreateCapabilityClaim(ctx, claim, idempotency); err != nil {
				t.Fatalf("create capability claim: %v", err)
			}
			got, err := store.GetCapabilityClaimByID(ctx, claimID)
			if err != nil {
				t.Fatalf("get capability claim: %v", err)
			}
			if test.date == nil {
				if got.ExpiryReviewDate != nil {
					t.Fatalf("expected omitted date to round-trip as nil, got %v", got.ExpiryReviewDate)
				}
				return
			}
			if got.ExpiryReviewDate == nil || !got.ExpiryReviewDate.Equal(*test.date) {
				t.Fatalf("expected date %v to round-trip, got %v", test.date, got.ExpiryReviewDate)
			}
		})
	}
}

func TestPgStoreIdempotency_ConcurrentRequestsAndDurableReplay(t *testing.T) {
	schema, pool := openIdempotencyTestPool(t)
	ctx := context.Background()

	type result struct {
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			capability := newTestCapability("IDEMPOTENCY_CONCURRENT")
			claim := testIdempotencyClaim("tenant-one", "CreateCapability", "concurrent-key", "same-request", capability.CapabilityID)
			store := NewPgStore(pool)
			ready.Done()
			<-start
			results <- result{err: store.CreateCapability(ctx, capability, claim)}
		}()
	}
	ready.Wait()
	close(start)

	var originals, replays int
	for range 2 {
		err := (<-results).err
		switch {
		case err == nil:
			originals++
		default:
			var replay *domain.IdempotentReplayError
			if errors.As(err, &replay) {
				replays++
			} else {
				t.Fatalf("concurrent write returned unexpected error: %v", err)
			}
		}
	}
	if originals != 1 || replays != 1 {
		t.Fatalf("expected one committed write and one replay, got originals=%d replays=%d", originals, replays)
	}

	var capabilityCount, claimCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM capabilities").Scan(&capabilityCount); err != nil {
		t.Fatalf("count capabilities: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM idempotency_keys").Scan(&claimCount); err != nil {
		t.Fatalf("count idempotency claims: %v", err)
	}
	if capabilityCount != 1 || claimCount != 1 {
		t.Fatalf("same-key concurrent requests left resources=%d claims=%d; want 1 each", capabilityCount, claimCount)
	}

	pool.Close()
	dsn := os.Getenv("TEST_DATABASE_URL")
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL after restart: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	restartedPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("reopen pool after simulated restart: %v", err)
	}
	t.Cleanup(restartedPool.Close)

	retryCapability := newTestCapability("IDEMPOTENCY_CONCURRENT")
	retryClaim := testIdempotencyClaim("tenant-one", "CreateCapability", "concurrent-key", "same-request", retryCapability.CapabilityID)
	err = NewPgStore(restartedPool).CreateCapability(ctx, retryCapability, retryClaim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		t.Fatalf("expected persisted replay claim after pool restart, got %v", err)
	}
	if replay.ResponseStatus != 201 || strings.ReplaceAll(string(replay.ResponseBody), " ", "") != `{"result":"original"}` {
		t.Fatalf("replay did not return the original persisted response: status=%d body=%s", replay.ResponseStatus, replay.ResponseBody)
	}
}

func TestPgStoreIdempotency_ConflictTenantAndOperationScopes(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := NewPgStore(pool)

	first := newTestCapability("IDEMPOTENCY_SCOPE_FIRST")
	if err := store.CreateCapability(ctx, first, testIdempotencyClaim(
		"tenant-one", "CreateCapability", "scope-key", "body-one", first.CapabilityID)); err != nil {
		t.Fatalf("create first capability: %v", err)
	}

	changed := newTestCapability("IDEMPOTENCY_SCOPE_CHANGED")
	err := store.CreateCapability(ctx, changed, testIdempotencyClaim(
		"tenant-one", "CreateCapability", "scope-key", "body-two", changed.CapabilityID))
	if !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("expected changed body to be refused, got %v", err)
	}

	otherTenant := newTestCapability("IDEMPOTENCY_SCOPE_TENANT_TWO")
	if err := store.CreateCapability(ctx, otherTenant, testIdempotencyClaim(
		"tenant-two", "CreateCapability", "scope-key", "body-two", otherTenant.CapabilityID)); err != nil {
		t.Fatalf("same key in a different tenant collided: %v", err)
	}

	release := &domain.Release{
		ReleaseID: uuid.NewString(), CapabilityID: first.CapabilityID, State: domain.ReleaseStateGA,
		EffectiveFrom: time.Now().UTC(), CreatedAt: time.Now().UTC(),
		CreatedByPrincipalID: "idempotency-test-principal",
	}
	if err := store.CreateRelease(ctx, release, testIdempotencyClaim(
		"tenant-one", "SetReleaseState", "scope-key", "body-two", release.ReleaseID)); err != nil {
		t.Fatalf("same key in a different operation collided: %v", err)
	}

	var capabilities, releases int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM capabilities").Scan(&capabilities); err != nil {
		t.Fatalf("count capabilities: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM releases").Scan(&releases); err != nil {
		t.Fatalf("count releases: %v", err)
	}
	if capabilities != 2 || releases != 1 {
		t.Fatalf("unexpected resources after body/tenant/operation tests: capabilities=%d releases=%d", capabilities, releases)
	}
}

func TestPgStoreIdempotency_ClaimUsesUniqueDatabaseIdentity(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := NewPgStore(pool)
	capability := newTestCapability("IDEMPOTENCY_CLAIM_KEY")
	claim := testIdempotencyClaim("tenant-one", "CreateCapability", "claim-key", "stable", capability.CapabilityID)
	if err := store.CreateCapability(ctx, capability, claim); err != nil {
		t.Fatalf("first write failed: %v", err)
	}
	duplicateKeyErr := store.withTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO idempotency_keys (
				tenant_id, operation, principal_id, idempotency_key, request_sha256,
				resource_id, response_status, response_body
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			claim.TenantID, claim.Operation, claim.PrincipalID, claim.Key,
			claim.RequestSHA256, claim.ResourceID, claim.ResponseStatus, claim.ResponseBody)
		return err
	})
	if duplicateKeyErr == nil {
		t.Fatal("expected PostgreSQL unique constraint to reject a duplicate idempotency identity")
	}
	var pgErr *pgconn.PgError
	if !errors.As(duplicateKeyErr, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected PostgreSQL unique violation, got %v", duplicateKeyErr)
	}
}

type testOutboxPublisher struct {
	failFirst bool
	calls     int
	eventIDs  []string
	payloads  [][]byte
}

func (p *testOutboxPublisher) PublishOutbox(_ context.Context, _ string, payload []byte) error {
	p.calls++
	var event struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return err
	}
	p.eventIDs = append(p.eventIDs, event.EventID)
	p.payloads = append(p.payloads, append([]byte(nil), payload...))
	if p.failFirst && p.calls == 1 {
		return errors.New("test Kafka unavailable")
	}
	return nil
}

func TestPgStoreOutbox_FailureRetriesSameEventWithoutDuplicateRows(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	capability := newTestCapability("OUTBOX_RETRY")
	claim := testIdempotencyClaim("tenant-outbox", "CreateCapability", "outbox-key", "same-body", capability.CapabilityID)
	if err := NewPgStore(pool).CreateCapability(ctx, capability, claim); err != nil {
		t.Fatalf("create capability and outbox event: %v", err)
	}

	var outboxRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events").Scan(&outboxRows); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxRows != 1 {
		t.Fatalf("successful write should atomically enqueue one event, got %d", outboxRows)
	}
	retryCapability := newTestCapability("OUTBOX_RETRY")
	replayErr := NewPgStore(pool).CreateCapability(ctx, retryCapability, claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(replayErr, &replay) {
		t.Fatalf("same-key retry should replay without creating a second outbox event, got %v", replayErr)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events").Scan(&outboxRows); err != nil {
		t.Fatalf("count outbox rows after idempotent replay: %v", err)
	}
	if outboxRows != 1 {
		t.Fatalf("idempotent replay created another outbox row: %d", outboxRows)
	}

	publisher := &testOutboxPublisher{failFirst: true}
	relay := outbox.NewRelay(pool, publisher, time.Second, 10, zap.NewNop())
	if err := relay.RelayOnce(ctx); err == nil {
		t.Fatal("expected publish failure to be surfaced by the relay")
	}
	var isPublished bool
	var attempts int
	var storedPayload []byte
	if err := pool.QueryRow(ctx, `
		SELECT published_at IS NOT NULL, publish_attempts, payload FROM outbox_events
	`).Scan(&isPublished, &attempts, &storedPayload); err != nil {
		t.Fatalf("read pending event after failure: %v", err)
	}
	if isPublished || attempts != 1 {
		t.Fatalf("failed event should remain pending and increment attempts: published=%v attempts=%d", isPublished, attempts)
	}

	if err := relay.RelayOnce(ctx); err != nil {
		t.Fatalf("retry event after Kafka recovers: %v", err)
	}
	if publisher.calls != 2 || len(publisher.payloads) != 2 {
		t.Fatalf("expected failure then one successful retry, got %d calls", publisher.calls)
	}
	if publisher.eventIDs[0] == "" || publisher.eventIDs[0] != publisher.eventIDs[1] {
		t.Fatalf("retry changed stable event ID: %v", publisher.eventIDs)
	}
	if string(publisher.payloads[0]) != string(publisher.payloads[1]) ||
		string(publisher.payloads[1]) != string(storedPayload) {
		t.Fatalf("retry changed event identity or payload: first=%s second=%s stored=%s",
			publisher.payloads[0], publisher.payloads[1], storedPayload)
	}

	if err := relay.RelayOnce(ctx); err != nil {
		t.Fatalf("poll after successful delivery: %v", err)
	}
	if publisher.calls != 2 {
		t.Fatalf("published outbox event was emitted again after success; calls=%d", publisher.calls)
	}
	if err := pool.QueryRow(ctx, "SELECT published_at IS NOT NULL FROM outbox_events").Scan(&isPublished); err != nil {
		t.Fatalf("read published state: %v", err)
	}
	if !isPublished {
		t.Fatal("successful publication did not mark the outbox event published")
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events").Scan(&outboxRows); err != nil {
		t.Fatalf("count outbox rows after retry: %v", err)
	}
	if outboxRows != 1 {
		t.Fatalf("retry created duplicate outbox rows: %d", outboxRows)
	}
}

func TestPgStoreOutbox_MarkPublishedFailureRetainsEventForRetry(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	capability := newTestCapability("OUTBOX_MARK_FAILED")
	claim := testIdempotencyClaim("tenant-outbox", "CreateCapability", "outbox-mark-key", "mark-body", capability.CapabilityID)
	if err := NewPgStore(pool).CreateCapability(ctx, capability, claim); err != nil {
		t.Fatalf("create capability and outbox event: %v", err)
	}

	_, err := pool.Exec(ctx, `
		CREATE FUNCTION reject_outbox_publish_mark() RETURNS trigger AS $$
		BEGIN
			IF NEW.published_at IS NOT NULL THEN
				RAISE EXCEPTION 'simulated PostgreSQL mark failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER reject_outbox_publish_mark
		BEFORE UPDATE ON outbox_events
		FOR EACH ROW EXECUTE FUNCTION reject_outbox_publish_mark();
	`)
	if err != nil {
		t.Fatalf("install simulated mark failure: %v", err)
	}

	publisher := &testOutboxPublisher{}
	relay := outbox.NewRelay(pool, publisher, time.Second, 10, zap.NewNop())
	if err := relay.RelayOnce(ctx); err == nil {
		t.Fatal("expected PostgreSQL published-state update failure to be surfaced")
	}
	var published bool
	var eventID string
	if err := pool.QueryRow(ctx, `
		SELECT published_at IS NOT NULL, payload->>'event_id' FROM outbox_events
	`).Scan(&published, &eventID); err != nil {
		t.Fatalf("read event after mark failure: %v", err)
	}
	if published {
		t.Fatal("event was marked delivered despite PostgreSQL marking failure")
	}
	if publisher.calls != 1 || len(publisher.eventIDs) != 1 || publisher.eventIDs[0] != eventID {
		t.Fatalf("first publish used unexpected event ID: calls=%d ids=%v stored=%s",
			publisher.calls, publisher.eventIDs, eventID)
	}

	if _, err := pool.Exec(ctx, "DROP TRIGGER reject_outbox_publish_mark ON outbox_events"); err != nil {
		t.Fatalf("drop simulated mark failure trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, "DROP FUNCTION reject_outbox_publish_mark()"); err != nil {
		t.Fatalf("drop simulated mark failure function: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE outbox_events SET claimed_until = NOW() - INTERVAL '1 second'"); err != nil {
		t.Fatalf("release expired outbox lease: %v", err)
	}
	if err := relay.RelayOnce(ctx); err != nil {
		t.Fatalf("retry after PostgreSQL marking recovers: %v", err)
	}
	if publisher.calls != 2 || publisher.eventIDs[1] != eventID {
		t.Fatalf("recovered retry did not use same event ID: calls=%d ids=%v stored=%s",
			publisher.calls, publisher.eventIDs, eventID)
	}
	if err := pool.QueryRow(ctx, "SELECT published_at IS NOT NULL FROM outbox_events").Scan(&published); err != nil {
		t.Fatalf("read event after recovery: %v", err)
	}
	if !published {
		t.Fatal("event was not marked published after successful recovery")
	}
	var outboxRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox_events").Scan(&outboxRows); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxRows != 1 {
		t.Fatalf("marking retry created duplicate outbox rows: %d", outboxRows)
	}
}

func TestPgStoreOutbox_InsertFailureRollsBackWriteAndIdempotencyClaim(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := NewPgStore(pool)
	first := newTestCapability("OUTBOX_ATOMICITY_FIRST")
	firstClaim := testIdempotencyClaim("tenant-outbox", "CreateCapability", "outbox-first", "first", first.CapabilityID)
	if err := store.CreateCapability(ctx, first, firstClaim); err != nil {
		t.Fatalf("create first resource: %v", err)
	}

	second := newTestCapability("OUTBOX_ATOMICITY_ROLLBACK")
	secondClaim := testIdempotencyClaim("tenant-outbox", "CreateCapability", "outbox-second", "second", second.CapabilityID)
	secondClaim.OutboxEventID = firstClaim.OutboxEventID
	if err := store.CreateCapability(ctx, second, secondClaim); err == nil {
		t.Fatal("expected duplicate outbox event ID to fail the write transaction")
	}

	var capabilities, idempotencyRows, outboxRows int
	for query, destination := range map[string]*int{
		"SELECT count(*) FROM capabilities":     &capabilities,
		"SELECT count(*) FROM idempotency_keys": &idempotencyRows,
		"SELECT count(*) FROM outbox_events":    &outboxRows,
	} {
		if err := pool.QueryRow(ctx, query).Scan(destination); err != nil {
			t.Fatalf("run %q: %v", query, err)
		}
	}
	if capabilities != 1 || idempotencyRows != 1 || outboxRows != 1 {
		t.Fatalf("failed outbox insert left partial state: resources=%d idempotency=%d outbox=%d",
			capabilities, idempotencyRows, outboxRows)
	}
}
