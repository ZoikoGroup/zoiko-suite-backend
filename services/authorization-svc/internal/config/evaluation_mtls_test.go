package config

import "testing"

func TestEvaluationMTLS_DefaultsToRequireOutsideLocal(t *testing.T) {
	t.Setenv("AUTHZ_EVALUATION_MTLS", "")
	for envName, want := range map[string]string{"local": "permit", "": "permit", "staging": "require", "production": "require"} {
		t.Setenv("ENV", envName)
		cfg, _ := Load()
		if cfg.EvaluationMTLS != want {
			t.Errorf("ENV=%q: want %s, got %s", envName, want, cfg.EvaluationMTLS)
		}
	}
}

func TestValidate_RequireModeNeedsListenerAndAllowList(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"permit, nothing else", Config{EvaluationMTLS: "permit"}, true},
		{"require, complete", Config{EvaluationMTLS: "require", MTLSEnabled: true, PEPAllowList: []string{"access-control-svc"}}, true},
		{"require, no listener", Config{EvaluationMTLS: "require", PEPAllowList: []string{"access-control-svc"}}, false},
		{"require, empty allow-list", Config{EvaluationMTLS: "require", MTLSEnabled: true}, false},
		{"unknown mode", Config{EvaluationMTLS: "off"}, false},
	}
	for _, c := range cases {
		if err := c.cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: want ok=%v, got err=%v", c.name, c.ok, err)
		}
	}
}
