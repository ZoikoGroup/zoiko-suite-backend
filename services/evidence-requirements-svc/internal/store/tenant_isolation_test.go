//go:build integration

// Package store_test contains cross-tenant isolation tests for PgStore, plus
// one regression test for the RLS statement form itself.
//
// Why the second thing matters as much as the first: Phase 5 shipped ten
// consecutive services with zero store-layer tests, and every one of them set
// tenant scope with `SET LOCAL app.tenant_id = $1`. Postgres does not accept
// bind parameters in SET, so that statement raises a syntax error and — because
// the error is returned and checked — every DB write in those services aborts.
// Nothing caught it because nothing ever executed their store layer against a
// real database. TestSetConfig_RLSStatementForm below is the test that would
// have.
//
// The isolation tests cover the second hazard: this platform connects as a
// Postgres superuser, which unconditionally bypasses Row-Level Security
// regardless of policy. RLS is defense-in-depth; the explicit
// `AND tenant_id = $N` in every query is the actual guarantee. general-ledger-svc
// and tenant-entity-registry-svc both learned this through real CI failures.
//
// Run:
//
//	go test -v -tags=integration -count=1 -timeout=120s ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
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

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
	"zoiko.io/evidence-requirements-svc/internal/store"
)

var (
	testPool  *pgxpool.Pool
	testStore *store.PgStore
)

func TestMain(m *testing.M) {
	dbPort := uint32(15901 + uint32(os.Getpid()%499))
	pg := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Port(dbPort).
			Database("evreq_isolation_test").
			Username("postgres").
			Password("postgres"),
	)
	if err := pg.Start(); err != nil {
		fmt.Printf("failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := fmt.Sprintf(
		"host=localhost port=%d dbname=evreq_isolation_test user=postgres password=postgres sslmode=disable",
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

	sql, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	if err != nil {
		fmt.Printf("failed to read migration: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}
	if _, err = testPool.Exec(ctx, string(sql)); err != nil {
		fmt.Printf("failed to apply migration: %v\n", err)
		testPool.Close()
		_ = pg.Stop()
		os.Exit(1)
	}

	testStore = store.New(testPool, zap.NewNop())

	code := m.Run()

	testPool.Close()
	_ = pg.Stop()
	os.Exit(code)
}

// TestSetConfig_RLSStatementForm pins the statement form PgStore.withRLS uses.
//
// This is the regression test twelve services on this platform are missing.
// It proves two things against a real Postgres:
//
//  1. `SELECT set_config('app.tenant_id', $1, true)` accepts a bind parameter
//     and works — this is the correct form.
//  2. `SET LOCAL app.tenant_id = $1` does NOT — it is a syntax error, not a
//     harmless no-op. If someone "simplifies" withRLS to that form, this test
//     fails loudly instead of the whole service silently losing its ability to
//     write.
func TestSetConfig_RLSStatementForm(t *testing.T) {
	ctx := context.Background()
	tenantID := uuid.New().String()

	tx, err := testPool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	require.NoError(t, err, "the form PgStore.withRLS uses must work against real Postgres")

	var got string
	require.NoError(t, tx.QueryRow(ctx, "SELECT current_setting('app.tenant_id')").Scan(&got))
	assert.Equal(t, tenantID, got, "tenant scope must actually be set, not silently ignored")

	// And the form twelve sibling services ship, which does not work.
	_, err = tx.Exec(ctx, "SET LOCAL app.tenant_id = $1", tenantID)
	assert.Error(t, err,
		"SET does not accept bind parameters in Postgres — if this ever passes, revisit withRLS")
}

// requirementFixture holds the key IDs for one tenant's seeded requirement.
type requirementFixture struct {
	tenantID      string
	entityID      string
	requirementID string
}

func setupFixture(t *testing.T, label string) requirementFixture {
	t.Helper()
	ctx := context.Background()

	f := requirementFixture{
		tenantID:      uuid.New().String(),
		entityID:      uuid.New().String(),
		requirementID: uuid.New().String(),
	}
	tctx := svcmiddleware.WithTenant(ctx, f.tenantID)

	spec, err := json.Marshal(domain.RequirementSpec{MinimumCount: 1, Description: "seeded by " + label})
	require.NoError(t, err)

	r := &domain.EvidenceRequirement{
		EvidenceRequirementID: f.requirementID,
		TenantID:              f.tenantID,
		LegalEntityID:         &f.entityID,
		DomainCode:            "FINANCE",
		ActionType:            "JOURNAL_POST",
		EvidenceType:          "SUPPORTING_DOCUMENT",
		RequirementPayload:    spec,
		EffectiveFrom:         time.Now().UTC().Add(-time.Hour),
		CreatedByPrincipalID:  "test-" + label,
		CorrelationID:         "corr-" + label,
	}
	created, err := testStore.CreateRequirement(tctx, r)
	require.NoError(t, err)
	require.True(t, created)

	return f
}

func TestPgStore_TenantIsolation_GetRequirement(t *testing.T) {
	a := setupFixture(t, "A-Get")
	b := setupFixture(t, "B-Get")

	// Probe: tenant B's context, tenant A's requirement ID.
	ctxB := svcmiddleware.WithTenant(context.Background(), b.tenantID)
	got, err := testStore.GetRequirement(ctxB, a.requirementID)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetRequirement returned Tenant A's row under Tenant B's context")

	// Sanity: tenant B can still read its own.
	own, err := testStore.GetRequirement(ctxB, b.requirementID)
	require.NoError(t, err)
	require.NotNil(t, own)
	assert.Equal(t, b.requirementID, own.EvidenceRequirementID)
}

func TestPgStore_TenantIsolation_EndDateRequirement(t *testing.T) {
	a := setupFixture(t, "A-EndDate")
	b := setupFixture(t, "B-EndDate")

	// Tenant B attempts to retire tenant A's requirement, passing its own
	// tenantID as the scope — what a handler bug would look like if tenant
	// scope came from the request body instead of the verified header.
	_, err := testStore.EndDateRequirement(context.Background(), b.tenantID, a.requirementID,
		time.Now().UTC(), "attacker retire", "attacker")
	assert.ErrorIs(t, err, domain.ErrRequirementNotFound,
		"ISOLATION FAILURE: tenant B was able to reach tenant A's requirement")

	// Tenant A's requirement must still be in force.
	ctxA := svcmiddleware.WithTenant(context.Background(), a.tenantID)
	got, err := testStore.GetRequirement(ctxA, a.requirementID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.EffectiveTo,
		"ISOLATION FAILURE: tenant A's requirement was retired by tenant B")

	// Sanity: tenant B can retire its OWN.
	updated, err := testStore.EndDateRequirement(context.Background(), b.tenantID, b.requirementID,
		time.Now().UTC(), "legit retire", "b-admin")
	require.NoError(t, err)
	require.NotNil(t, updated.EffectiveTo)
}

// Retirement is CAS-guarded: the second attempt is rejected, never a silent
// no-op that double-applies.
func TestPgStore_EndDate_SecondAttemptRejected(t *testing.T) {
	f := setupFixture(t, "AlreadyRetired")

	_, err := testStore.EndDateRequirement(context.Background(), f.tenantID, f.requirementID,
		time.Now().UTC(), "first", "admin")
	require.NoError(t, err)

	_, err = testStore.EndDateRequirement(context.Background(), f.tenantID, f.requirementID,
		time.Now().UTC(), "second", "admin")
	assert.ErrorIs(t, err, domain.ErrAlreadyRetired)
}

func TestPgStore_TenantIsolation_ListRequirements(t *testing.T) {
	a := setupFixture(t, "A-List")
	_ = setupFixture(t, "B-List")

	list, err := testStore.ListRequirements(context.Background(), domain.ListRequirementsFilter{TenantID: a.tenantID})
	require.NoError(t, err)
	require.NotEmpty(t, list)
	for _, r := range list {
		assert.Equal(t, a.tenantID, r.TenantID, "ISOLATION FAILURE: ListRequirements returned another tenant's row")
	}
}

func TestPgStore_TenantIsolation_EffectiveRequirements(t *testing.T) {
	a := setupFixture(t, "A-Effective")
	b := setupFixture(t, "B-Effective")

	// Tenant B's scope, tenant A's legal entity — must return nothing of A's.
	got, err := testStore.EffectiveRequirements(context.Background(), b.tenantID, a.entityID,
		"FINANCE", "JOURNAL_POST", time.Now().UTC())
	require.NoError(t, err)
	for _, r := range got {
		assert.Equal(t, b.tenantID, r.TenantID,
			"ISOLATION FAILURE: EffectiveRequirements leaked another tenant's requirement into the gate")
	}

	// Sanity: tenant A resolves its own.
	own, err := testStore.EffectiveRequirements(context.Background(), a.tenantID, a.entityID,
		"FINANCE", "JOURNAL_POST", time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, own, 1)
	assert.Equal(t, a.requirementID, own[0].EvidenceRequirementID)
}

// A tenant-wide requirement (NULL legal_entity_id) must apply to any entity in
// that tenant — this is the D1 decision made executable.
func TestPgStore_EffectiveRequirements_TenantWideAppliesToAnyEntity(t *testing.T) {
	ctx := context.Background()
	tenantID := uuid.New().String()
	tctx := svcmiddleware.WithTenant(ctx, tenantID)

	r := &domain.EvidenceRequirement{
		EvidenceRequirementID: uuid.New().String(),
		TenantID:              tenantID,
		LegalEntityID:         nil, // tenant-wide
		DomainCode:            "TAX",
		ActionType:            "RETURN_SUBMIT",
		EvidenceType:          "SIGNATURE",
		RequirementPayload:    json.RawMessage(`{}`),
		EffectiveFrom:         time.Now().UTC().Add(-time.Hour),
		CreatedByPrincipalID:  "tenant-admin",
		CorrelationID:         "corr-tenantwide",
	}
	created, err := testStore.CreateRequirement(tctx, r)
	require.NoError(t, err)
	require.True(t, created)

	// Any arbitrary entity in this tenant must pick it up.
	got, err := testStore.EffectiveRequirements(ctx, tenantID, uuid.New().String(),
		"TAX", "RETURN_SUBMIT", time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].LegalEntityID)
}

// Effective dating is the retirement mechanism — a retired requirement must
// drop out of the gate, without ever being deleted.
func TestPgStore_EffectiveRequirements_ExcludesRetired(t *testing.T) {
	f := setupFixture(t, "Retired-Excluded")
	ctx := context.Background()

	before, err := testStore.EffectiveRequirements(ctx, f.tenantID, f.entityID,
		"FINANCE", "JOURNAL_POST", time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, before, 1)

	_, err = testStore.EndDateRequirement(ctx, f.tenantID, f.requirementID,
		time.Now().UTC().Add(-time.Minute), "superseded", "admin")
	require.NoError(t, err)

	after, err := testStore.EffectiveRequirements(ctx, f.tenantID, f.entityID,
		"FINANCE", "JOURNAL_POST", time.Now().UTC())
	require.NoError(t, err)
	assert.Empty(t, after, "a retired requirement must no longer gate the action")

	// But the row still exists — no soft-delete, no hard delete.
	tctx := svcmiddleware.WithTenant(ctx, f.tenantID)
	still, err := testStore.GetRequirement(tctx, f.requirementID)
	require.NoError(t, err)
	require.NotNil(t, still, "retirement must end-date the row, never remove it")
	require.NotNil(t, still.EffectiveTo)
}

// Idempotency is enforced by the database, not by convention.
func TestPgStore_CreateRequirement_IdempotentOnCorrelationID(t *testing.T) {
	ctx := context.Background()
	tenantID := uuid.New().String()
	entityID := uuid.New().String()
	tctx := svcmiddleware.WithTenant(ctx, tenantID)

	mk := func() *domain.EvidenceRequirement {
		return &domain.EvidenceRequirement{
			EvidenceRequirementID: uuid.New().String(),
			TenantID:              tenantID,
			LegalEntityID:         &entityID,
			DomainCode:            "FINANCE",
			ActionType:            "PERIOD_LOCK",
			EvidenceType:          "APPROVAL_RECORD",
			RequirementPayload:    json.RawMessage(`{}`),
			EffectiveFrom:         time.Now().UTC(),
			CreatedByPrincipalID:  "admin",
			CorrelationID:         "corr-idem-1",
		}
	}

	first := mk()
	created, err := testStore.CreateRequirement(tctx, first)
	require.NoError(t, err)
	require.True(t, created)

	// Replay with a DIFFERENT generated primary key but the same
	// correlation_id: must return the original row, not mint a second.
	second := mk()
	created, err = testStore.CreateRequirement(tctx, second)
	require.NoError(t, err)
	assert.False(t, created, "a replayed create must not insert a duplicate")
	assert.Equal(t, first.EvidenceRequirementID, second.EvidenceRequirementID,
		"the replay must return the ORIGINAL requirement")
}

func TestPgStore_TenantIsolation_GetEvaluation(t *testing.T) {
	ctx := context.Background()
	seed := func(label string) (tenantID, evalID string) {
		tenantID = uuid.New().String()
		evalID = uuid.New().String()
		e := &domain.EvidenceEvaluation{
			EvaluationID:            evalID,
			TenantID:                tenantID,
			LegalEntityID:           uuid.New().String(),
			DomainCode:              "FINANCE",
			ActionType:              "JOURNAL_POST",
			Outcome:                 domain.OutcomeMissing,
			UnmetPayload:            json.RawMessage(`[]`),
			PresentArtifactsPayload: json.RawMessage(`[]`),
			EvaluatedForPrincipalID: "p-" + label,
			CorrelationID:           "corr-eval-" + label,
		}
		created, err := testStore.RecordEvaluation(svcmiddleware.WithTenant(ctx, tenantID), e)
		require.NoError(t, err)
		require.True(t, created)
		return tenantID, evalID
	}

	_, aEval := seed("A")
	bTenant, bEval := seed("B")

	ctxB := svcmiddleware.WithTenant(ctx, bTenant)
	got, err := testStore.GetEvaluation(ctxB, aEval)
	require.NoError(t, err)
	assert.Nil(t, got, "ISOLATION FAILURE: GetEvaluation returned Tenant A's determination to Tenant B")

	own, err := testStore.GetEvaluation(ctxB, bEval)
	require.NoError(t, err)
	require.NotNil(t, own)
	assert.Equal(t, bEval, own.EvaluationID)
}

// A replayed evaluation must return the ORIGINAL determination — the handler
// relies on created=false to decide not to republish its Kafka event.
func TestPgStore_RecordEvaluation_IdempotentOnCorrelationID(t *testing.T) {
	ctx := context.Background()
	tenantID := uuid.New().String()
	entityID := uuid.New().String()
	tctx := svcmiddleware.WithTenant(ctx, tenantID)

	mk := func(outcome domain.Outcome) *domain.EvidenceEvaluation {
		return &domain.EvidenceEvaluation{
			EvaluationID:            uuid.New().String(),
			TenantID:                tenantID,
			LegalEntityID:           entityID,
			DomainCode:              "FINANCE",
			ActionType:              "JOURNAL_POST",
			Outcome:                 outcome,
			UnmetPayload:            json.RawMessage(`[]`),
			PresentArtifactsPayload: json.RawMessage(`[]`),
			EvaluatedForPrincipalID: "admin",
			CorrelationID:           "corr-eval-idem",
		}
	}

	first := mk(domain.OutcomeMissing)
	created, err := testStore.RecordEvaluation(tctx, first)
	require.NoError(t, err)
	require.True(t, created)

	// Replay, deliberately with a different outcome: the stored determination
	// must win. An append-only ledger never gets rewritten.
	second := mk(domain.OutcomeSatisfied)
	created, err = testStore.RecordEvaluation(tctx, second)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.EvaluationID, second.EvaluationID)
	assert.Equal(t, domain.OutcomeMissing, second.Outcome,
		"the recorded determination is authoritative — a replay must not overwrite it")
}
