package handler_test

import (
	"context"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/schema-registry-svc/internal/handler"
)

// openapi.yaml is checked with a real OpenAPI implementation, not only for
// route parity: a parity test passes for a document no client generator can
// read. Sandbox: no request is served — only the router's registrations are
// set side by side with the document (same pattern as authorization-svc and
// governance-decision-log-svc).
//
// This test exists because the two drifted apart unobserved: the document
// claimed compatibility analysis was top-level-only long after the checker had
// learned to recurse, and did not mention the 401 every read can answer.
func loadSchemaRegistrySpec(t *testing.T) *openapi3.T {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join(filepath.Dir(thisFile), "../../openapi.yaml"))
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

// Every route the router serves is documented, and nothing documented is
// unserved. chi registers GET /v1/schemas as "/v1/schemas/"; the document
// spells it without the trailing slash, so one is trimmed before comparing.
func TestOpenAPI_MatchesTheRouter(t *testing.T) {
	doc := loadSchemaRegistrySpec(t)

	// Health probes are mounted by cmd/server, not by RegisterRoutes, and are
	// covered by internal/health's own tests; parity here is for the API.
	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		if path == "/healthz" || path == "/readyz" {
			continue
		}
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(&stubStore{}, &stubAuthz{}, "platform-scope-entity", zap.NewNop()))

	served := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})

	var missing, extra []string
	for k := range served {
		if !documented[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !served[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("openapi.yaml out of step with the router - undocumented: %v, unserved: %v", missing, extra)
	}
}
