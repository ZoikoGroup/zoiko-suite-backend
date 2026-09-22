package store_test

// Real-Postgres suite for PgStore. Same gating convention as every sibling
// service in this estate: skipped unless TEST_DATABASE_URL points at a Postgres
// this machine can use, then every migration is replayed onto a fresh schema so
// the assertions run against the database a deployment actually has — FORCE
// row-level security, the NOT VALID invariants, and the transactional outbox
// included.
//
// The service's own doc comment is worth quoting here because these tests write
// to it: "delegated authority must never exceed the delegator's own authority."
// That constraint lives in the handler, via live authorization-svc calls, and
// is not the store's to enforce — what the store owns, and what these tests
// pin, is that a grant that is written is a grant that is scoped, transitioned
// exactly once, and whose lifecycle event is queued atomically with the row.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/middleware"
	"zoiko.io/delegated-authority-svc/internal/store"
)

// requireTestDB yields a pool against TEST_DATABASE_URL, or skips — except
// where skipping would be a lie.
//
// A skip reports ok. That is the right answer on a developer laptop with no
// Postgres and the wrong one in CI, where a suite that silently ran nothing is
// indistinguishable from one that passed. Under CI or REQUIRE_DB_TESTS this
// fails instead. Same fix, same reasoning, as the sweep across the seven GOV
// services.
func requireTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("CI") != "" || os.Getenv("REQUIRE_DB_TESTS") != "" {
			t.Fatal("TEST_DATABASE_URL is not set, but CI or REQUIRE_DB_TESTS demands these run. " +
				"A skipped integration suite reports ok having verified nothing.")
		}
		t.Skip("TEST_DATABASE_URL not set — skipping real-Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	requireNotSuperuser(t, pool)

	// Fresh schema per test run. The outbox depends on nothing; the grants
	// table must be dropped last only because the outbox references no tables.
	_, err = pool.Exec(context.Background(), `
		DROP TABLE IF EXISTS delegation_outbox;
		DROP TABLE IF EXISTS delegation_grants;
	`)
	require.NoError(t, err)

	// Replay EVERY migration in order, discovered rather than listed — applying
	// only the initial schema builds a database no deployment has ever had and
	// silently skips what 000002's FORCE RLS and invariants assert. Globbing
	// means 000003 is picked up without anyone remembering to add it; sorting by
	// filename is what orders them, which is what the numeric prefix is for.
	_, filename, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(filename), "..", "..", "deployments", "migrations")

	migrations, err := filepath.Glob(filepath.Join(migDir, "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations, "no migrations found in %s", migDir)
	sort.Strings(migrations)

	for _, path := range migrations {
		sql, err := os.ReadFile(path)
		require.NoError(t, err, "reading migration %s", filepath.Base(path))
		_, err = pool.Exec(context.Background(), string(sql))
		require.NoError(t, err, "applying migration %s", filepath.Base(path))
	}

	return pool
}

// The two tenants every test splits between. The second is not filler: RLS and
// the explicit tenant predicate must keep tenant B unable to see, transition or
// expire tenant A's grants.
const (
	testTenantA = "11111111-1111-1111-1111-111111111111"
	testTenantB = "22222222-2222-2222-2222-222222222222"
	testEntityA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	testEntityB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// The principals used in sample grants. Distinct so a filter on delegator and
// one on delegate are distinguishable, and so the self-scope test has a
// principal who is delegator in one row, delegate in another, and absent in a
// third.
const (
	principalSelf  = "33333333-3333-3333-3333-333333333333"
	principalOther = "44444444-4444-4444-4444-444444444444"
	principalThird = "55555555-5555-5555-5555-555555555555"
)

func tenantCtx(tenantID string) context.Context {
	return middleware.WithTenant(context.Background(), tenantID)
}

// grant builds a DelegationGrant the way the handler would before handing it to
// the store, with every field the INSERT touches populated.
func grant(delegationID, correlationID, entity, delegator, delegate string, from, to time.Time) *domain.DelegationGrant {
	now := time.Now().UTC()
	return &domain.DelegationGrant{
		DelegationID:         delegationID,
		LegalEntityID:        entity,
		DelegatorPrincipalID: delegator,
		DelegatePrincipalID:  delegate,
		ActionType:           "PO_ISSUE",
		EffectiveFrom:        from,
		EffectiveTo:          to,
		Status:               domain.DelegationStatusActive,
		CreatedByPrincipalID: delegator,
		CorrelationID:        correlationID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
}

// outboxEvents reads back the enqueued lifecycle events for one delegation.
// The outbox table is FORCE RLS, and reads here are made as a request would be
// — with the tenant installed on the connection. This is exactly how the relay
// doesn't run, and asserting on events through the same lens the request path
// uses is the point.
func outboxEvents(t *testing.T, pool *pgxpool.Pool, tenantID, delegationID string) []string {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()

	_, err = tx.Exec(context.Background(), "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err)

	rows, err := tx.Query(context.Background(),
		`SELECT event_type FROM delegation_outbox WHERE delegation_id = $1 ORDER BY outbox_id`, delegationID)
	require.NoError(t, err)
	defer rows.Close()

	var out []string
	for rows.Next() {
		var e string
		require.NoError(t, rows.Scan(&e))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

// requireNotSuperuser refuses to run the isolation assertions as a superuser.
//
// This suite claims to prove tenant isolation, and under a superuser connection
// it proves nothing at all: Postgres exempts a superuser from row-level
// security unconditionally, and FORCE ROW LEVEL SECURITY forces it for the
// table OWNER — not for a superuser. So TestCrossTenantIsolation would pass
// against a database that was not enforcing the thing under test, and would go
// on passing if every policy were dropped.
//
// It passes for the right reason either way here, because pg_store.go carries
// an explicit tenant_id predicate on every statement. That is exactly what
// makes the hole dangerous: the belt holds, so nobody notices the braces are
// not fastened, and the day someone writes a query that leans on the policy is
// the day it is found in production.
func requireNotSuperuser(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var isSuper bool
	err := pool.QueryRow(context.Background(),
		"SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&isSuper)
	require.NoError(t, err, "checking whether the test role is a superuser")
	require.False(t, isSuper,
		"TEST_DATABASE_URL connects as a SUPERUSER, which bypasses row-level security "+
			"unconditionally. The isolation tests in this file would pass with every RLS "+
			"policy dropped. Point TEST_DATABASE_URL at a NOSUPERUSER NOBYPASSRLS role — "+
			"zoiko_app is the production runtime role and the one to reproduce.")
}

func TestCreateDelegationWritesAndEnqueuesDelegated(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	d := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00a1", "corr-a1", testEntityA, principalSelf, principalOther,
		time.Now().UTC(), time.Now().UTC().Add(30*24*time.Hour))

	created, err := s.CreateDelegation(ctx, d)
	require.NoError(t, err)
	require.True(t, created, "first create must report a real insert")
	require.Equal(t, testTenantA, d.TenantID, "the store stamps the tenant from the context")

	got, err := s.GetDelegation(ctx, d.DelegationID)
	require.NoError(t, err)
	require.Equal(t, d.DelegationID, got.DelegationID)
	require.Equal(t, principalOther, got.DelegatePrincipalID)
	require.Equal(t, domain.DelegationStatusActive, got.Status)
	require.Equal(t, "corr-a1", got.CorrelationID)

	events := outboxEvents(t, pool, testTenantA, d.DelegationID)
	require.Equal(t, []string{"authority.delegated"}, events,
		"the delegated event must be queued in the same transaction as the write")
}

func TestCreateDelegationIsIdempotentOnCorrelationID(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	d := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00a2", "corr-a2", testEntityA, principalSelf, principalOther,
		time.Now().UTC(), time.Now().UTC().Add(30*24*time.Hour))
	_, err := s.CreateDelegation(ctx, d)
	require.NoError(t, err)

	// A retried submission carries the same (tenant_id, correlation_id) and may
	// differ in every other field — a replay is not a fresh grant and must
	// neither write nor overwrite.
	replay := grant("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbba", "corr-a2", testEntityB, principalThird, principalOther,
		time.Now().UTC(), time.Now().UTC().Add(30*24*time.Hour))
	created, err := s.CreateDelegation(ctx, replay)
	require.NoError(t, err)
	require.False(t, created, "a replay must not report a real insert")

	// The replayed struct is resolved to the ORIGINAL grant, not to the retry's
	// fields or a fresh row.
	require.Equal(t, d.DelegationID, replay.DelegationID)
	require.Equal(t, principalSelf, replay.DelegatorPrincipalID)

	events := outboxEvents(t, pool, testTenantA, d.DelegationID)
	require.Equal(t, []string{"authority.delegated"}, events,
		"an idempotent retry must not emit a second authority.delegated")
}

func TestGetDelegationNotFoundAndMalformedID(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	_, err := s.GetDelegation(ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa9999")
	require.ErrorIs(t, err, domain.ErrDelegationNotFound, "a UUID nothing created is not found")

	// A non-UUID id reaches Postgres and comes back 22P02. The store must map
	// that to the same 404 the missing row gets, not leak it as a 503.
	_, err = s.GetDelegation(ctx, "not-a-uuid")
	require.ErrorIs(t, err, domain.ErrDelegationNotFound)
}

func TestCrossTenantIsolation(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctxA := tenantCtx(testTenantA)
	ctxB := tenantCtx(testTenantB)

	d := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00a3", "corr-a3", testEntityA, principalSelf, principalOther,
		time.Now().UTC(), time.Now().UTC().Add(30*24*time.Hour))
	_, err := s.CreateDelegation(ctxA, d)
	require.NoError(t, err)

	_, err = s.GetDelegation(ctxB, d.DelegationID)
	require.ErrorIs(t, err, domain.ErrDelegationNotFound,
		"tenant B must not read tenant A's grant")

	list, err := s.ListDelegations(ctxB, domain.ListDelegationsFilter{LegalEntityID: testEntityA})
	require.NoError(t, err)
	require.Empty(t, list, "tenant B's register must not include tenant A's grants")

	_, err = s.RevokeDelegation(ctxB, d.DelegationID, principalOther)
	require.ErrorIs(t, err, domain.ErrDelegationNotFound,
		"tenant B must not be able to transition tenant A's grant")

	expired, err := s.ExpireDue(ctxB)
	require.NoError(t, err)
	require.Empty(t, expired, "the sweep must not observe tenants it does not belong to")
}

func TestStoreRefusesUnscopedContext(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	bare := context.Background()

	_, err := s.CreateDelegation(bare, grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00a4", "corr-a4", testEntityA, principalSelf, principalOther,
		time.Now().UTC(), time.Now().UTC().Add(30*24*time.Hour)))
	require.ErrorIs(t, err, domain.ErrTenantMissing)

	_, err = s.GetDelegation(bare, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00a4")
	require.ErrorIs(t, err, domain.ErrTenantMissing)

	_, err = s.ListDelegations(bare, domain.ListDelegationsFilter{})
	require.ErrorIs(t, err, domain.ErrTenantMissing)

	_, err = s.RevokeDelegation(bare, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00a4", principalOther)
	require.ErrorIs(t, err, domain.ErrTenantMissing)

	_, err = s.ExpireDue(bare)
	require.ErrorIs(t, err, domain.ErrTenantMissing)
}

func TestListDelegationsFilters(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	now := time.Now().UTC()
	// PrincipalSelf is delegator of the first, delegate of the second, and
	// absent from the third — one principal, three scopes.
	grants := []*domain.DelegationGrant{
		grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00b1", "corr-b1", testEntityA, principalSelf, principalOther, now, now.Add(24*time.Hour)),
		grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00b2", "corr-b2", testEntityA, principalOther, principalSelf, now, now.Add(24*time.Hour)),
		grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00b3", "corr-b3", testEntityB, principalThird, principalOther, now, now.Add(24*time.Hour)),
	}
	for _, g := range grants {
		created, err := s.CreateDelegation(ctx, g)
		require.NoError(t, err)
		require.True(t, created)
	}

	list, err := s.ListDelegations(ctx, domain.ListDelegationsFilter{LegalEntityID: testEntityA})
	require.NoError(t, err)
	require.Len(t, list, 2, "the entity filter must cover only grants on that entity")

	byDelegator, err := s.ListDelegations(ctx, domain.ListDelegationsFilter{DelegatorPrincipalID: principalSelf})
	require.NoError(t, err)
	require.Len(t, byDelegator, 1)
	require.Equal(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00b1", byDelegator[0].DelegationID)

	byDelegate, err := s.ListDelegations(ctx, domain.ListDelegationsFilter{DelegatePrincipalID: principalSelf})
	require.NoError(t, err)
	require.Len(t, byDelegate, 1)
	require.Equal(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00b2", byDelegate[0].DelegationID)

	// The self scope: the caller is party to the first two but not the third.
	self, err := s.ListDelegations(ctx, domain.ListDelegationsFilter{SelfPrincipalID: principalSelf})
	require.NoError(t, err)
	require.Len(t, self, 2, "an unscoped self-read must answer only grants the caller is party to")
}

func TestListDelegationsStatusFilterAndUnknownStatus(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	now := time.Now().UTC()
	d := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00c1", "corr-c1", testEntityA, principalSelf, principalOther, now, now.Add(24*time.Hour))
	_, err := s.CreateDelegation(ctx, d)
	require.NoError(t, err)
	_, err = s.RevokeDelegation(ctx, d.DelegationID, principalOther)
	require.NoError(t, err)

	active, err := s.ListDelegations(ctx, domain.ListDelegationsFilter{Status: string(domain.DelegationStatusActive)})
	require.NoError(t, err)
	require.Empty(t, active, "the only grant is revoked, so ACTIVE finds nothing")

	revoked, err := s.ListDelegations(ctx, domain.ListDelegationsFilter{Status: string(domain.DelegationStatusRevoked)})
	require.NoError(t, err)
	require.Len(t, revoked, 1)
}

// TestListDelegationsPaging pins the tiebreaker comment in pg_store.go: the
// page must not return a row twice or skip one by paginating on created_at
// alone. Distinct timestamps would hide that class of bug, so they share one.
func TestListDelegationsPaging(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	now := time.Now().UTC()
	for i, id := range []string{
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00d1",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00d2",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00d3",
	} {
		d := grant(id, "corr-d"+id[31:], testEntityA, principalSelf, principalOther, now, now.Add(24*time.Hour))
		d.CreatedAt = now
		created, err := s.CreateDelegation(ctx, d)
		require.NoError(t, err)
		require.True(t, created, "grant %d", i)
		// Sleeping is forbidden here the same way it is forbidden in production
		// ordering arguments: all three rows share created_at.
	}

	pageOne, err := s.ListDelegations(ctx, domain.ListDelegationsFilter{Limit: 2, Offset: 0})
	require.NoError(t, err)
	require.Len(t, pageOne, 2)

	pageTwo, err := s.ListDelegations(ctx, domain.ListDelegationsFilter{Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, pageTwo, 1)

	seen := make(map[string]bool)
	for _, page := range [][]domain.DelegationGrant{pageOne, pageTwo} {
		for _, d := range page {
			require.False(t, seen[d.DelegationID], "delegation %s returned on two pages", d.DelegationID)
			seen[d.DelegationID] = true
		}
	}
	require.Len(t, seen, 3, "paging must cover the register exactly once")
}

func TestRevokeDelegationTransitionsAndEnqueuesRevoked(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	d := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00e1", "corr-e1", testEntityA, principalSelf, principalOther,
		time.Now().UTC(), time.Now().UTC().Add(24*time.Hour))
	_, err := s.CreateDelegation(ctx, d)
	require.NoError(t, err)

	revoked, err := s.RevokeDelegation(ctx, d.DelegationID, principalOther)
	require.NoError(t, err)
	require.Equal(t, domain.DelegationStatusRevoked, revoked.Status)
	require.Equal(t, principalOther, *revoked.RevokedByPrincipalID)
	require.NotNil(t, revoked.RevokedAt)

	events := outboxEvents(t, pool, testTenantA, d.DelegationID)
	require.Equal(t, []string{"authority.delegated", "authority.revoked"}, events,
		"the revocation event must be queued atomically with the transition")

	// A terminal grant cannot be revoked again — the second attempt is a 409,
	// not a silent no-op and not a fresh state.
	_, err = s.RevokeDelegation(ctx, d.DelegationID, principalOther)
	require.ErrorIs(t, err, domain.ErrInvalidTransition)
	require.True(t, errors.Is(err, domain.ErrInvalidTransition))

	events = outboxEvents(t, pool, testTenantA, d.DelegationID)
	require.Equal(t, []string{"authority.delegated", "authority.revoked"}, events,
		"a refused re-revocation must not emit a second authority.revoked")
}

func TestExpireDueFlipsPastWindowAndEnqueuesExpired(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	now := time.Now().UTC()
	due := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00f1", "corr-f1", testEntityA, principalSelf, principalOther,
		now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	open := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00f2", "corr-f2", testEntityA, principalSelf, principalOther,
		now, now.Add(24*time.Hour))
	_, err := s.CreateDelegation(ctx, due)
	require.NoError(t, err)
	_, err = s.CreateDelegation(ctx, open)
	require.NoError(t, err)

	expired, err := s.ExpireDue(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 1, "only the grant whose window has closed flips")
	require.Equal(t, due.DelegationID, expired[0].DelegationID)
	require.Equal(t, domain.DelegationStatusExpired, expired[0].Status)
	require.NotNil(t, expired[0].ExpiredAt)

	// The sweep's event is queued in the sweep's transaction — observing the
	// lapse is a state change exactly like a revoke, and the same atomicity
	// applies.
	events := outboxEvents(t, pool, testTenantA, due.DelegationID)
	require.Equal(t, []string{"authority.delegated", "authority.expired"}, events)

	events = outboxEvents(t, pool, testTenantA, open.DelegationID)
	require.Equal(t, []string{"authority.delegated"}, events,
		"an in-window grant must not be expired or evented")

	// A second sweep finds nothing left to flip, so nothing new is published.
	again, err := s.ExpireDue(ctx)
	require.NoError(t, err)
	require.Empty(t, again)
	events = outboxEvents(t, pool, testTenantA, due.DelegationID)
	require.Equal(t, []string{"authority.delegated", "authority.expired"}, events,
		"expiry must be observed once, not once per read")
}

func TestRelayDrainsEachEventExactlyOnce(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	d := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa00e2", "corr-g1", testEntityA, principalSelf, principalOther,
		time.Now().UTC(), time.Now().UTC().Add(24*time.Hour))
	_, err := s.CreateDelegation(ctx, d)
	require.NoError(t, err)

	pending, oldestAge, err := s.OutboxDepth(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), pending)
	require.GreaterOrEqual(t, oldestAge, time.Duration(0))

	var claimed []store.OutboxRecord
	err = s.ClaimOutbox(context.Background(), 10, func(recs []store.OutboxRecord) error {
		claimed = append(claimed, recs...)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, "authority.delegated", claimed[0].EventType)
	require.Equal(t, d.DelegationID, claimed[0].Key)

	// Published rows must not be claimed again: a second drain plumbs nothing,
	// so at-least-once delivery collapses here for a healthy relay.
	pending, _, err = s.OutboxDepth(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(0), pending)

	err = s.ClaimOutbox(context.Background(), 10, func(recs []store.OutboxRecord) error {
		require.Empty(t, recs, "a published row must not be redelivered")
		return nil
	})
	require.NoError(t, err)
}

// expired_at must name the moment the AUTHORITY ended, not the moment this
// service got around to noticing. They used to be the same column, so a grant
// that lapsed on a Friday and was next swept on Monday asserted in evidence
// that its authority ran all weekend.
func TestExpireDueRecordsWhenAuthorityEndedNotWhenObserved(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	now := time.Now().UTC()
	endedAt := now.Add(-72 * time.Hour)
	g := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa0201", "corr-0201", testEntityA, principalSelf, principalOther,
		now.Add(-96*time.Hour), endedAt)
	_, err := s.CreateDelegation(ctx, g)
	require.NoError(t, err)

	expired, err := s.ExpireDue(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	require.NotNil(t, expired[0].ExpiredAt)

	require.WithinDuration(t, endedAt, *expired[0].ExpiredAt, time.Second,
		"expired_at must equal effective_to — the authority ended when its window closed")
	require.WithinDuration(t, now, expired[0].UpdatedAt, time.Minute,
		"updated_at is the observation time, and keeps it")
	require.True(t, expired[0].UpdatedAt.Sub(*expired[0].ExpiredAt) > 48*time.Hour,
		"the two timestamps must be able to differ; conflating them is the defect")
}

// The background sweep crosses tenants. A tenant whose register nobody reads
// previously expired nothing at all, so its delegates kept authority
// indefinitely and authority.expired was never published for them.
func TestExpireDueAllTenantsCrossesTenantBoundaries(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)

	now := time.Now().UTC()
	from, to := now.Add(-48*time.Hour), now.Add(-24*time.Hour)

	a := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa0301", "corr-0301", testEntityA, principalSelf, principalOther, from, to)
	_, err := s.CreateDelegation(tenantCtx(testTenantA), a)
	require.NoError(t, err)

	b := grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa0302", "corr-0302", testEntityB, principalSelf, principalOther, from, to)
	_, err = s.CreateDelegation(tenantCtx(testTenantB), b)
	require.NoError(t, err)

	// No tenant installed: this is the background loop, which belongs to none.
	expired, err := s.ExpireDueAllTenants(context.Background(), 100)
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, e := range expired {
		ids[e.DelegationID] = true
		require.Equal(t, domain.DelegationStatusExpired, e.Status)
		require.NotNil(t, e.ExpiredAt)
		require.WithinDuration(t, to, *e.ExpiredAt, time.Second)
	}
	require.True(t, ids[a.DelegationID], "tenant A's due grant must be expired by the background sweep")
	require.True(t, ids[b.DelegationID], "tenant B's due grant must be expired without anyone reading tenant B")

	// Each tenant's event lands under its OWN tenant_id: the sweep crosses
	// tenants to find the rows, and must not blur them on the way out.
	require.Equal(t, []string{"authority.delegated", "authority.expired"},
		outboxEvents(t, pool, testTenantA, a.DelegationID))
	require.Equal(t, []string{"authority.delegated", "authority.expired"},
		outboxEvents(t, pool, testTenantB, b.DelegationID))
}

// The batch bound exists so a long-unswept register does not take one lock
// across the whole table. The caller loops while a pass comes back full.
func TestExpireDueAllTenantsRespectsBatchLimit(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)
	ctx := tenantCtx(testTenantA)

	now := time.Now().UTC()
	from, to := now.Add(-48*time.Hour), now.Add(-24*time.Hour)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa04%02d", i)
		g := grant(id, fmt.Sprintf("corr-04%02d", i), testEntityA, principalSelf, principalOther, from, to)
		_, err := s.CreateDelegation(ctx, g)
		require.NoError(t, err)
	}

	first, err := s.ExpireDueAllTenants(context.Background(), 2)
	require.NoError(t, err)
	require.Len(t, first, 2, "a pass must not exceed its batch limit")

	rest, err := s.ExpireDueAllTenants(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, rest, 3, "the remainder is taken by the next pass")

	empty, err := s.ExpireDueAllTenants(context.Background(), 100)
	require.NoError(t, err)
	require.Empty(t, empty, "expiry is observed once per grant, not once per pass")
}

// DueCount is what the backlog gauges read. It has to see across tenants for
// the same reason the sweep does, and it must not count what it already swept.
func TestDueCountSeesEveryTenantAndClearsAfterSweep(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool)

	now := time.Now().UTC()
	overdueBy := 36 * time.Hour
	from, to := now.Add(-96*time.Hour), now.Add(-overdueBy)

	_, err := s.CreateDelegation(tenantCtx(testTenantA),
		grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa0501", "corr-0501", testEntityA, principalSelf, principalOther, from, to))
	require.NoError(t, err)
	_, err = s.CreateDelegation(tenantCtx(testTenantB),
		grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa0502", "corr-0502", testEntityB, principalSelf, principalOther, from, to))
	require.NoError(t, err)
	// In window: must not be counted as due.
	_, err = s.CreateDelegation(tenantCtx(testTenantA),
		grant("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaa0503", "corr-0503", testEntityA, principalSelf, principalOther, now, now.Add(24*time.Hour)))
	require.NoError(t, err)

	due, oldest, err := s.DueCount(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), due, "both tenants' overdue grants are counted; the in-window one is not")
	require.InDelta(t, overdueBy.Seconds(), oldest.Seconds(), 120,
		"oldest overdue is measured from effective_to")

	_, err = s.ExpireDueAllTenants(context.Background(), 100)
	require.NoError(t, err)

	due, _, err = s.DueCount(context.Background())
	require.NoError(t, err)
	require.Zero(t, due, "a swept register has no backlog")
}
