package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/store"
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
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS filing_requirements, obligations CASCADE;")

	mig1, err := os.ReadFile("../../deployments/migrations/000001_initial_schema.up.sql")
	if err != nil {
		t.Fatalf("failed to read migration 1: %v", err)
	}
	if _, err := pool.Exec(ctx, string(mig1)); err != nil {
		t.Fatalf("failed to execute migration 1: %v", err)
	}
}

func validParams() domain.CreateObligationParams {
	return domain.CreateObligationParams{
		LegalEntityID:        "00000000-0000-0000-0000-000000000001",
		JurisdictionID:       "00000000-0000-0000-0000-000000000002",
		ObligationSourceType: "JURISDICTION_RULE",
		ObligationSourceID:   "rule-1",
		ObligationCode:       "OBL-2026-001",
		ObligationType:       "FILING",
		DueDate:              time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC),
		SeverityLevel:        "HIGH",
		ResponsibleFunction:  "Tax",
		SourceReference:      "IN-GST-FILING-RULE-07",
		CreatedByPrincipalID: "admin-1",
	}
}

func TestPgStore_CreateObligation_IdempotencyAnd409(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	params := validParams()

	// 1. Initial creation
	o1, created, err := s.CreateObligation(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on create: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on initial insert")
	}
	if o1.ObligationCode != "OBL-2026-001" {
		t.Errorf("expected code OBL-2026-001, got %s", o1.ObligationCode)
	}
	if o1.ObligationStatus != "OPEN" {
		t.Errorf("expected default status OPEN, got %s", o1.ObligationStatus)
	}

	// 2. Identical retry (idempotent no-op)
	o2, created, err := s.CreateObligation(ctx, params)
	if err != nil {
		t.Fatalf("unexpected error on identical retry: %v", err)
	}
	if created {
		t.Errorf("expected created=false on identical retry")
	}
	if o2.ObligationID != o1.ObligationID {
		t.Errorf("expected ID %s, got %s", o1.ObligationID, o2.ObligationID)
	}

	// 3. Same code, different attribute -> 409
	conflicting := params
	conflicting.SeverityLevel = "CRITICAL"
	conflicting.DueDate = params.DueDate.Add(24 * time.Hour)
	_, _, err = s.CreateObligation(ctx, conflicting)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestPgStore_FindObligationByID_NotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	_, err := s.FindObligationByID(context.Background(), "00000000-0000-0000-0000-000000000099")
	if !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("expected ErrObligationNotFound, got %v", err)
	}
}

func TestPgStore_ListObligations_Filters(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	p1 := validParams()
	p1.ObligationCode = "OBL-A"
	p1.LegalEntityID = "00000000-0000-0000-0000-0000000000a1"
	if _, _, err := s.CreateObligation(ctx, p1); err != nil {
		t.Fatalf("create p1: %v", err)
	}

	p2 := validParams()
	p2.ObligationCode = "OBL-B"
	p2.LegalEntityID = "00000000-0000-0000-0000-0000000000b2"
	if _, _, err := s.CreateObligation(ctx, p2); err != nil {
		t.Fatalf("create p2: %v", err)
	}

	results, err := s.ListObligations(ctx, domain.ListObligationsFilter{LegalEntityID: p1.LegalEntityID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 || results[0].ObligationCode != "OBL-A" {
		t.Fatalf("expected exactly OBL-A for legal_entity_id filter, got %+v", results)
	}

	all, err := s.ListObligations(ctx, domain.ListObligationsFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 obligations with no filter, got %d", len(all))
	}
}

func TestPgStore_UpdateObligationStatus_LegalTransitionsAndIdempotency(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// OPEN -> IN_PROGRESS: legal
	updated, transitioned, err := s.UpdateObligationStatus(ctx, o.ObligationID, "IN_PROGRESS")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !transitioned || updated.ObligationStatus != "IN_PROGRESS" {
		t.Fatalf("expected transition to IN_PROGRESS, got status=%s transitioned=%v", updated.ObligationStatus, transitioned)
	}

	// IN_PROGRESS -> IN_PROGRESS: idempotent no-op
	_, transitioned, err = s.UpdateObligationStatus(ctx, o.ObligationID, "IN_PROGRESS")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if transitioned {
		t.Errorf("expected idempotent no-op, got transitioned=true")
	}

	// IN_PROGRESS -> CLOSED: legal, closed_at must be stamped
	closed, transitioned, err := s.UpdateObligationStatus(ctx, o.ObligationID, "CLOSED")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !transitioned || closed.ObligationStatus != "CLOSED" {
		t.Fatalf("expected transition to CLOSED, got status=%s", closed.ObligationStatus)
	}
	if closed.ClosedAt == nil {
		t.Errorf("expected closed_at to be stamped on transition to CLOSED")
	}

	// CLOSED -> anything: illegal, CLOSED is terminal
	_, _, err = s.UpdateObligationStatus(ctx, o.ObligationID, "OPEN")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition out of terminal CLOSED, got %v", err)
	}
}

func TestPgStore_UpdateObligationStatus_IllegalSkipTransition(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// OPEN -> OVERDUE is legal (system-driven overdue sweep can skip IN_PROGRESS)
	_, transitioned, err := s.UpdateObligationStatus(ctx, o.ObligationID, "OVERDUE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !transitioned {
		t.Fatalf("expected OPEN -> OVERDUE to be legal")
	}

	// OVERDUE -> IN_PROGRESS is illegal (only CLOSED is reachable from OVERDUE)
	_, _, err = s.UpdateObligationStatus(ctx, o.ObligationID, "IN_PROGRESS")
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for OVERDUE -> IN_PROGRESS, got %v", err)
	}
}

func TestPgStore_FilingRequirement_CreateAndList(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	ctx := context.Background()

	o, _, err := s.CreateObligation(ctx, validParams())
	if err != nil {
		t.Fatalf("create obligation: %v", err)
	}

	f, err := s.CreateFilingRequirement(ctx, domain.CreateFilingRequirementParams{
		ObligationID:      o.ObligationID,
		FilingType:        "ANNUAL_RETURN",
		FilingAuthority:   "IRS",
		SubmissionChannel: "E_FILE",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.FilingStatus != "PENDING" {
		t.Errorf("expected default filing_status PENDING, got %s", f.FilingStatus)
	}

	list, err := s.ListFilingRequirements(ctx, o.ObligationID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(list) != 1 || list[0].FilingRequirementID != f.FilingRequirementID {
		t.Fatalf("expected exactly the one filing requirement, got %+v", list)
	}
}

func TestPgStore_CreateFilingRequirement_ObligationNotFound(t *testing.T) {
	pool := getTestPool(t)
	defer pool.Close()
	setupTestDB(t, pool)

	s := store.New(pool, zap.NewNop())
	_, err := s.CreateFilingRequirement(context.Background(), domain.CreateFilingRequirementParams{
		ObligationID:      "00000000-0000-0000-0000-000000000099",
		FilingType:        "ANNUAL_RETURN",
		FilingAuthority:   "IRS",
		SubmissionChannel: "E_FILE",
	})
	if !errors.Is(err, domain.ErrObligationNotFound) {
		t.Fatalf("expected ErrObligationNotFound, got %v", err)
	}
}
