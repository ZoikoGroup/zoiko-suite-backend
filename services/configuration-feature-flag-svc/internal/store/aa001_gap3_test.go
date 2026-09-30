package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── Audit 2026-09-23, gap 3: kill switch, rollback, emergency change ─────────
//
// §0: a bad value must be reversible. Each test drives one reversal mechanism
// through the store and reads the outcome back the way a consumer would —
// from the served snapshot, not the admin tables.

func resolveOne(t *testing.T, s *store.PgStore, key string, tenant *string) domain.ResolvedValue {
	t.Helper()
	res, err := s.Resolve(context.Background(), domain.ResolveParams{Environment: "staging", TenantID: tenant, Keys: []string{key}})
	if err != nil {
		t.Fatalf("resolve %s: %v", key, err)
	}
	for _, v := range res.Values {
		if v.Key == key {
			return v
		}
	}
	t.Fatalf("resolve %s: no answer", key)
	return domain.ResolvedValue{}
}

func expireNow(t *testing.T, pool *pgxpool.Pool, table, idCol, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE "+table+" SET expires_at = NOW() - INTERVAL '1 second' WHERE "+idCol+" = $1", id); err != nil {
		t.Fatalf("age %s: %v", table, err)
	}
}

// applyChange sets a material key's staging value the only way it may be set:
// an approved C2 change set.
func applyChange(t *testing.T, s *store.PgStore, key, value string) {
	t.Helper()
	c, err := s.CreateChange(context.Background(), domain.CreateChangeParams{
		ChangeClass: domain.ChangeClassC2, Environment: "staging",
		Parts:          []domain.ChangePart{{Kind: domain.PartKindConfig, Key: key, NewValue: []byte(value)}},
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("change %s: %v", key, err)
	}
	approveActivate(t, s, c.ChangeID)
}

func applyFlagChange(t *testing.T, s *store.PgStore, key string, enabled bool) {
	t.Helper()
	all := 100
	c, err := s.CreateChange(context.Background(), domain.CreateChangeParams{
		ChangeClass: domain.ChangeClassC2, Environment: "staging",
		Parts:          []domain.ChangePart{{Kind: domain.PartKindFlag, Key: key, NewEnabled: &enabled, RolloutPercentage: &all}},
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("flag change %s: %v", key, err)
	}
	approveActivate(t, s, c.ChangeID)
}

func upsertStaging(s *store.PgStore, key, value string) error {
	_, _, err := s.UpsertConfigEntry(context.Background(), domain.UpsertConfigEntryParams{
		Key: key, Value: []byte(value), Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	})
	return err
}

func emergency(t *testing.T, s *store.PgStore, key, value string) *domain.EmergencyChange {
	t.Helper()
	e, err := s.CreateEmergencyChange(context.Background(), domain.CreateEmergencyChangeParams{
		Key: key, Environment: "staging", NewValue: []byte(value), Reason: "sev1", IncidentID: "INC-42",
		ExpiresAt: time.Now().Add(time.Hour), CallerTenantID: testCallerTenant, ActorPrincipalID: "sre-1",
	})
	if err != nil {
		t.Fatalf("create emergency: %v", err)
	}
	return e
}

func approveActivate(t *testing.T, s *store.PgStore, changeID string) *domain.ConfigChange {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ApproveChange(ctx, changeID, domain.ChangeApproval{Approved: true, ByPrincipalID: "approver-1", ApprovedAt: time.Now()}, testCallerTenant); err != nil {
		t.Fatalf("approve %s: %v", changeID, err)
	}
	c, err := s.ActivateChange(ctx, changeID, testCallerTenant, "operator-1")
	if err != nil {
		t.Fatalf("activate %s: %v", changeID, err)
	}
	return c
}

// INV-16: the switch takes effect at once (it used to be recorded but never
// minted, so evaluation never saw it), and lifting it at expiry takes effect
// the same way.
func TestGap3_KillSwitchTakesEffectAndLiftsAtExpiry(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedFlag(t, pool, "checkout.new_ui")
	seedConfig(t, pool, "payroll.batch_size")
	if _, _, err := s.UpsertFeatureFlag(ctx, domain.UpsertFeatureFlagParams{
		Key: "checkout.new_ui", Enabled: true, RolloutPercentage: 100, Environment: "staging",
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	}); err != nil {
		t.Fatalf("flag write: %v", err)
	}
	eval := func() *domain.FlagEvaluation {
		t.Helper()
		ev, err := s.EvaluateFlag(ctx, domain.EvaluateFlagParams{Key: "checkout.new_ui", Environment: "staging", SubjectKey: "u-1"})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		return ev
	}
	if !eval().Enabled {
		t.Fatalf("baseline: flag should be on")
	}

	ks, err := s.CreateKillSwitch(ctx, domain.CreateKillSwitchParams{
		FlagKey: "checkout.new_ui", Environment: "staging", Reason: "checkout errors",
		SafeBehavior: domain.SafeBehaviorDisable, ExpiresAt: time.Now().Add(time.Hour),
		CallerTenantID: testCallerTenant, ActorPrincipalID: "sre-1",
	})
	if err != nil {
		t.Fatalf("kill switch: %v", err)
	}
	if ev := eval(); ev.Enabled || ev.Outcome != domain.OutcomeSafeDefault || ev.KillSwitch == nil {
		t.Fatalf("an active kill switch must force the safe behaviour at once, got enabled=%v outcome=%s", ev.Enabled, ev.Outcome)
	}

	expireNow(t, pool, "kill_switches", "kill_switch_id", ks.KillSwitchID)
	if res, err := s.SweepExpired(ctx, "staging"); err != nil || res.ExpiredKillSwitches != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if ev := eval(); !ev.Enabled || ev.KillSwitch != nil {
		t.Fatalf("an expired kill switch must lift at the sweep, got enabled=%v switch=%v", ev.Enabled, ev.KillSwitch)
	}

	if _, err := s.CreateKillSwitch(ctx, domain.CreateKillSwitchParams{
		FlagKey: "payroll.batch_size", Environment: "staging", Reason: "x",
		SafeBehavior: domain.SafeBehaviorDisable, ExpiresAt: time.Now().Add(time.Hour),
		CallerTenantID: testCallerTenant, ActorPrincipalID: "sre-1",
	}); !errors.Is(err, domain.ErrNotAFeatureFlag) {
		t.Errorf("kill switch on a config key: expected not_a_feature_flag, got %v", err)
	}
}

// INV-15: an activated break-glass value expires, is reverted exactly, and
// owes a retrospective that only a review reference closes.
func TestGap3_EmergencyChangeRevertsAtExpiryAndOwesRetrospective(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedMaterial(t, pool, "payroll.batch_size")
	applyChange(t, s, "payroll.batch_size", `100`)

	e := emergency(t, s, "payroll.batch_size", `500`)
	if _, err := s.ActivateEmergencyChange(ctx, e.EmergencyChangeID, testCallerTenant, "sre-1"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if v := resolveOne(t, s, "payroll.batch_size", nil); string(v.Value) != "500" {
		t.Fatalf("break-glass value must be served while active, got %s", v.Value)
	}
	if _, err := s.ActivateEmergencyChange(ctx, e.EmergencyChangeID, testCallerTenant, "sre-1"); !errors.Is(err, domain.ErrEmergencyNotActivatable) {
		t.Errorf("second activation: expected emergency_change_not_activatable, got %v", err)
	}
	if _, err := s.CloseEmergencyRetrospective(ctx, e.EmergencyChangeID, "review", testCallerTenant, "lead-1"); !errors.Is(err, domain.ErrRetrospectiveNotPending) {
		t.Errorf("retrospective before expiry: expected retrospective_not_pending, got %v", err)
	}

	expireNow(t, pool, "emergency_changes", "emergency_change_id", e.EmergencyChangeID)
	if res, err := s.SweepExpired(ctx, "staging"); err != nil || res.ExpiredEmergencyChanges != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if v := resolveOne(t, s, "payroll.batch_size", nil); string(v.Value) != "100" {
		t.Fatalf("expiry must restore the prior value, got %s", v.Value)
	}
	var status string
	var reverted bool
	var due *time.Time
	if err := pool.QueryRow(ctx, "SELECT status, reverted_to_prior, retrospective_due_at FROM emergency_changes WHERE emergency_change_id = $1",
		e.EmergencyChangeID).Scan(&status, &reverted, &due); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != domain.EmergencyStatusRetrospectivePending || !reverted || due == nil {
		t.Fatalf("expected RETROSPECTIVE_PENDING, reverted, due date set; got %s %v %v", status, reverted, due)
	}
	if _, err := s.ActivateEmergencyChange(ctx, e.EmergencyChangeID, testCallerTenant, "sre-1"); !errors.Is(err, domain.ErrEmergencyNotActivatable) {
		t.Errorf("reactivating an expired change: expected emergency_change_not_activatable, got %v", err)
	}

	if _, err := s.CloseEmergencyRetrospective(ctx, e.EmergencyChangeID, "  ", testCallerTenant, "lead-1"); !errors.Is(err, domain.ErrValueConstraintFailed) {
		t.Errorf("retrospective without a reference: expected value_constraint_failed, got %v", err)
	}
	closed, err := s.CloseEmergencyRetrospective(ctx, e.EmergencyChangeID, "PIR-2026-117", testCallerTenant, "lead-1")
	if err != nil || closed.Status != domain.EmergencyStatusClosed || closed.RetrospectiveReference == nil {
		t.Fatalf("close retrospective: %+v %v", closed, err)
	}
	if _, err := s.CloseEmergencyRetrospective(ctx, e.EmergencyChangeID, "again", testCallerTenant, "lead-1"); !errors.Is(err, domain.ErrRetrospectiveNotPending) {
		t.Errorf("closing twice: expected retrospective_not_pending, got %v", err)
	}
}

// INV-15 edge cases: a scope with no prior value returns to having none; a
// value superseded by a governed change after activation is left standing;
// a flag reverts like a config value.
func TestGap3_EmergencyReversionEdgeCases(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedMaterial(t, pool, "payroll.new_key")
	seedMaterial(t, pool, "payroll.batch_size")
	seedMaterialFlag(t, pool, "checkout.new_ui")
	applyChange(t, s, "payroll.batch_size", `100`)
	applyFlagChange(t, s, "checkout.new_ui", false)

	fresh := emergency(t, s, "payroll.new_key", `7`)
	superseded := emergency(t, s, "payroll.batch_size", `500`)
	flag := emergency(t, s, "checkout.new_ui", `{"enabled":true}`)
	for _, e := range []*domain.EmergencyChange{fresh, superseded, flag} {
		if _, err := s.ActivateEmergencyChange(ctx, e.EmergencyChangeID, testCallerTenant, "sre-1"); err != nil {
			t.Fatalf("activate %s: %v", e.Key, err)
		}
	}
	applyChange(t, s, "payroll.batch_size", `700`)
	for _, e := range []*domain.EmergencyChange{fresh, superseded, flag} {
		expireNow(t, pool, "emergency_changes", "emergency_change_id", e.EmergencyChangeID)
	}
	if res, err := s.SweepExpired(ctx, "staging"); err != nil || res.ExpiredEmergencyChanges != 3 {
		t.Fatalf("sweep: %+v %v", res, err)
	}

	if v := resolveOne(t, s, "payroll.new_key", nil); v.Reason != domain.ReasonNoValue {
		t.Errorf("no prior value: the scope must return to having none, got %+v", v)
	}
	if v := resolveOne(t, s, "payroll.batch_size", nil); string(v.Value) != "700" {
		t.Errorf("superseded: the later governed value must stand, got %s", v.Value)
	}
	ev, err := s.EvaluateFlag(ctx, domain.EvaluateFlagParams{Key: "checkout.new_ui", Environment: "staging", SubjectKey: "u-1"})
	if err != nil || ev.Enabled {
		t.Errorf("flag: expiry must restore enabled=false, got %+v %v", ev, err)
	}
}

// INV-17: rollback restores the before-state — values that existed come back,
// values the change created go away — through the governed path, and the
// rolled-back change is marked and cannot be rolled back twice.
func TestGap3_RollbackRestoresBeforeState(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	seedConfig(t, pool, "payroll.cutoff")
	if err := upsertStaging(s, "payroll.batch_size", `100`); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	tenant := testCallerTenant
	c, err := s.CreateChange(ctx, domain.CreateChangeParams{
		ChangeClass: domain.ChangeClassC2, Environment: "staging", ApprovalRequired: true,
		Parts: []domain.ChangePart{
			{Kind: domain.PartKindConfig, Key: "payroll.batch_size", Scope: domain.ChangePartScope{Environment: "staging"}, NewValue: []byte(`300`)},
			{Kind: domain.PartKindConfig, Key: "payroll.cutoff", Scope: domain.ChangePartScope{Environment: "staging", TenantID: &tenant}, NewValue: []byte(`25`)},
		},
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	target := approveActivate(t, s, c.ChangeID)
	if v := resolveOne(t, s, "payroll.cutoff", &tenant); string(v.Value) != "25" {
		t.Fatalf("change not applied: %+v", v)
	}

	rb, err := s.RollbackChange(ctx, target.ChangeID, testCallerTenant, "admin-2", "")
	if err != nil {
		t.Fatalf("propose rollback: %v", err)
	}
	if rb.Status != domain.ChangeStatusProposed || rb.RollbackChangeID == nil || *rb.RollbackChangeID != target.ChangeID ||
		rb.ChangeClass != target.ChangeClass || !rb.ApprovalRequired {
		t.Fatalf("rollback must be a PROPOSED change of the same class and approval, pointing at its target: %+v", rb)
	}
	if v := resolveOne(t, s, "payroll.batch_size", nil); string(v.Value) != "300" {
		t.Fatalf("proposing a rollback must not apply it, got %s", v.Value)
	}
	approveActivate(t, s, rb.ChangeID)

	if v := resolveOne(t, s, "payroll.batch_size", nil); string(v.Value) != "100" {
		t.Errorf("a value that existed before must come back, got %s", v.Value)
	}
	if v := resolveOne(t, s, "payroll.cutoff", &tenant); v.Reason != domain.ReasonNoValue {
		t.Errorf("a value the change created must be gone, got %+v", v)
	}
	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM config_changes WHERE change_id = $1", target.ChangeID).Scan(&status); err != nil || status != domain.ChangeStatusRolledBack {
		t.Errorf("target must be ROLLED_BACK, got %s %v", status, err)
	}
	if _, err := s.RollbackChange(ctx, target.ChangeID, testCallerTenant, "admin-2", ""); !errors.Is(err, domain.ErrRollbackTargetInvalid) {
		t.Errorf("rolling back twice: expected rollback_target_invalid, got %v", err)
	}
}

// TargetScope is what the handler authorizes by: a global target reports nil.
func TestGap3_TargetScopeReportsGlobalTargets(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedMaterial(t, pool, "payroll.batch_size")
	applyChange(t, s, "payroll.batch_size", `100`)
	e := emergency(t, s, "payroll.batch_size", `5`)
	if scope, err := s.TargetScope(ctx, "emergency", e.EmergencyChangeID); err != nil || scope != nil {
		t.Errorf("global emergency change must report a nil scope, got %v %v", scope, err)
	}
	if _, err := s.TargetScope(ctx, "change", "00000000-0000-0000-0000-000000000000"); !errors.Is(err, domain.ErrChangeNotFound) {
		t.Errorf("unknown target: expected change_not_found, got %v", err)
	}
}
