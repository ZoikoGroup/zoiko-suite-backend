package handler_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"zoiko.io/gateway-auth-svc/internal/jwks"
	"zoiko.io/gateway-auth-svc/internal/router"
)

func loadOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	require.NoError(t, err, "openapi.yaml must be present")
	doc, err := openapi3.NewLoader().LoadFromData(raw)
	require.NoError(t, err, "openapi.yaml must load")
	return doc
}

// The 27 Oct review found openapi.yaml did not parse at all (a duplicated
// `schema` key under the 503 response) while API-2 was recorded as done for
// this service: nothing here ever loaded it. kin-openapi is the gate.
func TestOpenAPIDocumentIsValid(t *testing.T) {
	doc := loadOpenAPI(t)
	require.NoError(t, doc.Validate(context.Background()), "openapi.yaml must be a valid OpenAPI document")
}

// Every path the production router serves is published, and every published
// path is served.
func TestOpenAPI_MatchesTheRouter(t *testing.T) {
	doc := loadOpenAPI(t)
	h, _, cfg := newTestEnv(t)
	r := router.New(h, jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL), nil)

	served := map[string]bool{}
	require.NoError(t, chi.Walk(r, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[route] = true
		return nil
	}))
	// router.New mounts /metrics only with a metrics registry, and a real one
	// cannot be registered twice in one test binary (see serveThroughRouter).
	served["/metrics"] = true
	declared := map[string]bool{}
	for p := range doc.Paths.Map() {
		declared[p] = true
	}

	var missing, stale []string
	for p := range served {
		if !declared[p] {
			missing = append(missing, p)
		}
	}
	for p := range declared {
		if !served[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	require.Empty(t, missing, "served but not in openapi.yaml")
	require.Empty(t, stale, "in openapi.yaml but not served")
}

// Every 4xx/5xx the contract publishes is RFC 9457 problem+json (ZS-API-001
// §12). /readyz is the exception: its 503 is the probe body the orchestrator
// reads, not an API error.
func TestOpenAPI_ErrorResponsesAreProblemDetails(t *testing.T) {
	doc := loadOpenAPI(t)
	for path, item := range doc.Paths.Map() {
		if path == "/readyz" || path == "/healthz" {
			continue
		}
		for method, op := range item.Operations() {
			for code, resp := range op.Responses.Map() {
				if code[0] != '4' && code[0] != '5' {
					continue
				}
				mt := resp.Value.Content.Get("application/problem+json")
				require.NotNil(t, mt, "%s %s %s must be application/problem+json", method, path, code)
				require.Equal(t, "#/components/schemas/Problem", mt.Schema.Ref, "%s %s %s", method, path, code)
			}
		}
	}
}
