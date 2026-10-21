package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/employee-master-svc/internal/domain"
	"zoiko.io/employee-master-svc/internal/middleware"
	"zoiko.io/employee-master-svc/internal/store"
)

// Real-Postgres tests (TEST_DATABASE_URL; set by the integration TestMain). The
// embedded superuser bypasses RLS, so tenant isolation here proves the explicit
// tenant_id predicate; the database guards are exercised with direct SQL.

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)
	_, filename, _, _ := runtime.Caller(0)
	// Every *.up.sql by glob, not a hardcoded list: a named list quietly stops
	// applying migrations the moment one is added.
	migrations, err := filepath.Glob(filepath.Join(filepath.Dir(filename), "../../deployments/migrations", "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, migrations, "no *.up.sql found - these tests would run against an empty schema")
	sort.Strings(migrations)
	for _, m := range migrations {
		sql, err := os.ReadFile(m)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sql))
		require.NoError(t, err, filepath.Base(m))
	}
	return pool
}

func tenantCtx() (string, context.Context) {
	id := uuid.NewString()
	return id, middleware.WithTenant(context.Background(), id)
}

func addEmployee(t *testing.T, pool *pgxpool.Pool, tenant, entity, status string, termination *string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO employees (employee_id, tenant_id, legal_entity_id, employee_number, first_name, last_name, email,
			worker_type, status, hire_date, termination_date, effective_from, created_at, updated_at)
		VALUES ($1,$2,$3,$4,'A','B',$5,'FULL_TIME',$6,'2020-01-01',$7::date,now(),now(),now())`,
		id, tenant, entity, "E-"+id[:8], id[:8]+"@example.test", status, termination)
	require.NoError(t, err)
	return id
}

type profileOpts struct {
	assignment, source, from string
	to, payee                *string
	version                  int
	jurisdiction, currency   string
	entity                   string
}

func newProfile(tenant, worker string, o profileOpts) *domain.WorkerFinancialProfile {
	if o.assignment == "" {
		o.assignment = "A1"
	}
	if o.source == "" {
		o.source = "hris"
	}
	if o.version == 0 {
		o.version = 1
	}
	if o.from == "" {
		o.from = "2026-01-01"
	}
	if o.jurisdiction == "" {
		o.jurisdiction = "GB"
	}
	if o.currency == "" {
		o.currency = "GBP"
	}
	if o.entity == "" {
		o.entity = "le-1"
	}
	req := domain.SyncWorkerFinancialProfileRequest{
		WorkerID: worker, AssignmentID: o.assignment, LegalEntityID: o.entity, SourceSystemID: o.source,
		SourceVersion: o.version, PayrollJurisdictionID: o.jurisdiction, PayCurrency: o.currency,
		EmploymentClassification: "EMPLOYEE", PayeeRef: o.payee, EffectiveFrom: o.from, EffectiveTo: o.to,
	}
	return &domain.WorkerFinancialProfile{
		ProfileID: uuid.NewString(), WorkerID: worker, AssignmentID: o.assignment, LegalEntityID: o.entity,
		PayrollJurisdictionID: o.jurisdiction, PayCurrency: o.currency, EmploymentClassification: "EMPLOYEE",
		PayeeRef: o.payee, EffectiveFrom: o.from, EffectiveTo: o.to, SourceSystemID: o.source, SourceVersion: o.version,
		SourceFingerprint: req.Fingerprint(tenant), CorrelationID: "corr", CreatedBy: "p-1",
	}
}

func events(tenant string) store.EventBuilder {
	return func(p *domain.WorkerFinancialProfile, sup []domain.WorkerFinancialProfile) ([]domain.OutboxEvent, error) {
		typ := domain.EventProfileSynced
		switch p.State {
		case domain.ProfileQuarantined:
			typ = domain.EventProfileRejected
		case domain.ProfileDeactivated:
			typ = domain.EventProfileDeactivated
		}
		out := []domain.OutboxEvent{{TenantID: tenant, ObjectID: p.WorkerID, EventType: typ, Payload: []byte(`{"k":"` + typ + `"}`)}}
		for range sup {
			out = append(out, domain.OutboxEvent{TenantID: tenant, ObjectID: p.WorkerID, EventType: domain.EventProfileSuperseded, Payload: []byte(`{}`)})
		}
		return out, nil
	}
}

func outboxTypes(t *testing.T, pool *pgxpool.Pool, tenant string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT event_type FROM wfp_outbox WHERE tenant_id = $1 ORDER BY outbox_id`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func sp(s string) *string { return &s }

func TestWFP01_SyncReplayStaleAndConflict(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tenantCtx()
	worker := addEmployee(t, pool, tenant, "le-1", "ACTIVE", nil)

	res, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 3}), "", events(tenant))
	require.NoError(t, err)
	assert.True(t, res.Created)
	assert.Equal(t, domain.ProfileActive, res.Profile.State)
	assert.Equal(t, domain.PayeeNone, res.Profile.PayeeRefStatus)

	// Same source version redelivered unchanged: same row, no new event.
	again, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 3}), "", events(tenant))
	require.NoError(t, err)
	assert.False(t, again.Created)
	assert.Equal(t, res.Profile.ProfileID, again.Profile.ProfileID)
	assert.Equal(t, []string{domain.EventProfileSynced}, outboxTypes(t, pool, tenant))

	// Same version, changed content: refused, current state untouched.
	_, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 3, currency: "EUR"}), "", events(tenant))
	assert.ErrorIs(t, err, domain.ErrSourceVersionConflict)

	// An out-of-order older version is rejected and current state is preserved.
	_, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 2, currency: "EUR"}), "", events(tenant))
	assert.ErrorIs(t, err, domain.ErrStaleSourceVersion)
	got, err := s.GetWorkerFinancialProfile(ctx, res.Profile.ProfileID)
	require.NoError(t, err)
	assert.Equal(t, "GBP", got.PayCurrency)
	assert.Equal(t, domain.ProfileActive, got.State)

	// A different source system has its own version line.
	other, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 1, source: "payroll-x", assignment: "A2"}), "", events(tenant))
	require.NoError(t, err)
	assert.True(t, other.Created)
}

func TestWFP01_NewVersionSupersedesAndEndDates(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tenantCtx()
	worker := addEmployee(t, pool, tenant, "le-1", "ACTIVE", nil)

	v1, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 1, from: "2026-01-01", payee: sp("payee-A")}), "", events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.PayeeUnverified, v1.Profile.PayeeRefStatus)

	// Later effective date: v1 stays valid for earlier dates (end-dated), not superseded.
	v2, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 2, from: "2026-07-01", payee: sp("payee-B")}), "", events(tenant))
	require.NoError(t, err)
	assert.Empty(t, v2.Superseded)
	assert.True(t, v2.Profile.PayeeRefChanged, "a changed payee reference is flagged")
	require.NotNil(t, v2.Profile.SupersedesProfileID)
	assert.Equal(t, v1.Profile.ProfileID, *v2.Profile.SupersedesProfileID)

	first, err := s.GetWorkerFinancialProfile(ctx, v1.Profile.ProfileID)
	require.NoError(t, err)
	require.NotNil(t, first.EffectiveTo)
	assert.Equal(t, "2026-07-01", *first.EffectiveTo)

	// As-of reconstruction: each date resolves to the profile in force then.
	at := func(d string) *domain.WorkerFinancialProfile {
		ps, err := s.GetAsOfProfiles(ctx, worker, "A1", d)
		require.NoError(t, err)
		require.Len(t, ps, 1, d)
		return &ps[0]
	}
	assert.Equal(t, v1.Profile.ProfileID, at("2026-06-30").ProfileID)
	assert.Equal(t, v2.Profile.ProfileID, at("2026-07-01").ProfileID)
	ps, err := s.GetAsOfProfiles(ctx, worker, "A1", "2025-12-31")
	require.NoError(t, err)
	assert.Empty(t, ps, "no profile before the first effective date")

	// Same payee on the next version: not flagged.
	v3, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 3, from: "2026-09-01", payee: sp("payee-B")}), "", events(tenant))
	require.NoError(t, err)
	assert.False(t, v3.Profile.PayeeRefChanged)

	// A correction effective at/before an existing start fully supersedes what it covers.
	v4, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 4, from: "2026-07-01", payee: sp("payee-B")}), "", events(tenant))
	require.NoError(t, err)
	assert.Len(t, v4.Superseded, 2, "v2 (starts the same day) and v3 (starts later) are replaced")
	vers, err := s.ListProfileVersions(ctx, worker, "A1")
	require.NoError(t, err)
	states := map[int]string{}
	for _, p := range vers {
		states[p.SourceVersion] = p.State
	}
	assert.Equal(t, map[int]string{1: "ACTIVE", 2: "SUPERSEDED", 3: "SUPERSEDED", 4: "ACTIVE"}, states)
	assert.Contains(t, outboxTypes(t, pool, tenant), domain.EventProfileSuperseded)

	// A bounded window that would split a longer-lived profile is refused.
	_, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 5, from: "2026-08-01", to: sp("2026-09-01")}), "", events(tenant))
	assert.ErrorIs(t, err, domain.ErrPartialOverlap)
}

func TestWFP01_OverlapIsImpossibleAtTheDatabase(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tenantCtx()
	worker := addEmployee(t, pool, tenant, "le-1", "ACTIVE", nil)
	_, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{version: 1}), "", nil)
	require.NoError(t, err)

	// Bypass the store: a second ACTIVE profile over the same dates must still fail (WFP-CTRL-002).
	_, err = pool.Exec(context.Background(), `
		INSERT INTO wfp_worker_financial_profiles (profile_id, tenant_id, worker_id, assignment_id, legal_entity_id,
			payroll_jurisdiction_id, pay_currency, employment_classification, effective_from, source_system_id,
			source_version, source_fingerprint, state, correlation_id, created_by)
		VALUES (gen_random_uuid(), $1, $2, 'A1', 'le-1', 'GB', 'GBP', 'EMPLOYEE', '2026-03-01', 'hris', 99, 'x', 'ACTIVE', 'c', 'p')`,
		tenant, worker)
	assert.Error(t, err)
}

func TestWFP01_ContentIsImmutableAndNeverDeleted(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tenantCtx()
	worker := addEmployee(t, pool, tenant, "le-1", "ACTIVE", nil)
	res, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{}), "", nil)
	require.NoError(t, err)
	id := res.Profile.ProfileID

	for _, stmt := range []string{
		`UPDATE wfp_worker_financial_profiles SET pay_currency = 'USD' WHERE profile_id = $1`,
		`UPDATE wfp_worker_financial_profiles SET payee_ref = 'attacker' , payee_ref_status = 'UNVERIFIED' WHERE profile_id = $1`,
		`UPDATE wfp_worker_financial_profiles SET legal_entity_id = 'le-2' WHERE profile_id = $1`,
		`DELETE FROM wfp_worker_financial_profiles WHERE profile_id = $1`,
	} {
		_, err := pool.Exec(context.Background(), stmt, id)
		assert.Error(t, err, stmt)
	}
	_, err = pool.Exec(context.Background(), `UPDATE wfp_worker_financial_profiles SET pay_currency = 'usd' WHERE profile_id = $1`, id)
	assert.Error(t, err, "currency format is a CHECK")
}

func TestWFP01_MappingFailuresQuarantineNeverDefault(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tenantCtx()

	// Unknown worker.
	ghost := uuid.NewString()
	r, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, ghost, profileOpts{}), "", events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.ProfileQuarantined, r.Profile.State)
	assert.Equal(t, domain.ReasonWorkerNotFound, *r.Profile.QuarantineReason)

	// Worker belongs to another legal entity: cross-entity context is blocked.
	other := addEmployee(t, pool, tenant, "le-OTHER", "ACTIVE", nil)
	r, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, other, profileOpts{}), "", events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.ReasonCrossEntityWorker, *r.Profile.QuarantineReason)

	// Terminated worker: a profile effective after termination is blocked;
	// one effective on/before it (final pay) is accepted.
	term := addEmployee(t, pool, tenant, "le-1", "TERMINATED", sp("2026-06-30"))
	r, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, term, profileOpts{from: "2026-07-01"}), "", events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.ReasonInactiveWorker, *r.Profile.QuarantineReason)
	r, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, term, profileOpts{version: 2, from: "2026-01-01"}), "", events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.ProfileActive, r.Profile.State)

	// A caller-side reason (unsupported jurisdiction) quarantines too.
	ok := addEmployee(t, pool, tenant, "le-1", "ACTIVE", nil)
	r, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, ok, profileOpts{jurisdiction: "ZZ"}), domain.ReasonUnsupportedJurisdiction, events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.ReasonUnsupportedJurisdiction, *r.Profile.QuarantineReason)

	// Quarantined profiles answer no as-of query and supersede nothing.
	ps, err := s.GetAsOfProfiles(ctx, ok, "A1", "2026-06-01")
	require.NoError(t, err)
	assert.Empty(t, ps)

	// Redelivery of the quarantined delivery is a replay of the same row.
	again, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, ok, profileOpts{jurisdiction: "ZZ"}), domain.ReasonUnsupportedJurisdiction, nil)
	require.NoError(t, err)
	assert.False(t, again.Created)
	assert.Equal(t, domain.ProfileQuarantined, again.Profile.State)
}

func TestWFP01_RevalidateActivatesOnceMappingIsFixed(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tenantCtx()

	worker := uuid.NewString()
	q, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{}), "", events(tenant))
	require.NoError(t, err)
	require.Equal(t, domain.ProfileQuarantined, q.Profile.State)

	// Still missing: stays quarantined, version moves, no activation event.
	still, err := s.RevalidateWorkerFinancialProfile(ctx, q.Profile.ProfileID, q.Profile.Version, "", events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.ProfileQuarantined, still.Profile.State)
	assert.Equal(t, 2, still.Profile.Version)

	// Stale token refused.
	_, err = s.RevalidateWorkerFinancialProfile(ctx, q.Profile.ProfileID, 1, "", nil)
	assert.ErrorIs(t, err, domain.ErrProfileVersionConflict)

	// The worker appears (the missing HR record is created): revalidation activates it.
	_, err = pool.Exec(context.Background(), `
		INSERT INTO employees (employee_id, tenant_id, legal_entity_id, employee_number, first_name, last_name, email,
			worker_type, status, hire_date, effective_from, created_at, updated_at)
		VALUES ($1,$2,'le-1','E-late','A','B','late@example.test','FULL_TIME','ACTIVE','2020-01-01',now(),now(),now())`, worker, tenant)
	require.NoError(t, err)
	done, err := s.RevalidateWorkerFinancialProfile(ctx, q.Profile.ProfileID, still.Profile.Version, "", events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.ProfileActive, done.Profile.State)
	assert.Nil(t, done.Profile.QuarantineReason)

	// Only quarantined profiles can be revalidated.
	_, err = s.RevalidateWorkerFinancialProfile(ctx, q.Profile.ProfileID, done.Profile.Version, "", nil)
	assert.ErrorIs(t, err, domain.ErrProfileNotQuarantined)
}

func TestWFP01_Deactivate(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tenantCtx()
	worker := addEmployee(t, pool, tenant, "le-1", "ACTIVE", nil)
	res, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{}), "", events(tenant))
	require.NoError(t, err)

	_, err = s.DeactivateWorkerFinancialProfile(ctx, res.Profile.ProfileID, 9, events(tenant))
	assert.ErrorIs(t, err, domain.ErrProfileVersionConflict)
	d, err := s.DeactivateWorkerFinancialProfile(ctx, res.Profile.ProfileID, res.Profile.Version, events(tenant))
	require.NoError(t, err)
	assert.Equal(t, domain.ProfileDeactivated, d.State)
	_, err = s.DeactivateWorkerFinancialProfile(ctx, res.Profile.ProfileID, d.Version, events(tenant))
	assert.ErrorIs(t, err, domain.ErrProfileNotActive)

	ps, err := s.GetAsOfProfiles(ctx, worker, "A1", "2026-06-01")
	require.NoError(t, err)
	assert.Empty(t, ps, "a deactivated profile no longer answers as-of")
	// DEACTIVATED is terminal at the database as well.
	_, err = pool.Exec(context.Background(), `UPDATE wfp_worker_financial_profiles SET state = 'ACTIVE' WHERE profile_id = $1`, res.Profile.ProfileID)
	assert.Error(t, err)
}

func TestWFP01_TenantIsolationAndMissingTenant(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenantA, ctxA := tenantCtx()
	_, ctxB := tenantCtx()
	worker := addEmployee(t, pool, tenantA, "le-1", "ACTIVE", nil)
	res, err := s.SyncWorkerFinancialProfile(ctxA, newProfile(tenantA, worker, profileOpts{}), "", nil)
	require.NoError(t, err)

	_, err = s.GetWorkerFinancialProfile(ctxB, res.Profile.ProfileID)
	assert.ErrorIs(t, err, domain.ErrProfileNotFound)
	ps, err := s.ListProfileVersions(ctxB, worker, "")
	require.NoError(t, err)
	assert.Empty(t, ps)
	ps, err = s.GetAsOfProfiles(ctxB, worker, "A1", "2026-06-01")
	require.NoError(t, err)
	assert.Empty(t, ps)
	facts, err := s.WorkerFactsFor(ctxB, worker)
	require.NoError(t, err)
	assert.False(t, facts.Found, "another tenant's worker is invisible")
	_, err = s.DeactivateWorkerFinancialProfile(ctxB, res.Profile.ProfileID, 1, nil)
	assert.ErrorIs(t, err, domain.ErrProfileNotFound)

	_, err = s.GetWorkerFinancialProfile(context.Background(), res.Profile.ProfileID)
	assert.ErrorIs(t, err, domain.ErrIdentityMissing)
}

func TestWFP01_OutboxIsAtomicWithStateChange(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, "")
	tenant, ctx := tenantCtx()
	worker := addEmployee(t, pool, tenant, "le-1", "ACTIVE", nil)

	// A failing event builder rolls the whole change back: no profile without its event.
	_, err := s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{}), "",
		func(*domain.WorkerFinancialProfile, []domain.WorkerFinancialProfile) ([]domain.OutboxEvent, error) {
			return nil, errors.New("boom")
		})
	require.Error(t, err)
	vers, err := s.ListProfileVersions(ctx, worker, "")
	require.NoError(t, err)
	assert.Empty(t, vers)

	// An event outside the catalogue is refused and rolls back too.
	_, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{}), "",
		func(p *domain.WorkerFinancialProfile, _ []domain.WorkerFinancialProfile) ([]domain.OutboxEvent, error) {
			return []domain.OutboxEvent{{TenantID: tenant, ObjectID: p.WorkerID, EventType: "payroll_paid", Payload: []byte(`{}`)}}, nil
		})
	require.Error(t, err)
	vers, _ = s.ListProfileVersions(ctx, worker, "")
	assert.Empty(t, vers)

	// Committed: queued, keyed by worker (so one worker's events stay ordered), drained once.
	_, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, worker, profileOpts{}), "", events(tenant))
	require.NoError(t, err)
	var keys []string
	require.NoError(t, s.ClaimOutbox(ctx, 50, func(recs []store.OutboxRecord) error {
		for _, r := range recs {
			keys = append(keys, r.Key)
		}
		return nil
	}))
	assert.Contains(t, keys, worker)
	var unpublished int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM wfp_outbox WHERE tenant_id = $1 AND published_at IS NULL`, tenant).Scan(&unpublished))
	assert.Zero(t, unpublished)

	// A publish failure leaves it queued and counts the attempt.
	w2 := addEmployee(t, pool, tenant, "le-1", "ACTIVE", nil)
	_, err = s.SyncWorkerFinancialProfile(ctx, newProfile(tenant, w2, profileOpts{}), "", events(tenant))
	require.NoError(t, err)
	require.Error(t, s.ClaimOutbox(ctx, 50, func([]store.OutboxRecord) error { return errors.New("broker down") }))
	var attempts int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT max(attempts) FROM wfp_outbox WHERE tenant_id = $1 AND published_at IS NULL`, tenant).Scan(&attempts))
	assert.Equal(t, 1, attempts)
}
