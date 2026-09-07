package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// SUPABASE_DB_URL matches none of the secret name markers and carries a live
// database password. Keying the masking decision off the variable NAME printed
// it in full, so the URL check has to run first and ignore the name.
func TestMaskNeverPrintsAURLPassword(t *testing.T) {
	cases := []struct {
		key, val string
		wantHide string // a substring that must NOT appear
		wantKeep string // a substring that must appear
	}{
		{"SUPABASE_DB_URL", "postgresql://postgres.abc:s3cr3tpw@aws-0-ap-northeast-1.pooler.supabase.com:5432/postgres?sslmode=require", "s3cr3tpw", "pooler.supabase.com"},
		{"REDIS_URL", "rediss://default:AaBbCc123@apt-cat-1234.upstash.io:6379", "AaBbCc123", "upstash.io"},
		{"DATABASE_URL", "postgres://app:hunter2@127.0.0.1:5432/general_ledger?sslmode=disable", "hunter2", "general_ledger"},
		// A name with no hint of a secret in it at all.
		{"SOME_ENDPOINT", "https://user:letmein@example.com/x", "letmein", "example.com"},
	}
	for _, c := range cases {
		got := Mask(c.key, c.val)
		if strings.Contains(got, c.wantHide) {
			t.Errorf("Mask(%s) leaked the password: %s", c.key, got)
		}
		if !strings.Contains(got, c.wantKeep) {
			t.Errorf("Mask(%s) hid too much, %q is gone: %s", c.key, c.wantKeep, got)
		}
		// url.String() percent-encodes an asterisk, which renders the mask as
		// %2A%2A%2A%2A and reads like a real value.
		if strings.Contains(got, "%2A") {
			t.Errorf("Mask(%s) percent-encoded the mask: %s", c.key, got)
		}
	}
}

func TestMaskByKeyName(t *testing.T) {
	if got := Mask("DB_PASSWORD", "postgres"); got != "****(8 chars)" {
		t.Errorf("DB_PASSWORD masked as %q", got)
	}
	// A key id names a key, it is not one; masking it makes the output useless
	// for checking which signing key a service came up with.
	if got := Mask("JWT_KEY_ID", "local-dev-key-1"); got != "local-dev-key-1" {
		t.Errorf("JWT_KEY_ID was masked: %q", got)
	}
	// A plain service URL must stay readable, or diagnosing cross-service wiring
	// from this output becomes impossible.
	if got := Mask("AUTHZ_SERVICE_URL", "http://127.0.0.1:8089"); got != "http://127.0.0.1:8089" {
		t.Errorf("a bare service URL was masked: %q", got)
	}
}

func TestLoadGlobalEnvPrecedenceAndQuoting(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, ".env")
	local := filepath.Join(dir, ".env.local")
	os.WriteFile(base, []byte("A=from-base\nB=base-only\n# comment\nQ=\"quoted\"\n"), 0o600)
	os.WriteFile(local, []byte("export A=from-local\nC=local-only\nD='single'\n"), 0o600)

	g, err := LoadGlobalEnv(base, local, filepath.Join(dir, "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Files()) != 2 {
		t.Errorf("expected 2 files loaded, got %v", g.Files())
	}
	for k, want := range map[string]string{
		"A": "from-local", // later file wins
		"B": "base-only",
		"C": "local-only",
		"Q": "quoted", // one layer of quotes stripped
		"D": "single",
	} {
		if got := g.Get(k, "<unset>"); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// A ${...} value is compose substitution syntax. Expanding it here would be
// wrong and leaving it as a literal is at least visible, so it must survive
// verbatim rather than becoming an empty string.
func TestLoadGlobalEnvDoesNotExpand(t *testing.T) {
	p := writeEnvFile(t, ".env", "X=${SOMETHING:-default}\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Get("X", ""); got != "${SOMETHING:-default}" {
		t.Errorf("X = %q; substitution syntax should survive literally", got)
	}
}

// PORT is the dangerous one: a stray PORT in the operator's shell would be
// handed to all 86 services, and 85 would fail to bind with no obvious cause.
func TestManagedKeysAreNotInherited(t *testing.T) {
	t.Setenv("PORT", "9999")
	t.Setenv("DB_NAME", "wrong_database")

	p := writeEnvFile(t, ".env", "PORT=7777\nDB_NAME=also_wrong\nDB_HOST=db.example.com\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Name: "x-svc", Port: 8098, DBName: "general_ledger"}
	env := g.ServiceEnv(svc)

	if env["PORT"] != "8098" {
		t.Errorf("PORT = %q, want the allocated 8098", env["PORT"])
	}
	if env["DB_NAME"] != "general_ledger" {
		t.Errorf("DB_NAME = %q, want the service's own database", env["DB_NAME"])
	}
	// A non-managed key from the global file SHOULD come through.
	if env["DB_HOST"] != "db.example.com" {
		t.Errorf("DB_HOST = %q, want the global value", env["DB_HOST"])
	}
}

func TestServiceEnvSupabaseProvider(t *testing.T) {
	p := writeEnvFile(t, ".env", strings.Join([]string{
		"ZOIKO_DB_PROVIDER=supabase",
		"SUPABASE_DB_HOST=aws-0-ap-northeast-1.pooler.supabase.com",
		"SUPABASE_DB_PORT=6543",
		"SUPABASE_PROJECT_REF=abcdefgh",
		"APP_DB_PASSWORD=pw123",
	}, "\n")+"\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	if g.Provider() != "supabase" {
		t.Fatalf("provider = %q", g.Provider())
	}
	env := g.ServiceEnv(&Service{Name: "general-ledger-svc", Port: 8098, DBName: "general_ledger", DBRole: "app_general_ledger"})

	// Per-service role, with the project ref appended. A bare role name is
	// rejected by the pooler as an authentication failure.
	if env["DB_USER"] != "app_general_ledger.abcdefgh" {
		t.Errorf("DB_USER = %q, want the per-service role plus project ref", env["DB_USER"])
	}
	// Supabase has one database, called postgres; separation is by schema.
	if env["DB_NAME"] != "postgres" {
		t.Errorf("DB_NAME = %q, want postgres", env["DB_NAME"])
	}
	if env["DB_SCHEMA"] != "general_ledger" {
		t.Errorf("DB_SCHEMA = %q, want the service's logical name", env["DB_SCHEMA"])
	}
	if env["DB_SSLMODE"] != "require" {
		t.Errorf("DB_SSLMODE = %q; Supabase requires TLS", env["DB_SSLMODE"])
	}
	// Without this, pgx on the transaction pooler produces "prepared statement
	// already exists" intermittently, under concurrency.
	if !strings.Contains(env["DB_OPTIONS"], "statement_cache_capacity=0") {
		t.Errorf("DB_OPTIONS = %q; the transaction pooler needs the cache off", env["DB_OPTIONS"])
	}
	// The 34 services that read a whole DATABASE_URL get schema routing through
	// the search_path parameter, which pgx passes to the server.
	if !strings.Contains(env["DATABASE_URL"], "search_path=general_ledger") {
		t.Errorf("DATABASE_URL = %q, want a search_path", env["DATABASE_URL"])
	}
}

// The discrete parts and the single URL must describe the same database. Under
// compose they were separate hand-written literals per service and could drift.
func TestDatabaseURLAgreesWithDiscreteParts(t *testing.T) {
	p := writeEnvFile(t, ".env", "ZOIKO_DB_PROVIDER=docker\nDB_HOST=127.0.0.1\nDB_PORT=5432\nDB_USER=app\nDB_PASSWORD=pw\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	env := g.ServiceEnv(&Service{Name: "gl", Port: 8098, DBName: "general_ledger"})
	for _, want := range []string{"app", "127.0.0.1:5432", "/general_ledger", "sslmode=disable"} {
		if !strings.Contains(env["DATABASE_URL"], want) {
			t.Errorf("DATABASE_URL %q is missing %q", env["DATABASE_URL"], want)
		}
	}
	if env["DB_NAME"] != "general_ledger" {
		t.Errorf("DB_NAME = %q", env["DB_NAME"])
	}
}

// A service with no database must receive no DB_* variables at all, rather than
// empty ones that look configured.
func TestServiceWithoutDatabaseGetsNoDBVars(t *testing.T) {
	p := writeEnvFile(t, ".env", "DB_HOST=127.0.0.1\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	env := g.ServiceEnv(&Service{Name: "mtls-management-svc", Port: 8140, DBName: ""})
	if _, set := env["DATABASE_URL"]; set {
		t.Error("a database-less service was given a DATABASE_URL")
	}
	if _, set := env["DB_NAME"]; set {
		t.Error("a database-less service was given a DB_NAME")
	}
}

// The service's own block overrides a shared default, but never PORT.
func TestServiceBlockOverridesGlobalButNotPort(t *testing.T) {
	p := writeEnvFile(t, ".env", "KAFKA_EVENTS_TOPIC=global.topic\nSHARED=global\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	env := g.ServiceEnv(&Service{
		Name: "gl", Port: 8098, DBName: "",
		Env: map[string]string{"KAFKA_EVENTS_TOPIC": "zoiko.general-ledger.events", "PORT": "1"},
	})
	if env["KAFKA_EVENTS_TOPIC"] != "zoiko.general-ledger.events" {
		t.Errorf("service block did not override the global: %q", env["KAFKA_EVENTS_TOPIC"])
	}
	if env["SHARED"] != "global" {
		t.Errorf("global value lost: %q", env["SHARED"])
	}
	if env["PORT"] != "8098" {
		t.Errorf("PORT = %q; the allocation must win over everything", env["PORT"])
	}
}

func TestProviderFallsBackToDocker(t *testing.T) {
	for _, in := range []string{"", "nonsense", "DOCKER", "Supabase"} {
		p := writeEnvFile(t, ".env", "ZOIKO_DB_PROVIDER="+in+"\n")
		g, err := LoadGlobalEnv(p)
		if err != nil {
			t.Fatal(err)
		}
		got := g.Provider()
		want := "docker"
		if strings.EqualFold(in, "supabase") {
			want = "supabase"
		}
		if got != want {
			t.Errorf("provider(%q) = %q, want %q", in, got, want)
		}
	}
}

// The isolation on a single-database host IS the per-service role: each holds
// USAGE on its own schema and no other. A shared login would give every service
// reach into every schema, so this asserts the roles differ per service and that
// the one documented naming exception is respected.
func TestSupabaseUsesOneRolePerService(t *testing.T) {
	p := writeEnvFile(t, ".env", strings.Join([]string{
		"ZOIKO_DB_PROVIDER=supabase",
		"SUPABASE_DB_HOST=pooler.example.com",
		"SUPABASE_PROJECT_REF=refref",
		"APP_DB_PASSWORD=pw",
	}, "\n")+"\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"identity-context-svc":        "app_identity_context.refref",
		"tenant-entity-registry-svc":  "app_tenant_entity_registry.refref",
		"governance-decision-log-svc": "app_governance_decision_log.refref",
		// The one exception: schema authorization_svc, role app_authorization.
		"authorization-svc": "app_authorization.refref",
	}
	seen := map[string]string{}
	for name, wantUser := range want {
		svc, ok := reg.Get(name)
		if !ok {
			t.Fatalf("no such service %q", name)
		}
		got := g.ServiceEnv(svc)["DB_USER"]
		if got != wantUser {
			t.Errorf("%s DB_USER = %q, want %q", name, got, wantUser)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s share the login role %q", prev, name, got)
		}
		seen[got] = name
	}
}

// The convention is app_<schema>; create-app-roles.sh uses it for all 21 of its
// entries, and deployments/supabase records the single exception.
func TestDBRoleConvention(t *testing.T) {
	for schema, want := range map[string]string{
		"general_ledger":    "app_general_ledger",
		"identity_context":  "app_identity_context",
		"authorization_svc": "app_authorization",
		"":                  "",
	} {
		if got := dbRoleFor(schema); got != want {
			t.Errorf("dbRoleFor(%q) = %q, want %q", schema, got, want)
		}
	}
}

// Handing the services an empty OTEL endpoint does not disable tracing: 41 of
// them declare env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318")
// and their env() helper treats empty as unset, so "" restores the Docker DNS
// name and every service logs "lookup otel-collector: no such host" every five
// seconds on a machine with no Docker.
func TestOTELEndpointNeverEmptyAndNeverDockerDNS(t *testing.T) {
	p := writeEnvFile(t, ".env", "DB_HOST=127.0.0.1\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	got := g.ServiceEnv(&Service{Name: "gl", Port: 8098})["OTEL_EXPORTER_OTLP_ENDPOINT"]
	if got == "" {
		t.Error("empty endpoint restores the services own Docker default")
	}
	if strings.Contains(got, "otel-collector") {
		t.Errorf("endpoint %q names a Docker host that has no DNS without Docker", got)
	}
	if !strings.Contains(got, "127.0.0.1") {
		t.Errorf("endpoint = %q, want loopback", got)
	}
}

// An explicit global value must still win, so a real collector can be used.
func TestOTELEndpointHonoursGlobalOverride(t *testing.T) {
	p := writeEnvFile(t, ".env", "OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.internal:4318\n")
	g, err := LoadGlobalEnv(p)
	if err != nil {
		t.Fatal(err)
	}
	got := g.ServiceEnv(&Service{Name: "gl", Port: 8098})["OTEL_EXPORTER_OTLP_ENDPOINT"]
	if got != "http://collector.internal:4318" {
		t.Errorf("endpoint = %q, want the global override", got)
	}
}
