package config_test

import (
	"os"
	"strings"
	"testing"

	"zoiko.io/jurisdiction-rules-svc/internal/config"
)

// withEnv sets the given variables for the duration of the test.
// t.Setenv restores the previous values and forbids parallel tests.
func withEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	// Unset everything this package reads first, so a variable left set in
	// the developer's shell cannot make a negative test pass.
	//
	// t.Setenv is called before os.Unsetenv purely to register the restore —
	// setting a variable to "" is NOT the same as unsetting it here, because
	// KAFKA_BROKERS distinguishes the two: explicitly empty means "no event
	// backbone", absent means "use the default".
	for _, k := range []string{
		"ENV", "PORT", "DB_HOST", "DB_PORT", "DB_NAME", "DB_USER", "DB_PASSWORD", "DB_SSLMODE",
		"KAFKA_BROKERS", "KAFKA_GROUP_ID", "KAFKA_EVENTS_TOPIC",
		"AUTHZ_SERVICE_URL", "AUTHZ_PLATFORM_SCOPE_ID", "OTEL_EXPORTER_OTLP_ENDPOINT",
	} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("failed to unset %s: %v", k, err)
		}
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

// TestLoad_KafkaBrokers covers the distinction envList exists for. Routing
// KAFKA_BROKERS through env() meant an explicitly empty value was replaced by
// the default, so a single-service local run could not turn the event
// backbone off and spent every mutation reaching for a broker that was never
// going to be there.
func TestLoad_KafkaBrokers(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value string
		want  []string
	}{
		{name: "unset uses the default", set: false, want: []string{"localhost:9092"}},
		{name: "explicitly empty means none", set: true, value: "", want: []string{}},
		{name: "whitespace-only means none", set: true, value: " , ", want: []string{}},
		{name: "single broker", set: true, value: "kafka:9094", want: []string{"kafka:9094"}},
		{name: "list is split and trimmed", set: true, value: "a:1, b:2 ,c:3", want: []string{"a:1", "b:2", "c:3"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			withEnv(t, env)
			if tc.set {
				t.Setenv("KAFKA_BROKERS", tc.value)
			}

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(cfg.Kafka.Brokers) != len(tc.want) {
				t.Fatalf("brokers = %v, want %v", cfg.Kafka.Brokers, tc.want)
			}
			for i := range tc.want {
				if cfg.Kafka.Brokers[i] != tc.want[i] {
					t.Errorf("brokers[%d] = %q, want %q", i, cfg.Kafka.Brokers[i], tc.want[i])
				}
			}
		})
	}
}

func TestLoad_LocalDefaults(t *testing.T) {
	withEnv(t, nil)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Env != "local" {
		t.Errorf("Env = %q, want local", cfg.Env)
	}
	if cfg.Port != 8082 {
		t.Errorf("Port = %d, want 8082", cfg.Port)
	}
	if cfg.Kafka.Topic != "zoiko.jurisdiction.events" {
		t.Errorf("Kafka topic = %q, want zoiko.jurisdiction.events", cfg.Kafka.Topic)
	}
	if len(cfg.Kafka.Brokers) == 0 {
		t.Error("expected a default Kafka broker list")
	}
}

func TestDSN(t *testing.T) {
	withEnv(t, map[string]string{
		"DB_HOST": "pg", "DB_PORT": "6000", "DB_NAME": "jr",
		"DB_USER": "u", "DB_PASSWORD": "p", "DB_SSLMODE": "disable",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "host=pg port=6000 dbname=jr user=u password=p sslmode=disable"
	if got := cfg.DB.DSN(); got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
}

// TestLoad_ProductionGuards covers the checks Load previously did not make —
// it returned a nil error unconditionally, so a production deployment with
// no DB password, plaintext Postgres, or no authorization scope came up
// looking perfectly healthy.
func TestLoad_ProductionGuards(t *testing.T) {
	valid := map[string]string{
		"ENV":                     "production",
		"DB_PASSWORD":             "s3cret",
		"DB_SSLMODE":              "require",
		"AUTHZ_PLATFORM_SCOPE_ID": "00000000-0000-0000-0000-0000000000ff",
	}

	cases := []struct {
		name      string
		mutate    func(map[string]string)
		wantError string
	}{
		{
			name:      "missing DB password",
			mutate:    func(m map[string]string) { delete(m, "DB_PASSWORD") },
			wantError: "DB_PASSWORD",
		},
		{
			name:      "plaintext Postgres connection",
			mutate:    func(m map[string]string) { m["DB_SSLMODE"] = "disable" },
			wantError: "DB_SSLMODE",
		},
		{
			// Without a scope every authorize call sends an empty
			// legal_entity_id, which authorization-svc answers 400 — a
			// fail-closed 503 on every admin mutation.
			name:      "missing authz platform scope",
			mutate:    func(m map[string]string) { delete(m, "AUTHZ_PLATFORM_SCOPE_ID") },
			wantError: "AUTHZ_PLATFORM_SCOPE_ID",
		},
		{
			name:      "invalid port",
			mutate:    func(m map[string]string) { m["PORT"] = "0" },
			wantError: "PORT",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range valid {
				env[k] = v
			}
			tc.mutate(env)
			withEnv(t, env)

			_, err := config.Load()
			if err == nil {
				t.Fatalf("expected startup to be refused, got nil error")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("error %q does not mention %q", err, tc.wantError)
			}
		})
	}

	t.Run("valid production config starts", func(t *testing.T) {
		withEnv(t, valid)
		if _, err := config.Load(); err != nil {
			t.Fatalf("a valid production config must start, got %v", err)
		}
	})
}

// TestLoad_LocalIsNotGuarded — the same settings that are fatal in production
// are exactly how the docker-compose dev stack runs, and must stay allowed.
func TestLoad_LocalIsNotGuarded(t *testing.T) {
	withEnv(t, map[string]string{
		"ENV":         "local",
		"DB_SSLMODE":  "disable",
		"DB_PASSWORD": "",
	})

	if _, err := config.Load(); err != nil {
		t.Fatalf("local development must not be blocked by the production guards, got %v", err)
	}
}
