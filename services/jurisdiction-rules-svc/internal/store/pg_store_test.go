package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// TestPgStore_FindRules_Integration verifies the real PostgreSQL SQL query for
// point-in-time rule fetching, specifically proving that:
//  1. Half-open interval filtering [effective_from, effective_to) works in Postgres.
//  2. Rules with status 'SUPERSEDED' ARE returned for historical effective_at queries.
//  3. Rules with status 'DRAFT' are excluded.
func TestPgStore_FindRules_Integration(t *testing.T) {
	s, pool, ctx := newTestStore(t)

	// Insert test jurisdiction
	const jurID = "a0000000-0000-0000-0000-000000000001"
	_, err := pool.Exec(ctx, `
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

// TestPgStore_FindRules_SurvivesDeactivation covers the §8.2 constraint that
// "historical actions must always be explainable against the rule set active
// at time of execution".
//
// FindRules resolved the jurisdiction with the active-only FindByID, so the
// moment a jurisdiction was deactivated every historical rule query against
// it started returning 404 — taking the audit trail with it.
func TestPgStore_FindRules_SurvivesDeactivation(t *testing.T) {
	s, _, ctx := newTestStore(t)
	j := mustCreateJurisdiction(t, s, ctx, "GB", nil)

	from := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := s.CreateRule(ctx, domain.CreateRuleParams{
		JurisdictionID:       j.JurisdictionID,
		RuleDomain:           "TAX",
		RuleCode:             "GB_VAT",
		RuleName:             "VAT",
		EffectiveFrom:        from,
		RulePayload:          []byte(`{}`),
		RuleStatus:           "ACTIVE",
		CreatedByPrincipalID: "admin-1",
	}); err != nil {
		t.Fatalf("failed to create rule: %v", err)
	}

	if _, err := s.DeactivateJurisdiction(ctx, j.JurisdictionID, "admin-1"); err != nil {
		t.Fatalf("failed to deactivate: %v", err)
	}

	rules, err := s.FindRules(ctx, store.FindRulesParams{
		JurisdictionID: j.JurisdictionID,
		EffectiveAt:    time.Date(2024, time.June, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("historical rule query must survive deactivation, got: %v", err)
	}
	if len(rules) != 1 {
		t.Errorf("expected the historical rule to still be explainable, got %d rules", len(rules))
	}

	// A genuinely unknown jurisdiction is still 404.
	if _, err := s.FindRules(ctx, store.FindRulesParams{JurisdictionID: "00000000-0000-0000-0000-0000000000ff"}); !errors.Is(err, domain.ErrJurisdictionNotFound) {
		t.Errorf("expected ErrJurisdictionNotFound for an unknown jurisdiction, got %v", err)
	}
}

// TestPgStore_List_Pagination verifies limit/offset actually page, and that
// the ordering is total — jurisdiction_code alone is not unique, so without
// the id tiebreaker two pages could repeat or skip a row.
func TestPgStore_List_Pagination(t *testing.T) {
	s, _, ctx := newTestStore(t)

	for _, code := range []string{"AA", "BB", "CC", "DD", "EE"} {
		mustCreateJurisdiction(t, s, ctx, code, nil)
	}

	page1, err := s.List(ctx, store.ListParams{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	page2, err := s.List(ctx, store.ListParams{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("expected 2 rows per page, got %d and %d", len(page1), len(page2))
	}
	if page1[0].JurisdictionCode != "AA" || page1[1].JurisdictionCode != "BB" {
		t.Errorf("page 1 = %s,%s — want AA,BB", page1[0].JurisdictionCode, page1[1].JurisdictionCode)
	}
	if page2[0].JurisdictionCode != "CC" || page2[1].JurisdictionCode != "DD" {
		t.Errorf("page 2 = %s,%s — want CC,DD", page2[0].JurisdictionCode, page2[1].JurisdictionCode)
	}

	// active=true must exclude a deactivated jurisdiction.
	if _, err := s.DeactivateJurisdiction(ctx, page1[0].JurisdictionID, "admin-1"); err != nil {
		t.Fatalf("failed to deactivate: %v", err)
	}
	activeOnly, err := s.List(ctx, store.ListParams{ActiveOnly: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(activeOnly) != 4 {
		t.Errorf("expected 4 active jurisdictions after deactivating one, got %d", len(activeOnly))
	}
}
