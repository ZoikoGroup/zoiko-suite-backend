package store_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── INV-07: five precedence layers, schema-defined and deterministic ─────────

func layered(t *testing.T, s *store.PgStore, layer, scopeID, actor, value string) error {
	t.Helper()
	sid := scopeID
	_, err := s.ActivateOverride(context.Background(), domain.ActivateOverrideParams{
		Key: "ui.page_size", Layer: layer, Environment: "staging", ScopeID: &sid,
		Value: []byte(value), CallerTenantID: testCallerTenant, ActorPrincipalID: actor,
	})
	return err
}

func TestLayers_PrecedenceAcrossAllFive(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	createDef(t, s, domain.CreateDefinitionParams{
		Key: "ui.page_size", ValueType: domain.ValueTypeInteger, SafetyClass: domain.SafetyS0,
		AllowedScopes: []string{domain.ScopeEnvironment, domain.LayerService, domain.ScopeTenant,
			domain.LayerOrgUnit, domain.LayerUserPreference},
	})
	tenant := testCallerTenant
	if err := upsertStaging(s, "ui.page_size", `10`); err != nil {
		t.Fatalf("environment: %v", err)
	}
	if err := layered(t, s, domain.LayerService, "reports-svc", "admin-1", `20`); err != nil {
		t.Fatalf("service: %v", err)
	}
	if _, err := s.ActivateOverride(ctx, domain.ActivateOverrideParams{
		Key: "ui.page_size", Layer: domain.ScopeTenant, Environment: "staging", ScopeID: &tenant,
		Value: []byte(`30`), CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	}); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if err := layered(t, s, domain.LayerOrgUnit, "ou-finance", "admin-1", `40`); err != nil {
		t.Fatalf("org unit: %v", err)
	}
	if err := layered(t, s, domain.LayerUserPreference, "user-7", "user-7", `50`); err != nil {
		t.Fatalf("user preference: %v", err)
	}

	resolve := func(p domain.ResolveParams) domain.ResolvedValue {
		t.Helper()
		p.Environment, p.Keys = "staging", []string{"ui.page_size"}
		res, err := s.Resolve(ctx, p)
		if err != nil || len(res.Values) != 1 {
			t.Fatalf("resolve: %+v %v", res, err)
		}
		return res.Values[0]
	}
	cases := []struct {
		name      string
		p         domain.ResolveParams
		value     string
		wantLayer string
	}{
		{"no context", domain.ResolveParams{}, "10", domain.ScopeEnvironment},
		{"service only", domain.ResolveParams{Service: "reports-svc"}, "20", domain.LayerService},
		{"tenant beats service", domain.ResolveParams{Service: "reports-svc", TenantID: &tenant}, "30", domain.ScopeTenant},
		{"org unit beats tenant", domain.ResolveParams{TenantID: &tenant, OrgUnit: "ou-finance"}, "40", domain.LayerOrgUnit},
		{"user preference beats all", domain.ResolveParams{TenantID: &tenant, OrgUnit: "ou-finance", Subject: "user-7", Service: "reports-svc"}, "50", domain.LayerUserPreference},
		{"another user falls through", domain.ResolveParams{TenantID: &tenant, OrgUnit: "ou-finance", Subject: "user-8"}, "40", domain.LayerOrgUnit},
		{"another service falls through", domain.ResolveParams{Service: "billing-svc"}, "10", domain.ScopeEnvironment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := resolve(tc.p)
			if string(v.Value) != tc.value || v.Layer != tc.wantLayer {
				t.Errorf("got %s from %s, want %s from %s", v.Value, v.Layer, tc.value, tc.wantLayer)
			}
		})
	}

	// The legacy list endpoint still shows only the two base layers.
	list, err := s.ListCurrentConfigEntries(ctx, store.ListFilter{Environment: "staging"})
	if err != nil || len(list) != 2 {
		t.Errorf("legacy list must hold the environment and tenant rows only, got %d %v", len(list), err)
	}
}

func TestLayers_WriteRules(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	createDef(t, s, domain.CreateDefinitionParams{
		Key: "ui.page_size", ValueType: domain.ValueTypeInteger, SafetyClass: domain.SafetyS0,
		AllowedScopes: []string{domain.ScopeEnvironment, domain.LayerUserPreference},
	})
	if err := layered(t, s, domain.LayerUserPreference, "user-7", "user-8", `50`); !errors.Is(err, domain.ErrScopeNotAllowed) {
		t.Errorf("setting another user's preference: expected scope_not_allowed, got %v", err)
	}
	if err := layered(t, s, domain.LayerOrgUnit, "ou-1", "admin-1", `50`); !errors.Is(err, domain.ErrScopeNotAllowed) {
		t.Errorf("a layer the declaration does not admit: expected scope_not_allowed, got %v", err)
	}
	if err := layered(t, s, domain.LayerUserPreference, "", "", `50`); !errors.Is(err, domain.ErrScopeNotAllowed) {
		t.Errorf("an extended layer with no scope id: expected scope_not_allowed, got %v", err)
	}
	if err := layered(t, s, domain.LayerUserPreference, "user-7", "user-7", `"fifty"`); !errors.Is(err, domain.ErrTypeMismatch) {
		t.Errorf("a layered value is type-checked like any other, got %v", err)
	}
}

// A material key reaches the extended layers through a change set, and a
// rollback restores them like any other scope.
func TestLayers_ChangeSetAndRollback(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	createDef(t, s, domain.CreateDefinitionParams{
		Key: "ap.approval_limit", ValueType: domain.ValueTypeDecimal, SafetyClass: domain.SafetyS2,
		AllowedScopes: []string{domain.ScopeEnvironment, domain.LayerOrgUnit},
	})
	tenant := testCallerTenant
	orgPart := domain.ChangePart{Kind: domain.PartKindConfig, Key: "ap.approval_limit", NewValue: []byte(`25000`),
		Scope: domain.ChangePartScope{Environment: "staging", TenantID: &tenant, Layer: domain.LayerOrgUnit, ScopeID: "ou-finance"}}
	c, err := s.CreateChange(ctx, stagingChange(domain.ChangeClassC2, orgPart))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	target := approveActivate(t, s, c.ChangeID)
	get := func() domain.ResolvedValue {
		res, err := s.Resolve(ctx, domain.ResolveParams{Environment: "staging", TenantID: &tenant, OrgUnit: "ou-finance", Keys: []string{"ap.approval_limit"}})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return res.Values[0]
	}
	if v := get(); string(v.Value) != "25000" || v.Layer != domain.LayerOrgUnit {
		t.Fatalf("org-unit value not served: %+v", v)
	}
	rb, err := s.RollbackChange(ctx, target.ChangeID, testCallerTenant, "admin-2", "")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	approveActivate(t, s, rb.ChangeID)
	if v := get(); v.Reason != domain.ReasonNoValue {
		t.Fatalf("rollback must end the org-unit value it created, got %+v", v)
	}

	// SERVICE is tenantless; a SERVICE part naming a tenant is refused.
	svc := domain.ChangePart{Kind: domain.PartKindConfig, Key: "ap.approval_limit", NewValue: []byte(`1`),
		Scope: domain.ChangePartScope{Environment: "staging", TenantID: &tenant, Layer: domain.LayerService, ScopeID: "ap-svc"}}
	if _, err := s.CreateChange(ctx, stagingChange(domain.ChangeClassC2, svc)); !errors.Is(err, domain.ErrScopeNotAllowed) {
		t.Errorf("a tenant-scoped SERVICE part: expected scope_not_allowed, got %v", err)
	}
}

// expected_before_hash: a change approved against one value does not
// overwrite another.
func TestExpectedBeforeHashIsEnforced(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	if err := upsertStaging(s, "payroll.batch_size", `100`); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	sum := md5.Sum([]byte(`100`))
	good := hex.EncodeToString(sum[:])
	stale := "0123456789abcdef0123456789abcdef"

	propose := func(hash string) string {
		p := cfgPart("payroll.batch_size", `200`)
		p.ExpectedBeforeHash = &hash
		c, err := s.CreateChange(ctx, stagingChange(domain.ChangeClassC1, p))
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.ApproveChange(ctx, c.ChangeID, domain.ChangeApproval{Approved: true, ByPrincipalID: "a"}, testCallerTenant); err != nil {
			t.Fatalf("approve: %v", err)
		}
		return c.ChangeID
	}
	if _, err := s.ActivateChange(ctx, propose(stale), testCallerTenant, "op"); !errors.Is(err, domain.ErrDriftDetected) {
		t.Errorf("stale expected hash: expected drift_detected, got %v", err)
	}
	if v := resolveOne(t, s, "payroll.batch_size", nil); string(v.Value) != "100" {
		t.Fatalf("a refused change must apply nothing, got %s", v.Value)
	}
	if _, err := s.ActivateChange(ctx, propose(good), testCallerTenant, "op"); err != nil {
		t.Errorf("matching expected hash must apply: %v", err)
	}
}
