package store_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// The governed lifecycle as the real runtime role. Every other store test
// connects as a superuser, which ignores row-level security — so a policy that
// refuses the service's own writes is invisible to them. The change and
// emergency paths run under the app.ops_sweep escape and write TENANT-scoped
// values, events and snapshots; this drives each of them through a
// NOSUPERUSER NOBYPASSRLS connection.
func TestAppRole_GovernedLifecycleWritesTenantScopedValues(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	seedConfig(t, admin, "payroll.cutoff")
	seedMaterial(t, admin, "payroll.approval_limit")
	s := store.New(appRolePool(t, admin), zap.NewNop())
	tenant := testCallerTenant

	// A tenant-scoped value through an approved change set.
	c, err := s.CreateChange(ctx, domain.CreateChangeParams{
		ChangeClass: domain.ChangeClassC1, Environment: "staging",
		Parts: []domain.ChangePart{{Kind: domain.PartKindConfig, Key: "payroll.cutoff",
			Scope: domain.ChangePartScope{Environment: "staging", TenantID: &tenant}, NewValue: []byte(`25`)}},
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("create change as app role: %v", err)
	}
	approveActivate(t, s, c.ChangeID)
	if v := resolveOne(t, s, "payroll.cutoff", &tenant); string(v.Value) != "25" {
		t.Fatalf("tenant change not served: %+v", v)
	}

	// A tenant-scoped break-glass value, activated and then reverted.
	e, err := s.CreateEmergencyChange(ctx, domain.CreateEmergencyChangeParams{
		Key: "payroll.approval_limit", Environment: "staging", TenantID: &tenant, NewValue: []byte(`9000`),
		Reason: "sev1", IncidentID: "INC-1", ExpiresAt: time.Now().Add(time.Hour),
		CallerTenantID: testCallerTenant, ActorPrincipalID: "sre-1",
	})
	if err != nil {
		t.Fatalf("create emergency as app role: %v", err)
	}
	if _, err := s.ActivateEmergencyChange(ctx, e.EmergencyChangeID, testCallerTenant, "sre-1"); err != nil {
		t.Fatalf("activate emergency as app role: %v", err)
	}
	if v := resolveOne(t, s, "payroll.approval_limit", &tenant); string(v.Value) != "9000" {
		t.Fatalf("tenant emergency value not served: %+v", v)
	}
	expireNow(t, admin, "emergency_changes", "emergency_change_id", e.EmergencyChangeID)
	if res, err := s.SweepExpired(ctx, "staging"); err != nil || res.ExpiredEmergencyChanges != 1 {
		t.Fatalf("sweep as app role: %+v %v", res, err)
	}
	if v := resolveOne(t, s, "payroll.approval_limit", &tenant); v.Reason != domain.ReasonNoValue {
		t.Fatalf("tenant emergency value must be reverted: %+v", v)
	}
}

// Publishing runs under the definition_admin escape, but the
// config.version.published row it enqueues is owned by the CALLER's tenant —
// the same rule every request-path event follows: the outbox policy admits a
// request-path row only for the session tenant or a named background escape,
// and definition_admin is neither. The enqueue used to name nobody, so the
// real role's INSERT was refused (42501) and — sharing the publish's
// transaction — failed the whole publish with a 503. SUPERUSER test
// connections cannot see this class of defect at all: RLS is off for them,
// which is why this runs as the real role.
func TestAppRole_PublishDefinitionOwnsTheOutboxRow(t *testing.T) {
	ctx := context.Background()
	admin := openTestPool(t)
	s := store.New(appRolePool(t, admin), zap.NewNop())

	def, err := s.CreateDefinition(ctx, domain.CreateDefinitionParams{
		Key: "payroll.publish_probe", Owner: "platform", ValueType: "INTEGER",
		SafetyClass: "S1", AllowedScopes: []string{"ENVIRONMENT", "TENANT"},
		FallbackPolicy: "SAFE_DEFAULT", Sensitivity: "INTERNAL",
		ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("declare as app role: %v", err)
	}
	v, err := s.PublishDefinition(ctx, domain.PublishDefinitionParams{
		DefinitionID: def.DefinitionID, Lifecycle: domain.LifecyclePublished,
		ActorPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	})
	if err != nil {
		t.Fatalf("publish as app role: %v", err)
	}
	if v == nil || v.Version != 1 {
		t.Fatalf("publish must return the first version, got %+v", v)
	}
	// The admin pool sees every RLS-tenanted row, so this asserts the row's
	// OWNER, not merely that some row exists.
	var owner, eventType string
	if err := admin.QueryRow(ctx, `
		SELECT tenant_id, event_type FROM event_outbox
		WHERE aggregate_key = $1`, v.VersionID).Scan(&owner, &eventType); err != nil {
		t.Fatalf("the publish must enqueue its event: %v", err)
	}
	if owner != testCallerTenant || eventType != "config.version.published" {
		t.Fatalf("outbox row must be owned by the caller: got tenant=%q type=%q", owner, eventType)
	}
}
