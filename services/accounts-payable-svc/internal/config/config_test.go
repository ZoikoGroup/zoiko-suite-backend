package config_test

import (
	"strings"
	"testing"

	"zoiko.io/accounts-payable-svc/internal/config"
)

// Load used to end in `return cfg, nil` unconditionally, so every one of these
// combinations started successfully. Each is a value that is correct locally and
// quietly wrong in production — the failure mode is a security property, not a
// crash, so nothing surfaced it.

func TestLoad_LocalDefaults_Succeed(t *testing.T) {
	t.Setenv("ENV", "local")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("local defaults must load: %v", err)
	}
	if cfg.Port != 8099 {
		t.Fatalf("expected default port 8099, got %d", cfg.Port)
	}
	if cfg.DB.Name != "accounts_payable" {
		t.Fatalf("expected default db accounts_payable, got %q", cfg.DB.Name)
	}
}

func TestLoad_ProductionGuards(t *testing.T) {
	// Every case starts from a valid production config and breaks exactly one
	// thing, so a failure names the guard that fired.
	valid := map[string]string{
		"ENV":               "production",
		"DB_PASSWORD":       "s3cret",
		"DB_SSLMODE":        "require",
		"AUTHZ_SERVICE_URL": "http://authorization-svc:8089",
		"KAFKA_BROKERS":     "kafka:9092",
	}

	for _, tc := range []struct {
		name     string
		override map[string]string
		wantIn   string
	}{
		{
			name:     "empty DB_PASSWORD",
			override: map[string]string{"DB_PASSWORD": ""},
			wantIn:   "DB_PASSWORD",
		},
		{
			name:     "sslmode disable",
			override: map[string]string{"DB_SSLMODE": "disable"},
			wantIn:   "DB_SSLMODE",
		},
		{
			name:     "authz pointed at localhost",
			override: map[string]string{"AUTHZ_SERVICE_URL": "http://localhost:8089"},
			wantIn:   "AUTHZ_SERVICE_URL",
		},
		{
			name:     "no kafka brokers",
			override: map[string]string{"KAFKA_BROKERS": ""},
			wantIn:   "KAFKA_BROKERS",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range valid {
				t.Setenv(k, v)
			}
			for k, v := range tc.override {
				t.Setenv(k, v)
			}

			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected %s to be refused when ENV=production", tc.wantIn)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error should name %s, got: %v", tc.wantIn, err)
			}
		})
	}
}

// The same values are all deliberately fine locally — the guard must not make a
// developer machine configure production secrets to boot.
func TestLoad_LocalToleratesWhatProductionRefuses(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("DB_PASSWORD", "")
	t.Setenv("DB_SSLMODE", "disable")
	t.Setenv("AUTHZ_SERVICE_URL", "http://localhost:8089")
	t.Setenv("KAFKA_BROKERS", "")

	if _, err := config.Load(); err != nil {
		t.Fatalf("local must tolerate these: %v", err)
	}
}

// An empty AUTHZ_SERVICE_URL is refused everywhere, local included: every
// mutation is gated on that call and fails closed, so the service would start
// healthy and be unable to do anything.
func TestLoad_EmptyAuthzURL_RefusedEvenLocally(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("AUTHZ_SERVICE_URL", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("an empty AUTHZ_SERVICE_URL must be refused")
	}
}

// os.Getenv cannot tell unset from empty, so `KAFKA_BROKERS=` used to be
// replaced by the default and the local no-broker path was unreachable.
func TestLoad_ExplicitlyEmptyKafkaBrokers_YieldsNoBrokers(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("KAFKA_BROKERS", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Kafka.Brokers) != 0 {
		t.Fatalf("expected no brokers, got %v", cfg.Kafka.Brokers)
	}
}

// strings.Split("", ",") returns one empty-string broker, which the writer
// accepts and then fails to dial on every publish.
func TestLoad_KafkaBrokers_DropsBlanks(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("KAFKA_BROKERS", "kafka:9092, , kafka2:9092,")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"kafka:9092", "kafka2:9092"}
	if len(cfg.Kafka.Brokers) != len(want) {
		t.Fatalf("expected %v, got %v", want, cfg.Kafka.Brokers)
	}
	for i, broker := range want {
		if cfg.Kafka.Brokers[i] != broker {
			t.Fatalf("broker %d = %q, want %q", i, cfg.Kafka.Brokers[i], broker)
		}
	}
}
