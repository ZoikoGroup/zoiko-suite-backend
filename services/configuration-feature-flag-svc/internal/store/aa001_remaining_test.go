package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── Audit 2026-09-23, the remaining scored items ─────────────────────────────
//
// Beyond the six top gaps: the §10.1 API obligations and §3 invariants still
// open after them. One test per item, each driving the negative path.

func createDef(t *testing.T, s *store.PgStore, p domain.CreateDefinitionParams) *domain.ConfigDefinition {
	t.Helper()
	ctx := context.Background()
	if p.Owner == "" {
		p.Owner = "payroll-svc"
	}
	if p.FallbackPolicy == "" {
		p.FallbackPolicy = domain.FallbackBlock
	}
	if p.Sensitivity == "" {
		p.Sensitivity = domain.SensitivityInternal
	}
	if len(p.AllowedScopes) == 0 {
		p.AllowedScopes = []string{domain.ScopeEnvironment, domain.ScopeTenant}
	}
	p.ActorPrincipalID = "admin-1"
	def, err := s.CreateDefinition(ctx, p)
	if err != nil {
		t.Fatalf("create %s: %v", p.Key, err)
	}
	if _, err := s.PublishDefinition(ctx, domain.PublishDefinitionParams{
		DefinitionID: def.DefinitionID, Lifecycle: domain.LifecyclePublished,
		ActorPrincipalID: "admin-1", ApprovalReference: "CAB-1",
	}); err != nil {
		t.Fatalf("publish %s: %v", p.Key, err)
	}
	return def
}

func stagingChange(class string, parts ...domain.ChangePart) domain.CreateChangeParams {
	return domain.CreateChangeParams{
		ChangeClass: class, Environment: "staging", Parts: parts,
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	}
}

func cfgPart(key, value string) domain.ChangePart {
	return domain.ChangePart{Kind: domain.PartKindConfig, Key: key, NewValue: []byte(value)}
}

// §10.1 POST /changes "validation" + "change classification": a change set is
// validated when proposed, its class must cover the keys it touches, and
// C2/C3 are always approval-gated.
func TestRemaining_ChangeSetValidatedAndClassifiedAtCreation(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	seedMaterial(t, pool, "payroll.approval_limit")

	if _, err := s.CreateChange(ctx, stagingChange(domain.ChangeClassC1, cfgPart("payroll.batch_size", `"lots"`))); !errors.Is(err, domain.ErrTypeMismatch) {
		t.Errorf("an invalid value must be refused when proposed, got %v", err)
	}
	if _, err := s.CreateChange(ctx, stagingChange(domain.ChangeClassC1, cfgPart("payroll.approval_limit", `5000`))); !errors.Is(err, domain.ErrChangeClassInsufficient) {
		t.Errorf("an S2 key in a C1 change: expected change_class_insufficient, got %v", err)
	}
	c, err := s.CreateChange(ctx, stagingChange(domain.ChangeClassC2, cfgPart("payroll.approval_limit", `5000`)))
	if err != nil {
		t.Fatalf("C2 change for an S2 key: %v", err)
	}
	if !c.ApprovalRequired {
		t.Errorf("a C2 change must be approval-gated even when the caller did not ask")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM config_changes WHERE status = 'PROPOSED'`); n != 1 {
		t.Errorf("refused changes must not be recorded; %d proposed", n)
	}
}

// §10.1 activate "effective time": a planned change does not apply early.
func TestRemaining_PlannedChangeDoesNotActivateEarly(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	future := time.Now().Add(time.Hour)
	p := stagingChange(domain.ChangeClassC1, cfgPart("payroll.batch_size", `300`))
	p.PlannedEffectiveAt = &future
	c, err := s.CreateChange(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ApproveChange(ctx, c.ChangeID, domain.ChangeApproval{Approved: true, ByPrincipalID: "approver-1", ApprovedAt: time.Now()}, testCallerTenant); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.ActivateChange(ctx, c.ChangeID, testCallerTenant, "operator-1"); !errors.Is(err, domain.ErrChangeNotYetEffective) {
		t.Fatalf("expected change_not_yet_effective, got %v", err)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM config_entries WHERE key = 'payroll.batch_size'`); n != 0 {
		t.Errorf("nothing may be applied before the effective time, found %d rows", n)
	}
}

// §10.1 emergency "restricted keys": break-glass is for material keys only.
func TestRemaining_BreakGlassRestrictedToMaterialKeys(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "ui.page_size")
	_, err := s.CreateEmergencyChange(context.Background(), domain.CreateEmergencyChangeParams{
		Key: "ui.page_size", Environment: "staging", NewValue: []byte(`50`), Reason: "r", IncidentID: "INC-1",
		ExpiresAt: time.Now().Add(time.Hour), CallerTenantID: testCallerTenant, ActorPrincipalID: "sre-1",
	})
	if !errors.Is(err, domain.ErrEmergencyScopeDenied) {
		t.Fatalf("break-glass on an S1 key: expected emergency_scope_denied, got %v", err)
	}
}

// §10.1 publish "approval binding for S2/S3".
func TestRemaining_MaterialPublishNeedsApproval(t *testing.T) {
	ctx := context.Background()
	s := store.New(openTestPool(t), zap.NewNop())
	def, err := s.CreateDefinition(ctx, domain.CreateDefinitionParams{
		Key: "tax.rounding", Owner: "tax-svc", ValueType: domain.ValueTypeString, SafetyClass: domain.SafetyS3,
		AllowedScopes: []string{domain.ScopeEnvironment}, FallbackPolicy: domain.FallbackBlock,
		Sensitivity: domain.SensitivityInternal, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.PublishDefinition(ctx, domain.PublishDefinitionParams{
		DefinitionID: def.DefinitionID, Lifecycle: domain.LifecyclePublished, ActorPrincipalID: "admin-1",
	}); !errors.Is(err, domain.ErrApprovalReferenceRequired) {
		t.Fatalf("S3 publish without approval: expected approval_reference_required, got %v", err)
	}
	v, err := s.PublishDefinition(ctx, domain.PublishDefinitionParams{
		DefinitionID: def.DefinitionID, Lifecycle: domain.LifecyclePublished, ActorPrincipalID: "admin-1",
		ApprovalReference: "CAB-2026-900",
	})
	if err != nil || v.ApprovalReference == nil || *v.ApprovalReference != "CAB-2026-900" {
		t.Fatalf("the published version must carry its approval: %+v %v", v, err)
	}
}

// §10.1 overrides "impact/change classification": an S2/S3 key never changes
// by a direct write.
func TestRemaining_MaterialKeysRefuseDirectWrites(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedMaterial(t, pool, "payroll.approval_limit")
	seedMaterialFlag(t, pool, "payroll.auto_release")
	if err := upsertStaging(s, "payroll.approval_limit", `5000`); !errors.Is(err, domain.ErrMaterialKeyRequiresChange) {
		t.Errorf("POST /v1/config: expected material_key_requires_change, got %v", err)
	}
	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "payroll.auto_release", Enabled: true, RolloutPercentage: 100, Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); !errors.Is(err, domain.ErrMaterialKeyRequiresChange) {
		t.Errorf("POST /v1/flags: expected material_key_requires_change, got %v", err)
	}
	if _, err := s.ActivateOverride(ctx, domain.ActivateOverrideParams{
		Key: "payroll.approval_limit", Layer: domain.ScopeEnvironment, Environment: "staging",
		Value: []byte(`5000`), CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	}); !errors.Is(err, domain.ErrMaterialKeyRequiresChange) {
		t.Errorf("PUT overrides: expected material_key_requires_change, got %v", err)
	}
}

func planFlag(t *testing.T, s *store.PgStore, pool *pgxpool.Pool, key, strategy, rules string) error {
	t.Helper()
	_, err := s.CreateReleasePlan(context.Background(), domain.CreateReleasePlanParams{
		FlagKey: key, Environment: "staging", Strategy: strategy, TargetingRules: json.RawMessage(rules),
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	return err
}

// NP-08 / INV-02 (CFG side): plan eligibility reads the gateway's plan, never
// the request body.
func TestRemaining_PlanEligibilityIgnoresClientAssertedPlan(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "reports.advanced")
	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "reports.advanced", Enabled: true, RolloutPercentage: 100, Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("flag: %v", err)
	}
	if err := planFlag(t, s, pool, "reports.advanced", domain.StrategyAllOrNothing,
		`{"eligibility":{"plans":["enterprise"]},"percentage":100}`); err != nil {
		t.Fatalf("plan: %v", err)
	}
	eval := func(trusted string) (*domain.FlagEvaluation, error) {
		return s.EvaluateFlag(ctx, domain.EvaluateFlagParams{
			Key: "reports.advanced", Environment: "staging", SubjectKey: "u-1",
			Context: map[string]any{"plan": "enterprise"}, TrustedPlan: trusted,
		})
	}
	if _, err := eval(""); !errors.Is(err, domain.ErrContextIncomplete) {
		t.Errorf("a body-asserted plan with no trusted plan: expected context_incomplete, got %v", err)
	}
	if ev, err := eval("starter"); err != nil || ev.Enabled {
		t.Errorf("a starter tenant claiming enterprise in the body must not be enabled: %+v %v", ev, err)
	}
	if ev, err := eval("enterprise"); err != nil || !ev.Enabled {
		t.Errorf("a gateway-verified enterprise plan must be enabled: %+v %v", ev, err)
	}
}

// staleSnapshot appends an imprint of the current content whose freshness
// deadline is `deadline` — snapshots are immutable, so staleness is simulated
// by issuing one, never by editing one.
func staleSnapshot(t *testing.T, pool *pgxpool.Pool, deadline time.Time) {
	t.Helper()
	ctx := context.Background()
	var epoch int64
	if err := pool.QueryRow(ctx, `UPDATE config_snapshot_epochs SET current_epoch = current_epoch + 1
		WHERE environment = 'staging' RETURNING current_epoch`).Scan(&epoch); err != nil {
		t.Fatalf("bump epoch: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO config_snapshots (environment, epoch, digest, content, freshness_deadline, created_by_principal_id)
		SELECT 'staging', $1, digest, content, $2, 'test:stale' FROM config_snapshots
		WHERE environment = 'staging' ORDER BY epoch DESC LIMIT 1`, epoch, deadline); err != nil {
		t.Fatalf("issue stale snapshot: %v", err)
	}
}

// INV-13: a stale snapshot never silently serves a protected value; the sweep
// refreshes a snapshot before it goes stale.
func TestRemaining_StaleSnapshotWithholdsMaterialKeys(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedMaterial(t, pool, "payroll.approval_limit")
	seedConfig(t, pool, "ui.page_size")
	seedMaterialFlag(t, pool, "payroll.auto_release")
	applyChange(t, s, "payroll.approval_limit", `5000`)
	applyFlagChange(t, s, "payroll.auto_release", true)
	if err := upsertStaging(s, "ui.page_size", `50`); err != nil {
		t.Fatalf("ui: %v", err)
	}

	staleSnapshot(t, pool, time.Now().Add(-time.Minute))
	res, err := s.Resolve(ctx, domain.ResolveParams{Environment: "staging", Keys: []string{"payroll.approval_limit", "ui.page_size"}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Stale {
		t.Errorf("the response must say the snapshot is stale")
	}
	for _, v := range res.Values {
		switch v.Key {
		case "payroll.approval_limit":
			if v.Outcome != domain.OutcomeBlocked || v.Reason != domain.ReasonStaleSnapshot || len(v.Value) != 0 {
				t.Errorf("a material value must be withheld from a stale snapshot, got %+v", v)
			}
		case "ui.page_size":
			if string(v.Value) != "50" {
				t.Errorf("an S1 value is still served (marked stale), got %+v", v)
			}
		}
	}
	if _, err := s.EvaluateFlag(ctx, domain.EvaluateFlagParams{Key: "payroll.auto_release", Environment: "staging", SubjectKey: "u"}); !errors.Is(err, domain.ErrSnapshotStale) {
		t.Errorf("a protected flag on a stale snapshot: expected snapshot_stale, got %v", err)
	}

	// The sweep re-mints once the newest snapshot is inside the refresh margin.
	staleSnapshot(t, pool, time.Now().Add(time.Hour))
	before := count(t, pool, `SELECT COUNT(*) FROM config_snapshots WHERE environment = 'staging'`)
	r, err := s.SweepExpired(ctx, "staging")
	if err != nil || !r.RefreshedSnapshot {
		t.Fatalf("sweep must refresh a snapshot near its deadline: %+v %v", r, err)
	}
	if after := count(t, pool, `SELECT COUNT(*) FROM config_snapshots WHERE environment = 'staging'`); after != before+1 {
		t.Errorf("expected one refreshed snapshot, had %d now %d", before, after)
	}
	if v := resolveOne(t, s, "payroll.approval_limit", nil); string(v.Value) != "5000" {
		t.Errorf("after refresh the material value is served again, got %+v", v)
	}
}

// INV-18: targeting attributes outside the rule language are refused.
func TestRemaining_UnknownTargetingAttributeRefused(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "checkout.new_ui")
	err := planFlag(t, s, pool, "checkout.new_ui", domain.StrategyPercentage,
		`{"eligibility":{"gender":["female"]},"percentage":50}`)
	if !errors.Is(err, domain.ErrTargetingNotPermitted) {
		t.Fatalf("expected targeting_not_permitted, got %v", err)
	}
}

// INV-19: no randomization for authority-bearing flags.
func TestRemaining_NoRandomizationForMaterialFlags(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedMaterialFlag(t, pool, "payroll.new_calc")
	if err := planFlag(t, s, pool, "payroll.new_calc", domain.StrategyPercentage, `{"percentage":50}`); !errors.Is(err, domain.ErrTargetingNotPermitted) {
		t.Errorf("percentage plan on an S2 flag: expected targeting_not_permitted, got %v", err)
	}
	half := 50
	on := true
	_, err := s.CreateChange(ctx, stagingChange(domain.ChangeClassC2,
		domain.ChangePart{Kind: domain.PartKindFlag, Key: "payroll.new_calc", NewEnabled: &on, RolloutPercentage: &half}))
	if !errors.Is(err, domain.ErrTargetingNotPermitted) {
		t.Errorf("50%% rollout of an S2 flag: expected targeting_not_permitted, got %v", err)
	}
	exp := domain.FlagClassExperiment
	deadline := time.Now().Add(24 * time.Hour)
	if _, err := s.CreateDefinition(ctx, domain.CreateDefinitionParams{
		Key: "payroll.ab", Owner: "payroll-svc", ValueType: domain.ValueTypeBoolean, SafetyClass: domain.SafetyS2,
		AllowedScopes: []string{domain.ScopeEnvironment}, FallbackPolicy: domain.FallbackBlock,
		Sensitivity: domain.SensitivityInternal, FlagClass: &exp, RetirementDeadline: &deadline, ActorPrincipalID: "admin-1",
	}); !errors.Is(err, domain.ErrTargetingNotPermitted) {
		t.Errorf("an S2 experiment flag: expected targeting_not_permitted, got %v", err)
	}
}

// INV-21 / NP-20: a retired key cannot be written again until its removal is
// verified with consumer-scan evidence.
func TestRemaining_RetiredKeyTombstonedUntilVerifiedRemoval(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "legacy.checkout")
	write := func() error {
		_, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
			Key: "legacy.checkout", Enabled: true, RolloutPercentage: 100, Environment: "staging",
			CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
		})
		return err
	}
	if err := write(); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if _, err := s.RetireFlag(ctx, domain.RetireFlagParams{
		Key: "legacy.checkout", Environment: "staging", FinalEnabled: true, FinalRollout: 100,
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := write(); !errors.Is(err, domain.ErrFlagKeyRetired) {
		t.Errorf("writing a retired key: expected flag_key_retired, got %v", err)
	}
	if _, err := s.MarkFlagRemoved(ctx, store.MarkFlagRemovedParams{
		Key: "legacy.checkout", Environment: "staging", ConsumerScanEvidence: json.RawMessage(`{}`),
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	}); !errors.Is(err, domain.ErrValueConstraintFailed) {
		t.Errorf("removal without evidence: expected value_constraint_failed, got %v", err)
	}
	if _, err := s.MarkFlagRemoved(ctx, store.MarkFlagRemovedParams{
		Key: "legacy.checkout", Environment: "staging",
		ConsumerScanEvidence: json.RawMessage(`{"scan":"repo-wide","references":0,"scanned_at":"2026-09-29"}`),
		CallerTenantID:       testCallerTenant, ActorPrincipalID: "admin-1",
	}); err != nil {
		t.Fatalf("verified removal: %v", err)
	}
	if err := write(); err != nil {
		t.Errorf("a removed key is reusable: %v", err)
	}
	if _, err := s.MarkFlagRemoved(ctx, store.MarkFlagRemovedParams{
		Key: "never.retired", Environment: "staging", ConsumerScanEvidence: json.RawMessage(`{"scan":"x"}`),
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	}); !errors.Is(err, domain.ErrFlagNotRetired) {
		t.Errorf("removing a key never retired: expected flag_not_retired, got %v", err)
	}
}

// INV-28: a declared safe default is served as SAFE_DEFAULT when no value is set.
func TestRemaining_DeclaredSafeDefaultServed(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	createDef(t, s, domain.CreateDefinitionParams{
		Key: "payroll.retry_limit", ValueType: domain.ValueTypeInteger, SafetyClass: domain.SafetyS1,
		DefaultValue: json.RawMessage(`3`), FallbackPolicy: domain.FallbackSafeDefault,
	})
	seedConfig(t, pool, "payroll.batch_size")
	if err := upsertStaging(s, "payroll.batch_size", `100`); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	v := resolveOne(t, s, "payroll.retry_limit", nil)
	if v.Outcome != domain.OutcomeSafeDefault || v.Reason != domain.ReasonDefault || string(v.Value) != "3" {
		t.Fatalf("expected SAFE_DEFAULT 3, got %+v", v)
	}
}

// NP-25: applied is not verified. A change stays APPLYING while an attesting
// runtime is behind, and verifies when the fleet catches up.
func TestRemaining_ChangeVerifiesOnlyAfterFleetConverges(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	if err := upsertStaging(s, "payroll.batch_size", `100`); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	id1, e1, d1 := currentSnap(t, pool, "staging")
	attest(t, s, "rt-1", "a1", "staging", &id1, e1, d1)

	c, err := s.CreateChange(ctx, stagingChange(domain.ChangeClassC1, cfgPart("payroll.batch_size", `200`)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	act := approveActivate(t, s, c.ChangeID)
	if act.Status != domain.ChangeStatusApplying || act.VerifiedAt != nil {
		t.Fatalf("with a runtime still on the old snapshot the change must be APPLYING, got %s", act.Status)
	}
	if r, err := s.SweepExpired(ctx, "staging"); err != nil || r.VerifiedChanges != 0 {
		t.Fatalf("nothing converged yet: %+v %v", r, err)
	}

	id2, e2, d2 := currentSnap(t, pool, "staging")
	attest(t, s, "rt-1", "a2", "staging", &id2, e2, d2)
	if r, err := s.SweepExpired(ctx, "staging"); err != nil || r.VerifiedChanges != 1 {
		t.Fatalf("the converged change must verify: %+v %v", r, err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM config_changes WHERE change_id = $1`, c.ChangeID).Scan(&status); err != nil || status != domain.ChangeStatusVerified {
		t.Fatalf("expected VERIFIED, got %s %v", status, err)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'config.change.verified' AND aggregate_key = $1`, c.ChangeID); n != 1 {
		t.Errorf("config.change.verified must be emitted once, at verification; got %d", n)
	}
}

// INV-26: a key restricted to a residency is delivered only to callers in it.
func TestRemaining_ResidencyEvaluatedBeforeDelivery(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	createDef(t, s, domain.CreateDefinitionParams{
		Key: "eu.dpo_contact", ValueType: domain.ValueTypeString, SafetyClass: domain.SafetyS1,
		AllowedRegions: []string{"EU"},
	})
	if err := upsertStaging(s, "eu.dpo_contact", `"dpo@example.eu"`); err != nil {
		t.Fatalf("write: %v", err)
	}
	for region, wantServed := range map[string]bool{"EU": true, "US": false, "": false} {
		res, err := s.Resolve(ctx, domain.ResolveParams{Environment: "staging", Keys: []string{"eu.dpo_contact"}, Region: region})
		if err != nil {
			t.Fatalf("resolve %q: %v", region, err)
		}
		v := res.Values[0]
		served := v.Outcome == domain.OutcomeValue && len(v.Value) > 0
		if served != wantServed || (!wantServed && v.Reason != domain.ReasonResidency) {
			t.Errorf("region %q: served=%v want %v (%+v)", region, served, wantServed, v)
		}
	}
}
