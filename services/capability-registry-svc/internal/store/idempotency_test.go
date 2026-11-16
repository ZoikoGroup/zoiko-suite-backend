package store

import (
	"context"
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

	"zoiko.io/capability-registry-svc/internal/domain"
)

func openIdempotencyTestPool(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	schema, pool := openEmptyTestPool(t)
	applyMigrations(t, pool, func(string) bool { return true })
	return schema, pool
}

// openEmptyTestPool opens a pool on a fresh, isolated schema with no
// migrations applied.
func openEmptyTestPool(t *testing.T) (string, *pgxpool.Pool) {
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
	return schema, pool
}

// applyMigrations applies, in order, every up migration whose file name
// include accepts. The legacy-carry-over test uses it to stop before 000004,
// seed the old outbox_events table, and then apply 000004 on its own.
func applyMigrations(t *testing.T, pool *pgxpool.Pool, include func(name string) bool) {
	t.Helper()
	ctx := context.Background()
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
		if !include(filepath.Base(migration)) {
			continue
		}
		sql, err := os.ReadFile(migration)
		if err != nil {
			t.Fatalf("read migration %s: %v", filepath.Base(migration), err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply migration %s: %v", filepath.Base(migration), err)
		}
	}
}

// testEventRegion is the residency region the store stamps on events in
// these tests; production takes it from EVENT_RESIDENCY_REGION.
const testEventRegion = "test-region"

func newTestStore(pool *pgxpool.Pool) *PgStore {
	return NewPgStore(pool, WithEventRegion(testEventRegion))
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
		CorrelationID: "corr-" + key,
	}
}

func TestPgStoreCapabilityClaim_ExpiryReviewDateRoundTrip(t *testing.T) {
	_, pool := openIdempotencyTestPool(t)
	ctx := context.Background()
	store := newTestStore(pool)

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
			store := newTestStore(pool)
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
	err = newTestStore(restartedPool).CreateCapability(ctx, retryCapability, retryClaim)
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
	store := newTestStore(pool)

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
	store := newTestStore(pool)
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
