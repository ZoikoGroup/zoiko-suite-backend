package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// TestPgStore_FindRules_Integration verifies the real PostgreSQL SQL query for
// point-in-time rule fetching, specifically proving that:
// 1. Half-open interval filtering [effective_from, effective_to) works in Postgres.
// 2. Rules with status 'SUPERSEDED' ARE returned for historical effective_at queries.
// 3. Rules with status 'DRAFT' are excluded.
func TestPgStore_FindRules_Integration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("Skipping Postgres integration test: TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pool.Close()

	// Locate and apply migration schema
	_, filename, _, _ := runtime.Caller(0)
	migPath := filepath.Join(filepath.Dir(filename), "../../deployments/migrations/000001_initial_schema.up.sql")
	migSQL, err := os.ReadFile(migPath)
	if err != nil {
		t.Fatalf("failed to read migration file %s: %v", migPath, err)
	}

	// Drop tables if existing for clean state
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS jurisdiction_rule_drift_events, jurisdiction_rules, jurisdictions CASCADE;")
	if _, err := pool.Exec(ctx, string(migSQL)); err != nil {
		t.Fatalf("failed to execute migration 1: %v", err)
	}

	migPath2 := filepath.Join(filepath.Dir(filename), "../../deployments/migrations/000002_add_audit_columns.up.sql")
	migSQL2, err := os.ReadFile(migPath2)
	if err != nil {
		t.Fatalf("failed to read migration file %s: %v", migPath2, err)
	}
	if _, err := pool.Exec(ctx, string(migSQL2)); err != nil {
		t.Fatalf("failed to execute migration 2: %v", err)
	}

	logger := zap.NewNop()
	s := store.New(pool, logger)

	// Insert test jurisdiction
	const jurID = "a0000000-0000-0000-0000-000000000001"
	_, err = pool.Exec(ctx, `
		INSERT INTO jurisdictions (jurisdiction_id, jurisdiction_code, jurisdiction_name, jurisdiction_type, authority_type, effective_from, active_flag, created_by_principal_id)
		VALUES ($1, 'TEST-US', 'Test US', 'COUNTRY', 'FEDERAL', '2020-01-01T00:00:00Z', true, 'admin');
	`, jurID)
	if err != nil {
		t.Fatalf("failed to insert test jurisdiction: %v", err)
	}

	// Insert Rule 1: SUPERSEDED (active from 2024-01-01 to 2025-01-01)
	const rule1ID = "b0000000-0000-0000-0000-000000000001"
	_, err = pool.Exec(ctx, `
		INSERT INTO jurisdiction_rules (jurisdiction_rule_id, jurisdiction_id, rule_domain, rule_code, rule_name, effective_from, effective_to, rule_payload, rule_status, created_by_principal_id)
		VALUES ($1, $2, 'TAX', 'RATE', 'Historical Rate', '2024-01-01T00:00:00Z', '2025-01-01T00:00:00Z', '{"rate": 0.20}', 'SUPERSEDED', 'admin');
	`, rule1ID, jurID)
	if err != nil {
		t.Fatalf("failed to insert rule 1: %v", err)
	}

	// Insert Rule 2: ACTIVE (active from 2025-01-01 onward — NULL effective_to)
	const rule2ID = "b0000000-0000-0000-0000-000000000002"
	_, err = pool.Exec(ctx, `
		INSERT INTO jurisdiction_rules (jurisdiction_rule_id, jurisdiction_id, rule_domain, rule_code, rule_name, effective_from, effective_to, rule_payload, rule_status, created_by_principal_id)
		VALUES ($1, $2, 'TAX', 'RATE', 'Current Rate', '2025-01-01T00:00:00Z', NULL, '{"rate": 0.25}', 'ACTIVE', 'admin');
	`, rule2ID, jurID)
	if err != nil {
		t.Fatalf("failed to insert rule 2: %v", err)
	}

	// Insert Rule 3: DRAFT (should be ignored even if dates match)
	const rule3ID = "b0000000-0000-0000-0000-000000000003"
	_, err = pool.Exec(ctx, `
		INSERT INTO jurisdiction_rules (jurisdiction_rule_id, jurisdiction_id, rule_domain, rule_code, rule_name, effective_from, effective_to, rule_payload, rule_status, created_by_principal_id)
		VALUES ($1, $2, 'TAX', 'RATE', 'Draft Rate', '2024-06-01T00:00:00Z', NULL, '{"rate": 0.30}', 'DRAFT', 'admin');
	`, rule3ID, jurID)
	if err != nil {
		t.Fatalf("failed to insert rule 3: %v", err)
	}

	// Test Case 1: Query historical point in time (2024-06-01).
	// Must return Rule 1 (SUPERSEDED) and ignore Rule 2 and Rule 3 (DRAFT).
	histTime := time.Date(2024, time.June, 1, 0, 0, 0, 0, time.UTC)
	rulesHist, err := s.FindRules(ctx, store.FindRulesParams{
		JurisdictionID: jurID,
		Domain:         "TAX",
		EffectiveAt:    histTime,
	})
	if err != nil {
		t.Fatalf("FindRules historical query failed: %v", err)
	}
	if len(rulesHist) != 1 {
		t.Fatalf("expected 1 historical rule, got %d", len(rulesHist))
	}
	if rulesHist[0].JurisdictionRuleID != rule1ID {
		t.Errorf("expected historical rule ID %s, got %s", rule1ID, rulesHist[0].JurisdictionRuleID)
	}
	if rulesHist[0].RuleStatus != "SUPERSEDED" {
		t.Errorf("expected status SUPERSEDED, got %s", rulesHist[0].RuleStatus)
	}

	// Test Case 2: Query current point in time (2025-06-01).
	// Must return Rule 2 (ACTIVE).
	currTime := time.Date(2025, time.June, 1, 0, 0, 0, 0, time.UTC)
	rulesCurr, err := s.FindRules(ctx, store.FindRulesParams{
		JurisdictionID: jurID,
		Domain:         "TAX",
		EffectiveAt:    currTime,
	})
	if err != nil {
		t.Fatalf("FindRules current query failed: %v", err)
	}
	if len(rulesCurr) != 1 {
		t.Fatalf("expected 1 current rule, got %d", len(rulesCurr))
	}
	if rulesCurr[0].JurisdictionRuleID != rule2ID {
		t.Errorf("expected current rule ID %s, got %s", rule2ID, rulesCurr[0].JurisdictionRuleID)
	}
	if rulesCurr[0].RuleStatus != "ACTIVE" {
		t.Errorf("expected status ACTIVE, got %s", rulesCurr[0].RuleStatus)
	}
}
