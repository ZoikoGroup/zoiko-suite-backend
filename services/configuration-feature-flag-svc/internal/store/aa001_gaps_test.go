package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── Audit 2026-09-23, gap 2: key declaration (INV-05/06/08, NP-01/02/03/05/07)
//
// Each test drives one negative path the audit called "unsatisfiable as built"
// through the store's real write and read paths, against a real schema.

// declare registers and publishes a definition through the registry itself,
// not a seeding shortcut, so the gate under test is the one production uses.
func declare(t *testing.T, s *store.PgStore, key, valueType, safety string, scopes ...string) {
	t.Helper()
	ctx := context.Background()
	def, err := s.CreateDefinition(ctx, domain.CreateDefinitionParams{
		Key: key, Owner: "payroll-svc", ValueType: valueType, SafetyClass: safety,
		AllowedScopes: scopes, FallbackPolicy: domain.FallbackBlock,
		Sensitivity: domain.SensitivityInternal, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("declare %s: %v", key, err)
	}
	if _, err := s.PublishDefinition(ctx, domain.PublishDefinitionParams{
		DefinitionID: def.DefinitionID, Lifecycle: domain.LifecyclePublished, ActorPrincipalID: "admin-1",
		ApprovalReference: "CAB-2026-001",
	}); err != nil {
		t.Fatalf("publish %s: %v", key, err)
	}
}

// seedSnapshot gives "production" a first snapshot: a change set pins its
// before-state to one, so the environment needs an imprint to propose into.
func seedSnapshot(t *testing.T, s *store.PgStore) {
	t.Helper()
	declare(t, s, "platform.bootstrap", domain.ValueTypeInteger, domain.SafetyS1, domain.ScopeEnvironment)
	if err := upsert(s, "platform.bootstrap", nil, `1`); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func upsert(s *store.PgStore, key string, tenant *string, value string) error {
	_, _, err := s.UpsertConfigEntry(context.Background(), domain.UpsertConfigEntryParams{
		Key: key, Value: []byte(value), Environment: "production", TenantID: tenant,
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	})
	return err
}

func override(s *store.PgStore, key, layer string, scopeID *string, value string) error {
	_, err := s.ActivateOverride(context.Background(), domain.ActivateOverrideParams{
		Key: key, Layer: layer, Environment: "production", ScopeID: scopeID,
		Value: []byte(value), CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	return err
}

// INV-05 / NP-02 (write side): an undeclared key is refused on every write path.
func TestGap2_UnknownKeyRefusedOnEveryWritePath(t *testing.T) {
	s := store.New(openTestPool(t), zap.NewNop())
	if err := upsert(s, "invented.key", nil, `1`); !errors.Is(err, domain.ErrKeyNotRegistered) {
		t.Errorf("POST /v1/config path: expected key_not_registered, got %v", err)
	}
	if err := override(s, "invented.key", domain.ScopeEnvironment, nil, `1`); !errors.Is(err, domain.ErrKeyNotRegistered) {
		t.Errorf("PUT overrides path: expected key_not_registered, got %v", err)
	}
}

// NP-02 (read side): a consumer asking for a key nobody declared must get an
// explicit BLOCKED answer, not an absence it can paper over with a default of
// its own invention.
func TestGap2_ResolveAnswersUnknownKeyExplicitly(t *testing.T) {
	s := store.New(openTestPool(t), zap.NewNop())
	declare(t, s, "payroll.batch_size", domain.ValueTypeInteger, domain.SafetyS1, domain.ScopeEnvironment)
	declare(t, s, "payroll.cutoff_day", domain.ValueTypeInteger, domain.SafetyS1, domain.ScopeEnvironment)
	if err := upsert(s, "payroll.batch_size", nil, `100`); err != nil {
		t.Fatalf("baseline write: %v", err)
	}

	res, err := s.Resolve(context.Background(), domain.ResolveParams{
		Environment: "production", Keys: []string{"payroll.batch_size", "invented.key", "payroll.cutoff_day"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := map[string]domain.ResolvedValue{}
	for _, v := range res.Values {
		got[v.Key] = v
	}
	if v := got["payroll.batch_size"]; v.Outcome != domain.OutcomeValue || string(v.Value) != "100" {
		t.Errorf("declared key with a value: got %+v", v)
	}
	if v, ok := got["invented.key"]; !ok || v.Outcome != domain.OutcomeBlocked || v.Reason != domain.ReasonUnknownKey || len(v.Value) != 0 {
		t.Errorf("undeclared key must answer BLOCKED/UNKNOWN_KEY with no value, got present=%v %+v", ok, v)
	}
	if v, ok := got["payroll.cutoff_day"]; !ok || v.Outcome != domain.OutcomeBlocked || v.Reason != domain.ReasonNoValue ||
		v.SafetyClass != domain.SafetyS1 || v.FallbackPolicy != domain.FallbackBlock {
		t.Errorf("declared key with no value must answer BLOCKED/NO_VALUE with its declared fallback, got present=%v %+v", ok, v)
	}
}

// INV-06: a key cannot be declared without every mandatory attribute — the
// store refuses it, not only the handler's missing-field check.
func TestGap2_DeclarationRequiresEveryMandatoryAttribute(t *testing.T) {
	s := store.New(openTestPool(t), zap.NewNop())
	base := domain.CreateDefinitionParams{
		Key: "k", Owner: "payroll-svc", ValueType: domain.ValueTypeInteger, SafetyClass: domain.SafetyS1,
		AllowedScopes: []string{domain.ScopeEnvironment}, FallbackPolicy: domain.FallbackBlock,
		Sensitivity: domain.SensitivityInternal, ActorPrincipalID: "admin-1",
	}
	for name, mutate := range map[string]func(*domain.CreateDefinitionParams){
		"owner":       func(p *domain.CreateDefinitionParams) { p.Owner = " " },
		"value type":  func(p *domain.CreateDefinitionParams) { p.ValueType = "" },
		"safety":      func(p *domain.CreateDefinitionParams) { p.SafetyClass = "" },
		"scopes":      func(p *domain.CreateDefinitionParams) { p.AllowedScopes = nil },
		"fallback":    func(p *domain.CreateDefinitionParams) { p.FallbackPolicy = "" },
		"sensitivity": func(p *domain.CreateDefinitionParams) { p.Sensitivity = "" },
	} {
		p := base
		p.Key = "k." + name
		mutate(&p)
		if _, err := s.CreateDefinition(context.Background(), p); !errors.Is(err, domain.ErrValueConstraintFailed) {
			t.Errorf("missing %s: expected value_constraint_failed, got %v", name, err)
		}
	}
}

// NP-01 / INV-08: a PLATFORM_ONLY safety key — allowed_scopes names the
// environment alone — cannot be overridden by a tenant on any path.
func TestGap2_TenantCannotOverridePlatformOnlyKey(t *testing.T) {
	s := store.New(openTestPool(t), zap.NewNop())
	declare(t, s, "security.mfa_required", domain.ValueTypeBoolean, domain.SafetyS3, domain.ScopeEnvironment)
	tenant := testCallerTenant
	// A material key takes no direct write at any scope.
	if err := upsert(s, "security.mfa_required", &tenant, `false`); !errors.Is(err, domain.ErrMaterialKeyRequiresChange) {
		t.Errorf("POST /v1/config tenant write: expected material_key_requires_change, got %v", err)
	}
	if err := override(s, "security.mfa_required", domain.ScopeTenant, &tenant, `false`); !errors.Is(err, domain.ErrMaterialKeyRequiresChange) {
		t.Errorf("PUT overrides/tenant: expected material_key_requires_change, got %v", err)
	}
	// Through the governed path, the tenant scope is still refused by the
	// allowlist, and the declared scope is accepted.
	part := func(tenantID *string) domain.CreateChangeParams {
		return domain.CreateChangeParams{
			ChangeClass: domain.ChangeClassC3, Environment: "production",
			Parts: []domain.ChangePart{{Kind: domain.PartKindConfig, Key: "security.mfa_required",
				Scope: domain.ChangePartScope{Environment: "production", TenantID: tenantID}, NewValue: []byte(`true`)}},
			CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
		}
	}
	if err := override(s, "security.mfa_required", domain.ScopeEnvironment, nil, `true`); !errors.Is(err, domain.ErrMaterialKeyRequiresChange) {
		t.Errorf("PUT overrides/environment: expected material_key_requires_change, got %v", err)
	}
	seedSnapshot(t, s)
	if _, err := s.CreateChange(context.Background(), part(&tenant)); !errors.Is(err, domain.ErrScopeNotAllowed) {
		t.Errorf("change set, tenant scope: expected scope_not_allowed, got %v", err)
	}
	if _, err := s.CreateChange(context.Background(), part(nil)); err != nil {
		t.Errorf("change set, declared scope: must be accepted, got %v", err)
	}
}

// NP-03: a BOOLEAN key published with a string value through the raw API.
func TestGap2_BooleanKeyRefusesStringValue(t *testing.T) {
	s := store.New(openTestPool(t), zap.NewNop())
	declare(t, s, "billing.dunning_enabled", domain.ValueTypeBoolean, domain.SafetyS1, domain.ScopeEnvironment)
	for _, v := range []string{`"true"`, `1`, `"yes"`} {
		if err := upsert(s, "billing.dunning_enabled", nil, v); !errors.Is(err, domain.ErrTypeMismatch) {
			t.Errorf("POST /v1/config value %s: expected type_mismatch, got %v", v, err)
		}
		if err := override(s, "billing.dunning_enabled", domain.ScopeEnvironment, nil, v); !errors.Is(err, domain.ErrTypeMismatch) {
			t.Errorf("PUT overrides value %s: expected type_mismatch, got %v", v, err)
		}
	}
}

// NP-05: two equal-precedence overrides for the same scope and time. Racing
// writers must leave exactly one current row, and the schema itself must
// refuse a second one however it is inserted.
func TestGap2_NoTwoCurrentOverridesForOneScope(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	declare(t, s, "payroll.batch_size", domain.ValueTypeInteger, domain.SafetyS1, domain.ScopeEnvironment)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			b, _ := json.Marshal(100 + v)
			_ = upsert(s, "payroll.batch_size", nil, string(b))
		}(i)
	}
	wg.Wait()
	if n := currentRows(t, pool, "payroll.batch_size"); n != 1 {
		t.Fatalf("racing writers left %d current rows for one scope, want 1", n)
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO config_entries (key, value, environment, tenant_id, created_by_principal_id)
		VALUES ('payroll.batch_size', '999', 'production', NULL, 'raw-sql')`)
	if err == nil {
		t.Fatalf("the schema admitted a second current row for the same scope")
	}
}

func currentRows(t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM config_entries WHERE key = $1 AND effective_to IS NULL`, key).Scan(&n); err != nil {
		t.Fatalf("count current rows: %v", err)
	}
	return n
}

// NP-07: a user preference may never modify an authority-bearing (S2/S3) key —
// neither by declaring the layer on such a key, nor by writing at that layer.
func TestGap2_UserPreferenceCannotTouchAuthorityBearingKey(t *testing.T) {
	s := store.New(openTestPool(t), zap.NewNop())
	for _, safety := range []string{domain.SafetyS2, domain.SafetyS3} {
		_, err := s.CreateDefinition(context.Background(), domain.CreateDefinitionParams{
			Key: "approval.limit." + safety, Owner: "ap-svc", ValueType: domain.ValueTypeDecimal, SafetyClass: safety,
			AllowedScopes:  []string{domain.ScopeEnvironment, domain.LayerUserPreference},
			FallbackPolicy: domain.FallbackBlock, Sensitivity: domain.SensitivityInternal, ActorPrincipalID: "admin-1",
		})
		if !errors.Is(err, domain.ErrScopeNotAllowed) {
			t.Errorf("%s key declaring USER_PREFERENCE: expected scope_not_allowed, got %v", safety, err)
		}
	}
	declare(t, s, "approval.limit", domain.ValueTypeDecimal, domain.SafetyS2, domain.ScopeEnvironment)
	subject := "user-1"
	if err := override(s, "approval.limit", domain.LayerUserPreference, &subject, `1000000`); !errors.Is(err, domain.ErrScopeNotAllowed) {
		t.Errorf("user-preference write to an S2 key: expected scope_not_allowed, got %v", err)
	}
}
