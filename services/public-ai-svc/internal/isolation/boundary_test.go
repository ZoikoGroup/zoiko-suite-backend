package isolation

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"zoiko.io/public-ai-svc/internal/config"
)

// Master prompt T12 (TB-X): the public runtime has no credential, endpoint
// or env route to any private ZoikoSuite system. Its config can name only its
// own database and event brokers; nothing it reads from the environment, and
// nothing in its deployment env files, points at another service.
func TestT12_NoRouteToPrivateSystems(t *testing.T) {
	allowedEndpointFields := map[string]bool{"DatabaseURL": true, "KafkaBrokers": true}
	endpointish := regexp.MustCompile(`(?i)(url|uri|endpoint|host|addr|broker|token|secret|key|password|credential)`)
	allowedOther := map[string]bool{
		"HTTPAddr": true, "AllowedOrigins": true, "AllowedHosts": true, // its own listener and allowlists
		"SigningPublicKey": true, "SigningKey": true, // release signatures, not access to other systems
		"LogRawPrompt": true, // a refused switch, not a route
		// Model token budgets, not access tokens.
		"TokenBudgetPerTurn": true, "DailyTokenCeiling": true, "MonthlyTokenCeiling": true,
		// The approved model provider's key: egress to Groq only, not a private ZoikoSuite system.
		"GroqAPIKey": true,
	}
	ct := reflect.TypeOf(config.Config{})
	for i := 0; i < ct.NumField(); i++ {
		name := ct.Field(i).Name
		if endpointish.MatchString(name) && !allowedEndpointFields[name] && !allowedOther[name] {
			t.Errorf("config field %s looks like a route or credential to another system", name)
		}
	}

	// Every env var the service reads is PUBLIC_AI_* (or the dev alias
	// MODEL_PROVIDER): it cannot pick up another service's URL or token.
	root, _ := filepath.Abs("../..")
	// The standard OTel exporter settings are the one other exception: they
	// name the trace collector (telemetry egress carrying allowlisted ids and
	// counts only, see internal/telemetry), not a private ZoikoSuite system.
	otelEnv := map[string]bool{"OTEL_EXPORTER_OTLP_ENDPOINT": true, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": true, "OTEL_SERVICE_NAME": true}
	envRead := regexp.MustCompile(`(?:env|envBool|envInt|envDuration|os\.Getenv)\("([A-Z0-9_]+)"`)
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range envRead.FindAllStringSubmatch(string(b), -1) {
			if v := m[1]; !strings.HasPrefix(v, "PUBLIC_AI_") && v != "MODEL_PROVIDER" && v != "KB_ALLOW_DRAFT" && v != "GROQ_API_KEY" && v != "GROQ_MODEL" && v != "TEST_DATABASE_URL" && !otelEnv[v] {
				t.Errorf("%s reads env %s outside the PUBLIC_AI_ namespace", filepath.Base(p), v)
			}
		}
		return nil
	})

	// The isolated compose stack gives the service no other service's address.
	compose, err := os.ReadFile(filepath.Join(root, "..", "..", "deployments", "docker-compose.public-ai.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?m)^\s+[A-Z_]*(SERVICE_URL|_SVC_URL)[A-Z_]*:`).Find(compose); m != nil {
		t.Fatalf("public-ai compose env names another service: %s", m)
	}
}
