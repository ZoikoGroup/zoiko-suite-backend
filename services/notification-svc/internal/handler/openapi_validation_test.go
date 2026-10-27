package handler_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
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

// The 27 Oct review found the document failed validation: 13 webhook
// operations had no `id` path parameter, and three flow-style summaries were
// split at an unquoted comma into stray keys. API-2 had recorded an "openapi
// gate" here with no test that ever loaded it. kin-openapi is the gate.
func TestOpenAPIDocumentIsValid(t *testing.T) {
	require.NoError(t, loadOpenAPI(t).Validate(context.Background()), "openapi.yaml must be a valid OpenAPI document")
}

// Every published 4xx/5xx is RFC 9457 problem+json (ZS-API-001 §12), the
// probe bodies aside.
func TestOpenAPI_ErrorResponsesAreProblemDetails(t *testing.T) {
	for path, item := range loadOpenAPI(t).Paths.Map() {
		if path == "/readyz" || path == "/healthz" || path == "/health" {
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
