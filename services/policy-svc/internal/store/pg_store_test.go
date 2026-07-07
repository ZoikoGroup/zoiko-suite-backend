package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/store"
)

func getTestPool(t *testing.T) *pgxpool.Pool {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping real PostgreSQL integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("failed to connect to TEST_DATABASE_URL: %v", err)
	}
	return pool
}

func setupTestDB(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS policy_versions, policies CASCADE;")

	mig1, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	if err != nil {
		t.Fatalf("failed to read migration 1: %v", err)
	}
	if _, err := pool.Exec(ctx, string(mig1)); err != nil {
		t.Fatalf("failed to execute migration 1: %v", err)
	}
}

func strPtr(s string) *string { return &s }

func TestPgStore_CreatePolicy_IdempotencyAnd409(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	params := domain.CreatePolicyParams{
		PolicyCode:           "APPROVAL_5K",
		PolicyName:           "5K Approval Threshold",
		PolicyType:           "APPROVAL_THRESHOLD",
		CreatedByPrincipalID: "admin-1",
	}

	// 1. Initial creation
	p1, created, err := s.CreatePolicy(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on create: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if p1.PolicyCode != "APPROVAL_5K" {
		t.Errorf("expected code APPROVAL_5K, got %s", p1.PolicyCode)
	}

	// 2. Identical retry (idempotent no-op)
	p2, created, err := s.CreatePolicy(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on identical retry")
	}
	if p2.PolicyID != p1.PolicyID {
		t.Errorf("expected ID %s, got %s", p1.PolicyID, p2.PolicyID)
	}

	// 3. Differing attribute on same dedup key (409 Conflict)
	conflictParams := params
	conflictParams.PolicyName = "Different Name"

	_, created, err = s.CreatePolicy(ctx, conflictParams)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict (409) on differing attribute, got: %v", err)
	}
	if created {
		t.Errorf("expected created=false on conflict")
	}
}

func TestPgStore_CreatePolicyVersion_IdempotencyConflictAndPolicyNotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	// Policy not found
	_, _, err := s.CreatePolicyVersion(ctx, domain.CreatePolicyVersionParams{
		PolicyID:             uuid.New().String(),
		RulePayload:          []byte(`{"threshold_amount":5000}`),
		EffectiveFrom:        time.Now().UTC(),
		CreatedByPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrPolicyNotFound) {
		t.Fatalf("expected ErrPolicyNotFound, got: %v", err)
	}

	p, _, err := s.CreatePolicy(ctx, domain.CreatePolicyParams{
		PolicyCode:           "APPROVAL_5K",
		PolicyName:           "5K Approval Threshold",
		PolicyType:           "APPROVAL_THRESHOLD",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	effFrom := time.Now().UTC().Truncate(time.Microsecond)
	params := domain.CreatePolicyVersionParams{
		PolicyID:             p.PolicyID,
		RulePayload:          []byte(`{"threshold_amount":5000}`),
		EffectiveFrom:        effFrom,
		CreatedByPrincipalID: "admin-1",
	}

	// 1. Initial creation
	v1, created, err := s.CreatePolicyVersion(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error creating version: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if v1.VersionStatus != "DRAFT" {
		t.Errorf("expected status DRAFT, got %s", v1.VersionStatus)
	}

	// 2. Identical retry (idempotent no-op)
	v2, created, err := s.CreatePolicyVersion(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on retry")
	}
	if v2.PolicyVersionID != v1.PolicyVersionID {
		t.Errorf("expected ID %s, got %s", v1.PolicyVersionID, v2.PolicyVersionID)
	}

	// 3. Differing payload on same dedup key (409 Conflict)
	conflictParams := params
	conflictParams.RulePayload = []byte(`{"threshold_amount":9999}`)

	_, created, err = s.CreatePolicyVersion(ctx, conflictParams)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict (409) on differing payload, got: %v", err)
	}
	if created {
		t.Errorf("expected created=false on conflict")
	}
}

func TestPgStore_ActivateVersion_SupersedesPreviousActiveAndIsIdempotent(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	p, _, err := s.CreatePolicy(ctx, domain.CreatePolicyParams{
		PolicyCode:           "APPROVAL_5K",
		PolicyName:           "5K Approval Threshold",
		PolicyType:           "APPROVAL_THRESHOLD",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	tenantID := strPtr(uuid.New().String())

	v1, _, err := s.CreatePolicyVersion(ctx, domain.CreatePolicyVersionParams{
		PolicyID:             p.PolicyID,
		TenantID:             tenantID,
		RulePayload:          []byte(`{"threshold_amount":5000}`),
		EffectiveFrom:        time.Now().UTC().Truncate(time.Microsecond),
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create version 1: %v", err)
	}

	// 1. Activate v1 (DRAFT -> ACTIVE), nothing to supersede yet.
	activated1, superseded1, transitioned1, err := s.ActivateVersion(ctx, v1.PolicyVersionID, "actor-1")
	if err != nil {
		t.Fatalf("unexpected error activating v1: %v", err)
	}
	if activated1.VersionStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE, got %s", activated1.VersionStatus)
	}
	if !transitioned1 {
		t.Errorf("expected transitioned=true for a real first activation")
	}
	if len(superseded1) != 0 {
		t.Errorf("expected nothing superseded on first activation, got %d", len(superseded1))
	}

	// 2. Idempotent retry: activating v1 again is a no-op, still ACTIVE.
	retry, retrySuperseded, retryTransitioned, err := s.ActivateVersion(ctx, v1.PolicyVersionID, "actor-1")
	if err != nil {
		t.Fatalf("unexpected error on idempotent retry: %v", err)
	}
	if retry.VersionStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE on retry, got %s", retry.VersionStatus)
	}
	if retryTransitioned {
		t.Errorf("expected transitioned=false on idempotent retry")
	}
	if len(retrySuperseded) != 0 {
		t.Errorf("expected nothing superseded on idempotent retry, got %d", len(retrySuperseded))
	}

	// 3. Create v2 in the SAME scope and activate it — must supersede v1.
	v2, _, err := s.CreatePolicyVersion(ctx, domain.CreatePolicyVersionParams{
		PolicyID:             p.PolicyID,
		TenantID:             tenantID,
		RulePayload:          []byte(`{"threshold_amount":7500}`),
		EffectiveFrom:        time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create version 2: %v", err)
	}

	activated2, superseded2, transitioned2, err := s.ActivateVersion(ctx, v2.PolicyVersionID, "actor-1")
	if err != nil {
		t.Fatalf("unexpected error activating v2: %v", err)
	}
	if activated2.VersionStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE, got %s", activated2.VersionStatus)
	}
	if !transitioned2 {
		t.Errorf("expected transitioned=true when activating v2")
	}
	if len(superseded2) != 1 || superseded2[0].PolicyVersionID != v1.PolicyVersionID {
		t.Fatalf("expected v1 to be returned as superseded, got %+v", superseded2)
	}
	if superseded2[0].VersionStatus != "SUPERSEDED" {
		t.Errorf("expected superseded entry to report status SUPERSEDED, got %s", superseded2[0].VersionStatus)
	}

	v1AfterSupersede, err := s.FindPolicyVersionByID(ctx, v1.PolicyVersionID)
	if err != nil {
		t.Fatalf("failed to re-fetch v1: %v", err)
	}
	if v1AfterSupersede.VersionStatus != "SUPERSEDED" {
		t.Errorf("expected v1 to be SUPERSEDED after activating v2, got %s", v1AfterSupersede.VersionStatus)
	}

	// 4. Illegal transition: activating a SUPERSEDED version must fail.
	_, _, _, err = s.ActivateVersion(ctx, v1.PolicyVersionID, "actor-1")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition activating a SUPERSEDED version, got: %v", err)
	}
}

func TestPgStore_ListVersionHistory_NewestFirstIncludesSuperseded(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	p, _, err := s.CreatePolicy(ctx, domain.CreatePolicyParams{
		PolicyCode:           "APPROVAL_5K",
		PolicyName:           "5K Approval Threshold",
		PolicyType:           "APPROVAL_THRESHOLD",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	v1, _, err := s.CreatePolicyVersion(ctx, domain.CreatePolicyVersionParams{
		PolicyID:             p.PolicyID,
		RulePayload:          []byte(`{"threshold_amount":5000}`),
		EffectiveFrom:        time.Now().UTC().Truncate(time.Microsecond),
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create v1: %v", err)
	}
	if _, _, _, err := s.ActivateVersion(ctx, v1.PolicyVersionID, "actor-1"); err != nil {
		t.Fatalf("failed to activate v1: %v", err)
	}

	v2, _, err := s.CreatePolicyVersion(ctx, domain.CreatePolicyVersionParams{
		PolicyID:             p.PolicyID,
		RulePayload:          []byte(`{"threshold_amount":7500}`),
		EffectiveFrom:        time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create v2: %v", err)
	}
	if _, _, _, err := s.ActivateVersion(ctx, v2.PolicyVersionID, "actor-1"); err != nil {
		t.Fatalf("failed to activate v2: %v", err)
	}

	history, err := s.ListVersionHistory(ctx, p.PolicyID)
	if err != nil {
		t.Fatalf("unexpected error listing history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 versions in history, got %d", len(history))
	}
	if history[0].PolicyVersionID != v2.PolicyVersionID {
		t.Errorf("expected v2 (newest) first, got %s", history[0].PolicyVersionID)
	}
	if history[1].VersionStatus != "SUPERSEDED" {
		t.Errorf("expected v1 to show as SUPERSEDED in history, got %s", history[1].VersionStatus)
	}
}

func TestPgStore_FindApplicableVersions_ScopePrecedenceAndIsolation(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	p, _, err := s.CreatePolicy(ctx, domain.CreatePolicyParams{
		PolicyCode:           "APPROVAL_5K",
		PolicyName:           "5K Approval Threshold",
		PolicyType:           "APPROVAL_THRESHOLD",
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	tenantA := strPtr(uuid.New().String())
	tenantB := strPtr(uuid.New().String())

	// Global version (no tenant/entity scope).
	global, _, err := s.CreatePolicyVersion(ctx, domain.CreatePolicyVersionParams{
		PolicyID:             p.PolicyID,
		RulePayload:          []byte(`{"threshold_amount":1000}`),
		EffectiveFrom:        time.Now().UTC().Truncate(time.Microsecond),
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create global version: %v", err)
	}
	if _, _, _, err := s.ActivateVersion(ctx, global.PolicyVersionID, "actor-1"); err != nil {
		t.Fatalf("failed to activate global version: %v", err)
	}

	// Tenant-A-specific override, more specific than global.
	tenantSpecific, _, err := s.CreatePolicyVersion(ctx, domain.CreatePolicyVersionParams{
		PolicyID:             p.PolicyID,
		TenantID:             tenantA,
		RulePayload:          []byte(`{"threshold_amount":9000}`),
		EffectiveFrom:        time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create tenant-specific version: %v", err)
	}
	if _, _, _, err := s.ActivateVersion(ctx, tenantSpecific.PolicyVersionID, "actor-1"); err != nil {
		t.Fatalf("failed to activate tenant-specific version: %v", err)
	}

	// Tenant A: should see BOTH (tenant-specific first, most specific; global second).
	resultsA, err := s.FindApplicableVersions(ctx, "APPROVAL_THRESHOLD", tenantA, nil)
	if err != nil {
		t.Fatalf("unexpected error for tenant A: %v", err)
	}
	if len(resultsA) != 2 {
		t.Fatalf("expected 2 applicable versions for tenant A, got %d", len(resultsA))
	}
	if resultsA[0].PolicyVersionID != tenantSpecific.PolicyVersionID {
		t.Errorf("expected tenant-specific version first (most specific), got %s", resultsA[0].PolicyVersionID)
	}
	if resultsA[1].PolicyVersionID != global.PolicyVersionID {
		t.Errorf("expected global version second, got %s", resultsA[1].PolicyVersionID)
	}
	if resultsA[0].PolicyCode != "APPROVAL_5K" {
		t.Errorf("expected policy_code APPROVAL_5K to be joined in, got %s", resultsA[0].PolicyCode)
	}

	// Tenant B: must NOT see tenant A's override — only the global fallback.
	resultsB, err := s.FindApplicableVersions(ctx, "APPROVAL_THRESHOLD", tenantB, nil)
	if err != nil {
		t.Fatalf("unexpected error for tenant B: %v", err)
	}
	if len(resultsB) != 1 {
		t.Fatalf("expected 1 applicable version for tenant B (global only), got %d", len(resultsB))
	}
	if resultsB[0].PolicyVersionID != global.PolicyVersionID {
		t.Errorf("expected global version for tenant B, got %s", resultsB[0].PolicyVersionID)
	}

	// No tenant specified at all: only the global version applies.
	resultsNone, err := s.FindApplicableVersions(ctx, "APPROVAL_THRESHOLD", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error with no tenant: %v", err)
	}
	if len(resultsNone) != 1 || resultsNone[0].PolicyVersionID != global.PolicyVersionID {
		t.Fatalf("expected only the global version with no tenant specified, got %+v", resultsNone)
	}

	// Wrong policy_type: no matches.
	resultsWrongType, err := s.FindApplicableVersions(ctx, "SPEND_CONTROL", tenantA, nil)
	if err != nil {
		t.Fatalf("unexpected error for wrong policy_type: %v", err)
	}
	if len(resultsWrongType) != 0 {
		t.Fatalf("expected 0 results for unrelated policy_type, got %d", len(resultsWrongType))
	}
}
