package store_test

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
)

// ── Audit 2026-09-23, gap 6: secret references and the promotion boundary ────

func declareSecret(t *testing.T, s *store.PgStore, key string) {
	t.Helper()
	ctx := context.Background()
	def, err := s.CreateDefinition(ctx, domain.CreateDefinitionParams{
		Key: key, Owner: "payments-svc", ValueType: domain.ValueTypeString, SafetyClass: domain.SafetyS1,
		AllowedScopes: []string{domain.ScopeEnvironment}, FallbackPolicy: domain.FallbackBlock,
		Sensitivity: domain.SensitivitySecretReferenceOnly, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("declare %s: %v", key, err)
	}
	if _, err := s.PublishDefinition(ctx, domain.PublishDefinitionParams{
		DefinitionID: def.DefinitionID, Lifecycle: domain.LifecyclePublished, ActorPrincipalID: "admin-1",
		ApprovalReference: "CAB-2026-002",
	}); err != nil {
		t.Fatalf("publish %s: %v", key, err)
	}
}

func writeEnv(s *store.PgStore, key, env, value string) error {
	_, _, err := s.UpsertConfigEntry(context.Background(), domain.UpsertConfigEntryParams{
		Key: key, Value: []byte(value), Environment: env,
		CreatedByPrincipalID: "admin-1", CallerTenantID: testCallerTenant,
	})
	return err
}

// INV-09/10/11: every write path refuses material, and a reference resolves
// only in the environment it names.
func TestGap6_SecretReferencesAreEnvironmentBound(t *testing.T) {
	s := store.New(openTestPool(t), zap.NewNop())
	declareSecret(t, s, "payments.stripe_key")

	if err := writeEnv(s, "payments.stripe_key", "production", `"secret://production/payments/stripe"`); err != nil {
		t.Fatalf("a production reference in production must be accepted: %v", err)
	}
	if err := writeEnv(s, "payments.stripe_key", "staging", `"secret://production/payments/stripe"`); !errors.Is(err, domain.ErrSecretReferenceEnvironment) {
		t.Errorf("staging pointing at the production store (INV-11): expected mismatch, got %v", err)
	}
	if err := writeEnv(s, "payments.stripe_key", "production", `"sk_live_0123456789abcdefABCD"`); !errors.Is(err, domain.ErrSecretValueProhibited) {
		t.Errorf("material on a secret key: expected prohibited, got %v", err)
	}
	if _, err := s.ActivateOverride(context.Background(), domain.ActivateOverrideParams{
		Key: "payments.stripe_key", Layer: domain.ScopeEnvironment, Environment: "staging",
		Value: []byte(`"secret://production/payments/stripe"`), CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	}); !errors.Is(err, domain.ErrSecretReferenceEnvironment) {
		t.Errorf("override path: expected mismatch, got %v", err)
	}
}

// INV-09 on an ordinary key: being undeclared as a secret does not license
// storing one.
func TestGap6_MaterialRefusedOnOrdinaryKey(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	declare(t, s, "smtp.relay", domain.ValueTypeStructured, domain.SafetyS1, domain.ScopeEnvironment)
	if err := writeEnv(s, "smtp.relay", "production", `{"host":"smtp.internal","password":"hunter2"}`); !errors.Is(err, domain.ErrSecretValueProhibited) {
		t.Errorf("expected secret_value_prohibited, got %v", err)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM config_entries WHERE key = 'smtp.relay'`); n != 0 {
		t.Errorf("refused material must not be persisted, found %d rows", n)
	}
}

// INV-11: a change set lives in one environment; a part naming another is
// refused, so a lower-environment change can never write production values.
func TestGap6_ChangeSetCannotCrossEnvironments(t *testing.T) {
	pool := openTestPool(t)
	s := store.New(pool, zap.NewNop())
	seedConfig(t, pool, "payroll.batch_size")
	if err := upsertStaging(s, "payroll.batch_size", `100`); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	_, err := s.CreateChange(context.Background(), domain.CreateChangeParams{
		ChangeClass: domain.ChangeClassC0, Environment: "staging",
		Parts: []domain.ChangePart{{
			Kind: domain.PartKindConfig, Key: "payroll.batch_size",
			Scope: domain.ChangePartScope{Environment: "production"}, NewValue: []byte(`999`),
		}},
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	if !errors.Is(err, domain.ErrEnvironmentBoundaryViolation) {
		t.Fatalf("expected environment_boundary_violation, got %v", err)
	}

	// An omitted part environment is the change's own.
	c, err := s.CreateChange(context.Background(), domain.CreateChangeParams{
		ChangeClass: domain.ChangeClassC0, Environment: "staging",
		Parts:          []domain.ChangePart{{Kind: domain.PartKindConfig, Key: "payroll.batch_size", NewValue: []byte(`150`)}},
		CallerTenantID: testCallerTenant, ActorPrincipalID: "admin-1",
	})
	if err != nil {
		t.Fatalf("part with no environment: %v", err)
	}
	approveActivate(t, s, c.ChangeID)
	if v := resolveOne(t, s, "payroll.batch_size", nil); string(v.Value) != "150" {
		t.Errorf("expected the change applied in staging, got %s", v.Value)
	}
}
