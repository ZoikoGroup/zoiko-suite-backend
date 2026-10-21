package config

import "testing"

// Governance audit N-12: outside a local stack the entity registry is
// required, or the tenant of an unprojected entity is taken on trust.
func TestValidate_EntityRegistryRequiredOutsideLocal(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"local, no registry", Config{Env: "local", EvaluationMTLS: "permit"}, true},
		{"staging, no registry", Config{Env: "staging", EvaluationMTLS: "permit"}, false},
		{"production, registry set", Config{Env: "production", EvaluationMTLS: "permit", EntityRegistryURL: "http://tenant-entity-registry-svc:8081"}, true},
	}
	for _, c := range cases {
		if err := c.cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: want ok=%v, got err=%v", c.name, c.ok, err)
		}
	}
}

// Governance audit N-07: the step-up policy is the server's, defaulting to
// "an unstated authentication age is not recent".
func TestStepUpPolicy_DefaultsAndValidation(t *testing.T) {
	t.Setenv("AUTHZ_STEP_UP_UNKNOWN_AGE", "")
	t.Setenv("AUTHZ_STEP_UP_ACTIONS", "PAYMENT_RELEASE, gl.period.close")
	t.Setenv("AUTHZ_STEP_UP_MAX_AUTHN_AGE_SECONDS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StepUpUnknownAge != "step_up" || cfg.StepUpMaxAuthnAgeSeconds != 300 {
		t.Errorf("defaults: unknown_age=%q max_age=%d, want step_up / 300", cfg.StepUpUnknownAge, cfg.StepUpMaxAuthnAgeSeconds)
	}
	if len(cfg.StepUpActions) != 2 || cfg.StepUpActions[1] != "gl.period.close" {
		t.Errorf("actions = %v", cfg.StepUpActions)
	}
	if err := (&Config{EvaluationMTLS: "permit", StepUpUnknownAge: "allow"}).Validate(); err == nil {
		t.Error("an unknown AUTHZ_STEP_UP_UNKNOWN_AGE was accepted")
	}
}
