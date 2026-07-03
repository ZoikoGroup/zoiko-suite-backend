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

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
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
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS jurisdiction_rules CASCADE;")
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS jurisdictions CASCADE;")

	mig1, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	if err != nil {
		t.Fatalf("failed to read migration 1: %v", err)
	}
	if _, err := pool.Exec(ctx, string(mig1)); err != nil {
		t.Fatalf("failed to execute migration 1: %v", err)
	}

	mig2, err := os.ReadFile("../../deployments/migrations/000002_add_audit_columns.up.sql")
	if err != nil {
		t.Fatalf("failed to read migration 2: %v", err)
	}
	if _, err := pool.Exec(ctx, string(mig2)); err != nil {
		t.Fatalf("failed to execute migration 2: %v", err)
	}
}

func TestPgStore_CreateJurisdiction_IdempotencyAnd409(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	id := uuid.New().String()
	params := domain.CreateJurisdictionParams{
		JurisdictionID:       id,
		JurisdictionCode:     "US-CA",
		JurisdictionName:     "California",
		JurisdictionType:     "STATE_PROVINCE",
		AuthorityType:        "STATE",
		EffectiveFrom:        time.Now().UTC().Truncate(time.Microsecond),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	}

	// 1. Initial creation
	j1, created, err := s.CreateJurisdiction(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on create: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if j1.JurisdictionCode != "US-CA" {
		t.Errorf("expected code US-CA, got %s", j1.JurisdictionCode)
	}

	// 2. Identical retry (idempotent 200 OK no-op)
	j2, created, err := s.CreateJurisdiction(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on identical retry")
	}
	if j2.JurisdictionID != j1.JurisdictionID {
		t.Errorf("expected ID %s, got %s", j1.JurisdictionID, j2.JurisdictionID)
	}

	// 3. Differing attribute on same dedup key (409 Conflict)
	conflictParams := params
	conflictParams.JurisdictionID = uuid.New().String() // different ID, but same (code, type, parent)
	conflictParams.JurisdictionName = "California Republic State" // differing attribute!

	_, created, err = s.CreateJurisdiction(ctx, conflictParams)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict (409) on differing attribute, got: %v", err)
	}
	if created {
		t.Errorf("expected created=false on conflict")
	}
}

func TestPgStore_DeactivateJurisdiction(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	id := uuid.New().String()
	params := domain.CreateJurisdictionParams{
		JurisdictionID:       id,
		JurisdictionCode:     "GB",
		JurisdictionName:     "United Kingdom",
		JurisdictionType:     "COUNTRY",
		AuthorityType:        "FEDERAL",
		EffectiveFrom:        time.Now().UTC(),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	}

	_, _, err := s.CreateJurisdiction(ctx, params)
	if err != nil {
		t.Fatalf("failed to create jurisdiction: %v", err)
	}

	// Deactivate
	deactivated, err := s.DeactivateJurisdiction(ctx, id, "actor-deactivate")
	if err != nil {
		t.Fatalf("unexpected error on deactivate: %v", err)
	}
	if deactivated.ActiveFlag {
		t.Errorf("expected ActiveFlag=false after deactivation")
	}
	if deactivated.UpdatedAt == nil {
		t.Fatal("expected UpdatedAt to be set")
	}
	if deactivated.UpdatedByPrincipalID == nil || *deactivated.UpdatedByPrincipalID != "actor-deactivate" {
		t.Errorf("expected UpdatedByPrincipalID=actor-deactivate, got %v", deactivated.UpdatedByPrincipalID)
	}

	// Non-existent deactivation
	_, err = s.DeactivateJurisdiction(ctx, uuid.New().String(), "actor-1")
	if !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("expected ErrJurisdictionNotFound for unknown UUID, got: %v", err)
	}
}

func TestPgStore_CreateRule_IdempotencyAnd409(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	// Create parent jurisdiction first
	jid := uuid.New().String()
	_, _, err := s.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionID:       jid,
		JurisdictionCode:     "DE",
		JurisdictionName:     "Germany",
		JurisdictionType:     "COUNTRY",
		AuthorityType:        "FEDERAL",
		EffectiveFrom:        time.Now().UTC(),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create parent jurisdiction: %v", err)
	}

	ruleID := uuid.New().String()
	effFrom := time.Now().UTC().Truncate(time.Microsecond)
	params := domain.CreateRuleParams{
		JurisdictionRuleID:   ruleID,
		JurisdictionID:       jid,
		RuleDomain:           "TAX",
		RuleCode:             "DE_VAT_STANDARD",
		RuleName:             "Standard VAT Rate",
		EffectiveFrom:        effFrom,
		RulePayload:          []byte(`{"rate": 0.19}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	}

	// 1. Initial creation
	r1, created, err := s.CreateRule(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error creating rule: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if r1.RuleCode != "DE_VAT_STANDARD" {
		t.Errorf("expected code DE_VAT_STANDARD, got %s", r1.RuleCode)
	}

	// 2. Identical retry (idempotent 200 OK no-op)
	r2, created, err := s.CreateRule(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on retry")
	}
	if r2.JurisdictionRuleID != r1.JurisdictionRuleID {
		t.Errorf("expected ID %s, got %s", r1.JurisdictionRuleID, r2.JurisdictionRuleID)
	}

	// 3. Differing payload on same dedup key (409 Conflict)
	conflictParams := params
	conflictParams.JurisdictionRuleID = uuid.New().String()
	conflictParams.RulePayload = []byte(`{"rate": 0.20}`) // differing payload!

	_, created, err = s.CreateRule(ctx, conflictParams)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict (409) on differing payload, got: %v", err)
	}
	if created {
		t.Errorf("expected created=false on conflict")
	}
}

func TestPgStore_TransitionRuleStatus_StateMachineAndNoOp(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	jid := uuid.New().String()
	_, _, err := s.CreateJurisdiction(ctx, domain.CreateJurisdictionParams{
		JurisdictionID:       jid,
		JurisdictionCode:     "FR",
		JurisdictionName:     "France",
		JurisdictionType:     "COUNTRY",
		AuthorityType:        "FEDERAL",
		EffectiveFrom:        time.Now().UTC(),
		ActiveFlag:           true,
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create jurisdiction: %v", err)
	}

	ruleID := uuid.New().String()
	r, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionRuleID:   ruleID,
		JurisdictionID:       jid,
		RuleDomain:           "PAYROLL",
		RuleCode:             "FR_SOCIAL_SEC",
		RuleName:             "Social Security Contribution",
		EffectiveFrom:        time.Now().UTC(),
		RulePayload:          []byte(`{"applies": true}`),
		RuleStatus:           "DRAFT", // initial state
		CreatedByPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	// 1. Legal transition DRAFT -> ACTIVE
	allowedPriors := []string{"DRAFT"}
	updated, err := s.TransitionRuleStatus(ctx, r.JurisdictionRuleID, "ACTIVE", allowedPriors, "actor-1")
	if err != nil {
		t.Fatalf("unexpected error transitioning DRAFT -> ACTIVE: %v", err)
	}
	if updated.RuleStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE, got %s", updated.RuleStatus)
	}
	if updated.UpdatedAt == nil {
		t.Fatal("expected UpdatedAt to be set after transition")
	}

	// 2. Idempotent network retry: call ACTIVE again when current is already ACTIVE!
	// Notice allowedPriors is still ["DRAFT"] — without our pre-read check, this would fail!
	retryUpdated, err := s.TransitionRuleStatus(ctx, r.JurisdictionRuleID, "ACTIVE", allowedPriors, "actor-1")
	if err != nil {
		t.Fatalf("unexpected error on idempotent retry: %v", err)
	}
	if retryUpdated.RuleStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE on retry, got %s", retryUpdated.RuleStatus)
	}

	// 3. Illegal transition: try ACTIVE -> DRAFT (DRAFT is not in allowed priors for moving backward)
	illegalPriors := []string{} // cannot move back to DRAFT from anything
	_, err = s.TransitionRuleStatus(ctx, r.JurisdictionRuleID, "DRAFT", illegalPriors, "actor-1")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition on illegal transition, got: %v", err)
	}
}
