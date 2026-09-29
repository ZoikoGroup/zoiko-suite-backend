package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
)

func TestLooksLikeSecretMaterial(t *testing.T) {
	material := map[string]string{
		"pem private key":      `"-----BEGIN RSA PRIVATE KEY-----\nMIIE..."`,
		"aws access key":       `"AKIAIOSFODNN7EXAMPLE"`,
		"github token":         `"ghp_0123456789abcdefghijklmnopqrstuvwxyz"`,
		"slack token":          `"xoxb-1234567890-abcdefghij"`,
		"stripe live key":      `"sk_live_0123456789abcdefABCD"`,
		"jwt":                  `"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"`,
		"uri with credentials": `"postgres://app:hunter2@db.internal:5432/app"`,
		"password field":       `{"host":"smtp.internal","password":"hunter2"}`,
		"nested token field":   `{"clients":[{"id":"a","client_secret":"s3cr3t"}]}`,
	}
	for name, v := range material {
		if !domain.LooksLikeSecretMaterial(json.RawMessage(v)) {
			t.Errorf("%s: not detected", name)
		}
	}

	// Ordinary configuration must not trip the guard.
	clean := map[string]string{
		"integer":                   `100`,
		"url without credentials":   `"https://api.example.com/v1"`,
		"jwks url":                  `"https://auth.example.com/.well-known/jwks.json"`,
		"email":                     `"ops@example.com"`,
		"password field references": `{"host":"smtp.internal","password":"secret://production/smtp/password"}`,
		"password policy settings":  `{"password_min_length":12,"token_ttl_seconds":3600}`,
		"begin marker in prose":     `"-----BEGIN NOTICE----- maintenance tonight"`,
	}
	for name, v := range clean {
		if domain.LooksLikeSecretMaterial(json.RawMessage(v)) {
			t.Errorf("%s: false positive", name)
		}
	}
}

func TestParseSecretReference(t *testing.T) {
	env, path, ok := domain.ParseSecretReference(json.RawMessage(`"secret://production/payments/stripe"`))
	if !ok || env != "production" || path != "payments/stripe" {
		t.Errorf("valid reference: got %q %q %v", env, path, ok)
	}
	for _, v := range []string{`"secret://payments"`, `"secret:///x"`, `"secret://production/"`, `"vault://production/x"`, `42`, `"secret://PROD/x"`} {
		if _, _, ok := domain.ParseSecretReference(json.RawMessage(v)); ok {
			t.Errorf("%s: must not parse as a reference", v)
		}
	}
}

func TestValidateValue_SecretReferenceBoundToEnvironment(t *testing.T) {
	def := &domain.ConfigDefinition{ValueType: domain.ValueTypeString, Sensitivity: domain.SensitivitySecretReferenceOnly}
	if err := domain.ValidateValue(json.RawMessage(`"secret://production/payments/stripe"`), def, "production"); err != nil {
		t.Errorf("same-environment reference refused: %v", err)
	}
	if err := domain.ValidateValue(json.RawMessage(`"secret://production/payments/stripe"`), def, "staging"); !errors.Is(err, domain.ErrSecretReferenceEnvironment) {
		t.Errorf("staging reaching a production store (INV-11): expected mismatch, got %v", err)
	}
	if err := domain.ValidateValue(json.RawMessage(`"secret://staging/payments/stripe"`), def, "production"); !errors.Is(err, domain.ErrSecretReferenceEnvironment) {
		t.Errorf("production resolving outside production (INV-10): expected mismatch, got %v", err)
	}
	// A reference naming no environment reads its first segment as one
	// ("payments"), which is not the environment being written — refused
	// either way. A single-segment reference cannot parse at all.
	if err := domain.ValidateValue(json.RawMessage(`"secret://payments/stripe"`), def, "production"); !errors.Is(err, domain.ErrSecretReferenceEnvironment) {
		t.Errorf("reference naming no environment: expected refusal, got %v", err)
	}
	if err := domain.ValidateValue(json.RawMessage(`"secret://stripe"`), def, "production"); !errors.Is(err, domain.ErrSecretValueProhibited) {
		t.Errorf("single-segment reference: expected prohibited, got %v", err)
	}
	if err := domain.ValidateValue(json.RawMessage(`"sk_live_0123456789abcdefABCD"`), def, "production"); !errors.Is(err, domain.ErrSecretValueProhibited) {
		t.Errorf("material on a secret key: expected prohibited, got %v", err)
	}
}

func TestValidateValue_MaterialRefusedOnOrdinaryKeys(t *testing.T) {
	def := &domain.ConfigDefinition{ValueType: domain.ValueTypeStructured, Sensitivity: domain.SensitivityInternal}
	if err := domain.ValidateValue(json.RawMessage(`{"host":"smtp.internal","password":"hunter2"}`), def, "production"); !errors.Is(err, domain.ErrSecretValueProhibited) {
		t.Errorf("INV-09 applies to every key: expected prohibited, got %v", err)
	}
	if err := domain.ValidateValue(json.RawMessage(`{"host":"smtp.internal","port":587}`), def, "production"); err != nil {
		t.Errorf("clean structured value refused: %v", err)
	}
}
