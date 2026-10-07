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
