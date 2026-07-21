//go:build integration

// Package store_test contains cross-tenant isolation tests for PgStore.
//
// These tests exercise EVERY method that was identified as vulnerable to the
// superuser RLS bypass described in the fix commit for general-ledger-svc:
//
//	Services connect as the Postgres superuser (DB_USER=postgres). Postgres
//	superusers unconditionally bypass Row-Level Security regardless of policy
//	text — set_config('app.tenant_id', …) has no effect because RLS never runs.
//	The only real isolation guarantee is an explicit AND tenant_id = $N in every
//	query's WHERE clause.
//
// Each subtest:
//  1. Creates two independent tenants (A and B) with realistic data.
//  2. Executes the method under test with TENANT B's context but TENANT A's IDs.
//  3. Asserts no cross-tenant data is returned (nil / empty / zero rows affected).
//  4. Asserts tenant B can still read its own data (the fix must not over-restrict).
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=180s ./internal/store/
package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	// Start embedded Postgres
	dbPort := uint32(15701 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.PostgresVersion("16.0.0")).
			Port(dbPort).
			Database("ter_isolation_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=ter_isolation_test user=postgres password=postgres sslmode=disable",
		dbPort,
	)

	ctx := context.Background()
	var err error
	testPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Printf("failed to connect to postgres: %v\n", err)
		_ = pg.Stop()
		os.Exit(1)
	}

	// Wait for pool to be ready
	for i := 0; i < 75; i++ {
		if err = testPool.Ping(ctx); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		fmt.Printf("postgres did not become ready: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}

	// Run migrations
	migrations := []string{
		"000001_initial_schema.up.sql",
		"000002_add_tenant_id_to_junction_tables.up.sql",
		"000003_add_residency_region_to_policies.up.sql",
		"000004_add_data_classification.up.sql",
	}
	for _, mig := range migrations {
		sql, err := os.ReadFile("../../deployments/migrations/" + mig)
		if err != nil {
			fmt.Printf("failed to read migration %s: %v\n", mig, err)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
		if _, err = testPool.Exec(ctx, string(sql)); err != nil {
			fmt.Printf("failed to apply migration %s: %v\n", mig, err)
			testPool.Close()
			_ = pg.Stop()
			os.Exit(1)
		}
	}

	testStore = store.New(testPool, zap.NewNop())

	code := m.Run()

	testPool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

// tenantFixture holds the key IDs for a created tenant + its entities.
type tenantFixture struct {
	tenantID     string
	policyID     string
	entityID     string
	hierarchyID  string
	assignmentID string
	bundleID     string
}

// setupIsolationFixture creates a fully-wired tenant fixture for isolation testing.
// Tenant code must be unique across the test run.
func setupIsolationFixture(t *testing.T, s *store.PgStore, tenantCode string) tenantFixture {
	t.Helper()
	ctx := context.Background()

	f := tenantFixture{
		tenantID:     uuid.New().String(),
		policyID:     uuid.New().String(),
		entityID:     uuid.New().String(),
		hierarchyID:  uuid.New().String(),
		assignmentID: uuid.New().String(),
		bundleID:     uuid.New().String(),
	}
	tctx := domain.WithTenant(ctx, f.tenantID)

	// Create tenant (no FK on default_data_residency_policy_id in this schema).
	require.NoError(t, s.CreateTenant(tctx, &domain.Tenant{
		TenantID: f.tenantID, TenantCode: tenantCode, LegalName: tenantCode,
		Status: domain.TenantStatusActive, DefaultCurrencyCode: "USD",
		PrimaryTimezone: "UTC", PrimaryLocale: "en-US",
		DefaultDataResidencyPolicyID: f.policyID,
		LifecycleState:               domain.TenantLifecycleOnboarding,
		CreatedByPrincipalID:         "test",
	}))

	// Create residency policy.
	require.NoError(t, s.CreateResidencyPolicy(tctx, &domain.DataResidencyPolicy{
		DataResidencyPolicyID: f.policyID, TenantID: f.tenantID,
		PolicyName: "Default", PolicyCode: tenantCode + "-POLICY",
		ResidencyMode:          domain.ResidencyModeFollowEntity,
		ConflictResolutionMode: domain.ConflictResolutionFailClosed,
		ActiveFlag:             true, CreatedByPrincipalID: "test",
	}))

	// Create entity.
	require.NoError(t, s.CreateEntity(tctx, &domain.LegalEntity{
		LegalEntityID: f.entityID, TenantID: f.tenantID,
		EntityCode: tenantCode + "-E1", LegalName: tenantCode + " Entity",
		EntityType: domain.EntityTypeSubsidiary, DefaultCurrencyCode: "USD",
		FiscalCalendarID:      uuid.New().String(),
		EntityStatus:          domain.EntityStatusActive,
		PrimaryJurisdictionID: uuid.New().String(),
		DataResidencyPolicyID: f.policyID,
		CreatedByPrincipalID:  "test",
	}))

	// Create entity hierarchy (parent = entity itself for simplicity).
	require.NoError(t, s.CreateHierarchy(tctx, &domain.EntityHierarchy{
		HierarchyID: f.hierarchyID, TenantID: f.tenantID,
		ParentLegalEntityID: f.entityID, ChildLegalEntityID: f.entityID,
		RelationshipType:     domain.HierarchyRelationshipOwnership,
		EffectiveFrom:        time.Now().UTC(),
		CreatedByPrincipalID: "test",
	}))

	// Create jurisdiction assignment.
	require.NoError(t, s.CreateJurisdictionAssignment(tctx, &domain.EntityJurisdictionAssignment{
		AssignmentID: f.assignmentID, TenantID: f.tenantID,
		LegalEntityID: f.entityID, JurisdictionID: uuid.New().String(),
		AssignmentType:       domain.JurisdictionAssignmentPrimary,
		EffectiveFrom:        time.Now().UTC(),
		SourceBasis:          "test",
		CreatedByPrincipalID: "test",
	}))

	// Create tax identity bundle (classified RESTRICTED — highest-risk data).
	require.NoError(t, s.CreateTaxIdentityBundle(tctx, &domain.TaxIdentityBundle{
		TaxIdentityBundleID: f.bundleID, TenantID: f.tenantID,
		LegalEntityID:        f.entityID,
		JurisdictionID:       uuid.New().String(),
		Status:               domain.TaxIdentityBundleActive,
		EffectiveFrom:        time.Now().UTC(),
		DataClassification:   "RESTRICTED",
		CreatedByPrincipalID: "test",
	}))

	return f
}

// TestPgStore_TenantIsolation_GetEntityByID proves the cross-tenant leak
// and verifies the fix. Tenant B's context MUST NOT read tenant A's entity.
func TestPgStore_TenantIsolation_GetEntityByID(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-GetEntity")
	b := setupIsolationFixture(t, s, "ISO-B-GetEntity")

	// Probe: tenant B's context, tenant A's entity UUID.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	got, err := s.GetEntityByID(ctxB, a.entityID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetEntityByID returned tenant A's data in tenant B's context")

	// Sanity: tenant B can still read its own entity.
	gotOwn, err := s.GetEntityByID(ctxB, b.entityID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn, "tenant B should be able to read its own entity")
	assert.Equal(t, b.entityID, gotOwn.LegalEntityID)
}

func TestPgStore_TenantIsolation_GetEntityStatus(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-GetStatus")
	b := setupIsolationFixture(t, s, "ISO-B-GetStatus")

	ctxB := domain.WithTenant(ctx, b.tenantID)
	got, err := s.GetEntityStatus(ctxB, a.entityID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetEntityStatus leaked tenant A's data into tenant B's context")

	gotOwn, err := s.GetEntityStatus(ctxB, b.entityID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.entityID, gotOwn.EntityID)
}

func TestPgStore_TenantIsolation_UpdateEntity(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-UpdateEntity")
	b := setupIsolationFixture(t, s, "ISO-B-UpdateEntity")

	// Tenant B attempts to rename tenant A's entity.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	newName := "HIJACKED NAME"
	got, err := s.UpdateEntity(ctxB, a.entityID, domain.UpdateEntityRequest{
		LegalName:        &newName,
		ActorPrincipalID: "attacker",
	})
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: UpdateEntity returned a row — tenant B mutated tenant A's entity")

	// Verify tenant A's entity is genuinely unchanged.
	ctxA := domain.WithTenant(ctx, a.tenantID)
	original, err := s.GetEntityByID(ctxA, a.entityID)
	require.NoError(t, err)
	require.NotNil(t, original)
	assert.NotEqual(t, "HIJACKED NAME", original.LegalName, "tenant A's entity was mutated by tenant B")

	// Sanity: tenant B can update its own entity.
	ownName := "Updated B Name"
	updatedOwn, err := s.UpdateEntity(ctxB, b.entityID, domain.UpdateEntityRequest{
		LegalName:        &ownName,
		ActorPrincipalID: "b-actor",
	})
	require.NoError(t, err)
	require.NotNil(t, updatedOwn)
	assert.Equal(t, ownName, updatedOwn.LegalName)
}

func TestPgStore_TenantIsolation_TransitionEntityStatus(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-TransitionStatus")
	b := setupIsolationFixture(t, s, "ISO-B-TransitionStatus")

	// Tenant B attempts to suspend tenant A's entity.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	rowsAffected, _, err := s.TransitionEntityStatus(ctxB, a.entityID,
		domain.EntityStatusSuspended,
		[]domain.EntityStatus{domain.EntityStatusActive},
		"attacker", "corr-x",
	)
	require.NoError(t, err)
	assert.Equal(t, int64(0), rowsAffected, "ISOLATION FAILURE: TransitionEntityStatus mutated tenant A's entity from tenant B's context")

	// Verify tenant A's entity status is still Active.
	ctxA := domain.WithTenant(ctx, a.tenantID)
	status, err := s.GetEntityStatus(ctxA, a.entityID)
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, domain.EntityStatusActive, status.EntityStatus, "tenant A's entity status was mutated by tenant B")
}

func TestPgStore_TenantIsolation_ListHierarchiesByEntity(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-ListHierarchy")
	b := setupIsolationFixture(t, s, "ISO-B-ListHierarchy")

	// Tenant B queries using tenant A's entity ID.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	results, err := s.ListHierarchiesByEntity(ctxB, a.entityID)
	require.NoError(t, err)
	for _, h := range results {
		assert.NotEqual(t, a.tenantID, h.TenantID, "ISOLATION FAILURE: ListHierarchiesByEntity returned tenant A's hierarchy in tenant B's context")
	}

	// Sanity: tenant B can still read its own hierarchies.
	ownResults, err := s.ListHierarchiesByEntity(ctxB, b.entityID)
	require.NoError(t, err)
	assert.NotEmpty(t, ownResults, "tenant B should see its own hierarchy")
}

func TestPgStore_TenantIsolation_EndDateHierarchy(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-EndDateHierarchy")
	b := setupIsolationFixture(t, s, "ISO-B-EndDateHierarchy")

	// Tenant B attempts to end-date tenant A's hierarchy.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	endDate := time.Now().UTC()

	// EndDateHierarchy returns no error even on 0 rows (it's idempotent).
	// The proof is in whether the row was actually modified.
	err := s.EndDateHierarchy(ctxB, a.hierarchyID, endDate, "attacker", "corr-x")
	require.NoError(t, err)

	// Verify: tenant A's hierarchy's effective_to should still be NULL.
	rows, err := s.ListHierarchiesByEntity(
		domain.WithTenant(context.Background(), a.tenantID),
		a.entityID,
	)
	require.NoError(t, err)
	for _, h := range rows {
		if h.HierarchyID == a.hierarchyID {
			assert.Nil(t, h.EffectiveTo, "ISOLATION FAILURE: tenant B end-dated tenant A's hierarchy — effective_to was set")
		}
	}
}

func TestPgStore_TenantIsolation_ListJurisdictionAssignments(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-ListJurisdiction")
	b := setupIsolationFixture(t, s, "ISO-B-ListJurisdiction")

	// Tenant B queries using tenant A's entity ID.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	results, err := s.ListJurisdictionAssignments(ctxB, a.entityID)
	require.NoError(t, err)
	for _, a := range results {
		assert.NotEqual(t, a.TenantID, b.tenantID, "sanity check failed")
		// What we really assert: none of these should be returned at all.
	}
	assert.Empty(t, results, "ISOLATION FAILURE: ListJurisdictionAssignments returned tenant A's assignments in tenant B's context")

	// Sanity: tenant B can still list its own assignments.
	ownResults, err := s.ListJurisdictionAssignments(ctxB, b.entityID)
	require.NoError(t, err)
	assert.NotEmpty(t, ownResults, "tenant B should see its own jurisdiction assignments")
}

func TestPgStore_TenantIsolation_EndDateJurisdictionAssignment(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-EndDateAssignment")
	b := setupIsolationFixture(t, s, "ISO-B-EndDateAssignment")

	ctxB := domain.WithTenant(ctx, b.tenantID)
	err := s.EndDateJurisdictionAssignment(ctxB, a.assignmentID, time.Now().UTC(), "attacker", "corr-x")
	require.NoError(t, err)

	// Verify: tenant A's assignment is still active (effective_to = NULL).
	ctxA := domain.WithTenant(context.Background(), a.tenantID)
	rows, err := s.ListJurisdictionAssignments(ctxA, a.entityID)
	require.NoError(t, err)
	for _, r := range rows {
		if r.AssignmentID == a.assignmentID {
			assert.Nil(t, r.EffectiveTo, "ISOLATION FAILURE: tenant B end-dated tenant A's jurisdiction assignment")
		}
	}
}

func TestPgStore_TenantIsolation_GetResidencyPolicyByID(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-GetPolicy")
	b := setupIsolationFixture(t, s, "ISO-B-GetPolicy")

	ctxB := domain.WithTenant(ctx, b.tenantID)
	got, err := s.GetResidencyPolicyByID(ctxB, a.policyID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetResidencyPolicyByID returned tenant A's policy in tenant B's context")

	// Sanity: tenant B can read its own policy.
	gotOwn, err := s.GetResidencyPolicyByID(ctxB, b.policyID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.policyID, gotOwn.DataResidencyPolicyID)
}

// TestPgStore_TenantIsolation_GetTaxIdentityBundleByID is the highest-risk
// case: tax_identity_bundles carries RESTRICTED-classified tax data.
func TestPgStore_TenantIsolation_GetTaxIdentityBundleByID(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-GetBundle")
	b := setupIsolationFixture(t, s, "ISO-B-GetBundle")

	ctxB := domain.WithTenant(ctx, b.tenantID)
	got, err := s.GetTaxIdentityBundleByID(ctxB, a.bundleID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetTaxIdentityBundleByID returned RESTRICTED tax data across tenant boundary")

	// Sanity: tenant B can read its own bundle.
	gotOwn, err := s.GetTaxIdentityBundleByID(ctxB, b.bundleID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.bundleID, gotOwn.TaxIdentityBundleID)
}

func TestPgStore_TenantIsolation_ListTaxIdentityBundlesByEntity(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-ListBundles")
	b := setupIsolationFixture(t, s, "ISO-B-ListBundles")

	// Tenant B probes using tenant A's entity ID.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	results, err := s.ListTaxIdentityBundlesByEntity(ctxB, a.entityID)
	require.NoError(t, err)
	assert.Empty(t, results, "ISOLATION FAILURE: ListTaxIdentityBundlesByEntity returned RESTRICTED data across tenant boundary")

	// Sanity: tenant B can list its own bundles.
	ownResults, err := s.ListTaxIdentityBundlesByEntity(ctxB, b.entityID)
	require.NoError(t, err)
	assert.NotEmpty(t, ownResults, "tenant B should see its own tax bundles")
}

func TestPgStore_TenantIsolation_TransitionTaxIdentityBundleStatus(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-TransitionBundle")
	b := setupIsolationFixture(t, s, "ISO-B-TransitionBundle")

	// Tenant B attempts to revoke tenant A's tax identity bundle.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	err := s.TransitionTaxIdentityBundleStatus(ctxB, a.bundleID, domain.TaxIdentityBundleExpired, "attacker", "corr-x")
	require.NoError(t, err)

	// Verify: tenant A's bundle status is still Active.
	ctxA := domain.WithTenant(context.Background(), a.tenantID)
	bundle, err := s.GetTaxIdentityBundleByID(ctxA, a.bundleID)
	require.NoError(t, err)
	require.NotNil(t, bundle)
	assert.Equal(t, domain.TaxIdentityBundleActive, bundle.Status,
		"ISOLATION FAILURE: tenant B expired tenant A's tax identity bundle")
}

func TestPgStore_TenantIsolation_GetTenantByID(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-GetTenant")
	b := setupIsolationFixture(t, s, "ISO-B-GetTenant")

	// Probe: tenant B's context, tenant A's tenant ID.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	got, err := s.GetTenantByID(ctxB, a.tenantID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetTenantByID returned Tenant A's row or Tenant B's own row in mismatch context")

	// Sanity: tenant B can still read its own tenant.
	gotOwn, err := s.GetTenantByID(ctxB, b.tenantID)
	require.NoError(t, err)
	require.NotNil(t, gotOwn)
	assert.Equal(t, b.tenantID, gotOwn.TenantID)
}

func TestPgStore_TenantIsolation_TransitionTenantLifecycle(t *testing.T) {
	s := testStore
	ctx := context.Background()

	a := setupIsolationFixture(t, s, "ISO-A-TransitionTenant")
	b := setupIsolationFixture(t, s, "ISO-B-TransitionTenant")

	// Tenant B attempts to transition Tenant A's lifecycle state.
	ctxB := domain.WithTenant(ctx, b.tenantID)
	err := s.TransitionTenantLifecycle(ctxB, a.tenantID, domain.TenantLifecycleActive, "attacker", "corr-x")
	require.NoError(t, err)

	// Verify Tenant A's state remains ONBOARDING.
	ctxA := domain.WithTenant(ctx, a.tenantID)
	tenant, err := s.GetTenantByID(ctxA, a.tenantID)
	require.NoError(t, err)
	require.NotNil(t, tenant)
	assert.Equal(t, domain.TenantLifecycleOnboarding, tenant.LifecycleState, "ISOLATION FAILURE: tenant B transitioned tenant A's lifecycle state")
}

