package handler

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
)

func loadOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	require.NoError(t, err, "openapi.yaml must be present")
	doc, err := openapi3.NewLoader().LoadFromData(raw)
	require.NoError(t, err, "openapi.yaml must load")
	return doc
}

// The 27 Oct review found API-2 recorded an "openapi gate" for this service
// with no test that ever loaded the document. kin-openapi is the gate.
func TestOpenAPIDocumentIsValid(t *testing.T) {
	require.NoError(t, loadOpenAPI(t).Validate(context.Background()), "openapi.yaml must be a valid OpenAPI document")
}

// Every route Handler.Routes serves is published and every published path is
// served. The probes and /metrics are mounted by main.go, outside Routes.
func TestOpenAPI_MatchesTheRouter(t *testing.T) {
	doc := loadOpenAPI(t)
	served := map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}
	require.NoError(t, chi.Walk(newHarness(t, nil).router, func(_ string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[route] = true
		return nil
	}))
	declared := map[string]bool{}
	for p := range doc.Paths.Map() {
		declared[p] = true
	}
	var missing, stale []string
	for p := range served {
		if !declared[p] && p != "/metrics" && p != "/healthz" {
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

// Every published 4xx/5xx is RFC 9457 problem+json (ZS-API-001 §12), the
// readiness probe's body aside.
func TestOpenAPI_ErrorResponsesAreProblemDetails(t *testing.T) {
	for path, item := range loadOpenAPI(t).Paths.Map() {
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
