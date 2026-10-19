package config

import "testing"

func TestLoad_AdmissionDefaultsAreProtective(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBodyBytes != 10<<20 || cfg.MaxCompressedBodyBytes != 1<<20 {
		t.Errorf("body caps: %d %d", cfg.MaxBodyBytes, cfg.MaxCompressedBodyBytes)
	}
	if len(cfg.AllowedRequestEncodings) != 0 {
		t.Errorf("compressed request bodies must be refused by default, got %v", cfg.AllowedRequestEncodings)
	}
	if len(cfg.SupportedAPIVersions) != 1 || cfg.SupportedAPIVersions[0] != "v1" {
		t.Errorf("versions: %v", cfg.SupportedAPIVersions)
	}
	if cfg.RateIPPerSecond <= 0 || cfg.RateIPBurst < 1 || cfg.RateTenantPerSecond <= 0 || cfg.RateTenantBurst < 1 || cfg.MaxInFlight <= 0 {
		t.Errorf("a protection is off by default: %+v", cfg)
	}
	if cfg.AdminAddr != "" || cfg.AdminTokenFile != "" {
		t.Error("the query listener must be off by default")
	}
}

func TestLoad_AdmissionOverrides(t *testing.T) {
	t.Setenv("GATEWAY_MAX_BODY_BYTES", "2048")
	t.Setenv("GATEWAY_ALLOWED_REQUEST_ENCODINGS", "gzip, br")
	t.Setenv("GATEWAY_SUPPORTED_API_VERSIONS", "v1,v2")
	t.Setenv("GATEWAY_RATE_IP_PER_SECOND", "0.5")
	t.Setenv("GATEWAY_RATE_IP_BURST", "3")
	t.Setenv("GATEWAY_MAX_INFLIGHT", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxBodyBytes != 2048 || cfg.RateIPPerSecond != 0.5 || cfg.RateIPBurst != 3 || cfg.MaxInFlight != 0 {
		t.Errorf("%+v", cfg)
	}
	if len(cfg.AllowedRequestEncodings) != 2 || cfg.AllowedRequestEncodings[1] != "br" {
		t.Errorf("encodings: %v", cfg.AllowedRequestEncodings)
	}
	if len(cfg.SupportedAPIVersions) != 2 {
		t.Errorf("versions: %v", cfg.SupportedAPIVersions)
	}
}

func TestLoad_EmptyVersionListMeansNoVersionAdmission(t *testing.T) {
	t.Setenv("GATEWAY_SUPPORTED_API_VERSIONS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SupportedAPIVersions) != 0 {
		t.Fatalf("an explicitly empty list must disable the gate, got %v", cfg.SupportedAPIVersions)
	}
}

// A limit that silently fell back to a default would leave an operator
// believing a protection was set that was not.
func TestLoad_AdmissionRefusesBadValues(t *testing.T) {
	for name, kv := range map[string][2]string{
		"malformed int":     {"GATEWAY_MAX_INFLIGHT", "lots"},
		"malformed float":   {"GATEWAY_RATE_IP_PER_SECOND", "fast"},
		"malformed bytes":   {"GATEWAY_MAX_BODY_BYTES", "10MB"},
		"negative bytes":    {"GATEWAY_MAX_BODY_BYTES", "-1"},
		"negative rate":     {"GATEWAY_RATE_TENANT_PER_SECOND", "-5"},
		"negative inflight": {"GATEWAY_MAX_INFLIGHT", "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(kv[0], kv[1])
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%s was accepted", kv[0], kv[1])
			}
		})
	}
	t.Run("rate without burst", func(t *testing.T) {
		t.Setenv("GATEWAY_RATE_IP_PER_SECOND", "10")
		t.Setenv("GATEWAY_RATE_IP_BURST", "0")
		if _, err := Load(); err == nil {
			t.Fatal("an enabled rate with no burst would refuse everything")
		}
	})
}

func TestLoad_AdminListenerNeedsBothAddressAndTokenFile(t *testing.T) {
	t.Run("address only", func(t *testing.T) {
		t.Setenv("GATEWAY_ADMIN_ADDR", "127.0.0.1:9099")
		if _, err := Load(); err == nil {
			t.Fatal("the listener must never be served without authentication")
		}
	})
	t.Run("token file only", func(t *testing.T) {
		t.Setenv("GATEWAY_ADMIN_TOKEN_FILE", "/run/secrets/gw")
		if _, err := Load(); err == nil {
			t.Fatal("a token file with no address is a misconfiguration")
		}
	})
	t.Run("both", func(t *testing.T) {
		t.Setenv("GATEWAY_ADMIN_ADDR", "127.0.0.1:9099")
		t.Setenv("GATEWAY_ADMIN_TOKEN_FILE", "/run/secrets/gw")
		cfg, err := Load()
		if err != nil || cfg.AdminAddr == "" || cfg.AdminTokenFile != "/run/secrets/gw" {
			t.Fatalf("%+v %v", cfg, err)
		}
	})
}
