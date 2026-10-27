package store_test

import (
	"errors"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// A11: a definition's first rules are a draft; nothing runs until someone
// other than their creator approves them.
func TestRuleVersions_DraftCannotRunUntilIndependentlyApproved(t *testing.T) {
	f := newFixture(t)
	d, err := f.s.DefineReconciliation(f.ctx, orgA, defineRequest("draft", 0), "test-operator", f.claim("DefineReconciliation", "draft"))
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != 0 || d.LatestRuleVersion != 1 || d.Populations != nil {
		t.Fatalf("a draft-only definition has no rules in effect: %+v", d)
	}
	if _, err := f.startRunE(orgA, d, cleanPops()); !errors.Is(err, domain.ErrNoEffectiveRules) {
		t.Fatalf("run on unapproved rules: %v", err)
	}
	if _, err := f.s.ApproveRuleVersion(f.ctx, orgA, d.DefinitionID, 1, "test-operator", f.claim("Approve", d.DefinitionID)); !errors.Is(err, domain.ErrSelfApproval) {
		t.Fatalf("self-approval: %v", err)
	}
	approved := f.approve(orgA, d.DefinitionID, 1)
	if approved.Version != 1 || approved.RulesSHA256 == "" || len(approved.Populations) != 2 {
		t.Fatalf("after approval: %+v", approved)
	}
	if _, err := f.s.ApproveRuleVersion(f.ctx, orgA, d.DefinitionID, 1, "someone-else", f.claim("Approve", d.DefinitionID)); !errors.Is(err, domain.ErrRuleVersionApproved) {
		t.Fatalf("second approval: %v", err)
	}
	if _, err := f.s.ApproveRuleVersion(f.ctx, orgA, d.DefinitionID, 9, "someone-else", f.claim("Approve", d.DefinitionID)); !errors.Is(err, domain.ErrRuleVersionNotFound) {
		t.Fatalf("approve a missing version: %v", err)
	}
	run := f.startRun(orgA, approved, cleanPops())
	if run.RulesSHA256 != approved.RulesSHA256 || run.DefinitionVersion != 1 {
		t.Fatalf("run must pin the approved version's digest: %+v", run)
	}
}

// A draft version does not displace the version in effect, and a new version
// must be based on the latest one (optimistic check).
func TestRuleVersions_DraftsAndOptimisticCheck(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "optimistic", 0)
	stale := f.nextVersion(def, nil)

	tol := int64(99)
	draft, err := f.s.CreateRuleVersion(f.ctx, orgA, def.DefinitionID, f.nextVersion(def, func(r *domain.RulesInput) {
		r.Tolerance.Currencies["USD"] = domain.CurrencyTolerance{AbsoluteMinorUnits: &tol}
	}), "test-operator", f.claim("CreateRuleVersion", def.DefinitionID))
	if err != nil || draft.RuleVersion != 2 || draft.Status != "DRAFT" {
		t.Fatalf("draft: %+v %v", draft, err)
	}
	now, _ := f.s.GetDefinition(f.ctx, orgA, def.DefinitionID)
	if now.Version != 1 || now.LatestRuleVersion != 2 || *now.Tolerance.Currencies["USD"].AbsoluteMinorUnits != 0 {
		t.Fatalf("an unapproved draft must not take effect: %+v", now)
	}
	if _, err := f.s.CreateRuleVersion(f.ctx, orgA, def.DefinitionID, stale, "test-operator",
		f.claim("CreateRuleVersion", def.DefinitionID)); !errors.Is(err, domain.ErrRuleVersionConflict) {
		t.Fatalf("stale based_on_version: %v", err)
	}
	// The run started now still runs under v1.
	run := f.startRun(orgA, now, cleanPops())
	if run.DefinitionVersion != 1 {
		t.Fatalf("run pinned version %d", run.DefinitionVersion)
	}
}

// Effective dating: an approved version governs only from its effective date.
func TestRuleVersions_FutureVersionIsNotYetInEffect(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "dated", 0)
	req := f.nextVersion(def, func(r *domain.RulesInput) { r.CertificationProfile.PermitAcceptedRisk = true })
	req.EffectiveFrom = "2999-01-01"
	v, err := f.s.CreateRuleVersion(f.ctx, orgA, def.DefinitionID, req, "test-operator", f.claim("CreateRuleVersion", def.DefinitionID))
	if err != nil || v.EffectiveFrom != "2999-01-01" {
		t.Fatalf("future version: %+v %v", v, err)
	}
	after := f.approve(orgA, def.DefinitionID, v.RuleVersion)
	if after.Version != 1 || after.CertificationProfile.PermitAcceptedRisk {
		t.Fatalf("a future-dated version took effect early: %+v", after)
	}

	// A version dated before the one in effect could never take effect: refused.
	past := f.nextVersion(after, func(r *domain.RulesInput) { r.BalanceDimensions = []string{"entity"} })
	past.EffectiveFrom = "2026-01-01"
	if _, err := f.s.CreateRuleVersion(f.ctx, orgA, def.DefinitionID, past, "test-operator",
		f.claim("CreateRuleVersion", def.DefinitionID)); !errors.Is(err, domain.ErrBackdatedRuleVersion) {
		t.Fatalf("backdated version: %v", err)
	}
	// A version effective the same day as the one in effect takes over once
	// approved (same date: the higher version governs).
	today := f.nextVersion(after, func(r *domain.RulesInput) { r.BalanceDimensions = []string{"entity"} })
	tv, err := f.s.CreateRuleVersion(f.ctx, orgA, def.DefinitionID, today, "test-operator", f.claim("CreateRuleVersion", def.DefinitionID))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.approve(orgA, def.DefinitionID, tv.RuleVersion); got.Version != tv.RuleVersion || len(got.BalanceDimensions) != 1 {
		t.Fatalf("approved same-day version not in effect: %+v", got)
	}
}

// A definition from before migration 000008 continues its version numbering:
// its first rule version is based on the version its in-place edits reached.
func TestRuleVersions_LegacyNumberingContinues(t *testing.T) {
	f := newFixture(t)
	const id = "rdf_00000000-0000-0000-0000-0000000000aa"
	if _, err := f.admin.Exec(f.ctx, `INSERT INTO reconciliation_definitions (definition_id, tenant_id, legal_entity_id, name, version, created_by)
		VALUES ($1, $2, $3, 'pre-000008', 3, 'old-operator')`, id, orgA, entityA); err != nil {
		t.Fatal(err)
	}
	def, err := f.s.GetDefinition(f.ctx, orgA, id)
	if err != nil || def.Version != 0 || def.LatestRuleVersion != 3 {
		t.Fatalf("legacy definition: %+v %v", def, err)
	}
	next := f.publish(def, func(r *domain.RulesInput) {
		scale := 2
		in := defineRequest("x", 0).RulesInput
		*r = in
		r.AmountScale = &scale
	})
	if next.Version != 4 {
		t.Fatalf("first rule version of a v3 definition is %d, want 4", next.Version)
	}
}

// Rule versions are immutable evidence: the database refuses to edit or
// delete one, to approve one as its own creator, or to edit a definition row;
// a version whose stored rules no longer match its digest is refused on read.
func TestRuleVersions_DatabaseProtectsThem(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "sealed", 0)
	draft, err := f.s.CreateRuleVersion(f.ctx, orgA, def.DefinitionID, f.nextVersion(def, nil), "test-operator",
		f.claim("CreateRuleVersion", def.DefinitionID))
	if err != nil {
		t.Fatal(err)
	}
	run := f.startRun(orgA, def, cleanPops())

	for name, tc := range map[string]struct{ stmt, want string }{
		"approved rules edited": {`UPDATE reconciliation_rule_versions SET effective_from = '2000-01-01'
			WHERE definition_id = '` + def.DefinitionID + `' AND rule_version = 1`, "immutable"},
		"draft rules edited": {`UPDATE reconciliation_rule_versions SET rules = '{}' WHERE rule_version_id = '` + draft.RuleVersionID + `'`, "only the approval"},
		"self-approved":      {`UPDATE reconciliation_rule_versions SET approved_by = created_by WHERE rule_version_id = '` + draft.RuleVersionID + `'`, "check"},
		"version deleted":    {`DELETE FROM reconciliation_rule_versions WHERE rule_version_id = '` + draft.RuleVersionID + `'`, "cannot be deleted"},
		"definition renamed": {`UPDATE reconciliation_definitions SET name = 'other' WHERE definition_id = '` + def.DefinitionID + `'`, "immutable"},
		"run digest changed": {`UPDATE reconciliation_runs SET rules_sha256 = repeat('0', 64) WHERE run_id = '` + run.RunID + `'`, "pinned"},
	} {
		tx, err := f.app.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, setErr := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA)
		_, err = tx.Exec(f.ctx, tc.stmt)
		_ = tx.Rollback(f.ctx) // before any Fatal: a leaked transaction blocks the pool's Close
		if setErr != nil {
			t.Fatal(setErr)
		}
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
			t.Fatalf("%s: %v, want a refusal mentioning %q", name, err, tc.want)
		}
	}

	// Tampering below the trigger (as a superuser) is caught by the digest.
	if _, err := f.admin.Exec(f.ctx, `ALTER TABLE reconciliation_rule_versions DISABLE TRIGGER trigger_guard_rule_version;
		UPDATE reconciliation_rule_versions SET rules = jsonb_set(rules, '{amount_scale}', '6')
			WHERE definition_id = '`+def.DefinitionID+`' AND rule_version = 1;
		ALTER TABLE reconciliation_rule_versions ENABLE TRIGGER trigger_guard_rule_version;`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetDefinition(f.ctx, orgA, def.DefinitionID); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("tampered rules accepted: %v", err)
	}
}

// Rule versions are tenant-isolated.
func TestRuleVersions_TenantIsolated(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "isolated", 0)
	if _, err := f.s.ListRuleVersions(f.ctx, orgB, def.DefinitionID); !errors.Is(err, domain.ErrDefinitionNotFound) {
		t.Fatalf("tenant B listed tenant A's rule versions: %v", err)
	}
	if _, err := f.s.ApproveRuleVersion(f.ctx, orgB, def.DefinitionID, 1, "x", f.claim("Approve", def.DefinitionID)); !errors.Is(err, domain.ErrRuleVersionNotFound) {
		t.Fatalf("tenant B approved tenant A's version: %v", err)
	}
}
